package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestBreakersContract(t *testing.T) {
	contracttest.RunBreakers(t, func(t *testing.T) contracttest.BreakersFixture {
		s := NewBreakers(mysqltest.SetupDB(t))
		return contracttest.BreakersFixture{Breakers: s, Admin: s}
	})
}

// TestRunTxRollsBackAfterStatementFailure は rollback を外す変異を検出する。
// 別の tx の INSERT がロック待ちタイムアウトで失敗し、後始末では保持中の tx を rollback して DB 削除を待たせない。
func TestRunTxRollsBackAfterStatementFailure(t *testing.T) {
	db := mysqltest.SetupDB(t)
	db.SetMaxOpenConns(2)
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE tx_rollback_probe (id INT PRIMARY KEY) ENGINE=InnoDB`); err != nil {
		t.Fatalf("creating probe table: %v", err)
	}
	var conns []*sql.Conn
	t.Cleanup(func() {
		for _, conn := range conns {
			if err := conn.Close(); err != nil {
				t.Errorf("closing timeout-configured connection: %v", err)
			}
		}
	})
	for range 2 {
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatalf("checking out timeout-configured connection: %v", err)
		}
		conns = append(conns, conn)
		if _, err := conn.ExecContext(context.Background(), `SET SESSION innodb_lock_wait_timeout = 1`); err != nil {
			t.Fatalf("setting short InnoDB lock wait timeout: %v", err)
		}
	}
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			t.Fatalf("releasing timeout-configured connection: %v", err)
		}
	}
	conns = nil

	var failedTx *sql.Tx
	err := runTx(context.Background(), db, func(tx *sql.Tx) error {
		failedTx = tx
		if _, err := tx.ExecContext(context.Background(), `INSERT INTO tx_rollback_probe (id) VALUES (1)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(context.Background(), `INSERT INTO missing_tx_rollback_probe (id) VALUES (2)`)
		return err
	})
	if err == nil {
		t.Fatal("runTx error = nil, want failed statement error")
	}
	t.Cleanup(func() {
		if failedTx != nil {
			if err := failedTx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				t.Errorf("releasing transaction after rollback assertion: %v", err)
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `INSERT INTO tx_rollback_probe (id) VALUES (1)`); err != nil {
		t.Fatalf("second transaction could not insert after failed transaction; rollback did not release its lock: %v", err)
	}
}

func TestRunTxMapsUniqueViolationToConflict(t *testing.T) {
	db := mysqltest.SetupDB(t)
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE tx_unique_probe (id INT PRIMARY KEY)`); err != nil {
		t.Fatalf("creating unique probe table: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO tx_unique_probe (id) VALUES (1)`); err != nil {
		t.Fatalf("inserting initial probe row: %v", err)
	}
	err := runTx(context.Background(), db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO tx_unique_probe (id) VALUES (1)`)
		return err
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate insert error = %v, want store.ErrConflict", err)
	}
}
