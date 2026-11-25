package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/mysqldb"
	"github.com/fetburner/rokuban/internal/store"
)

// runTx は store の書き込みメソッドに 1 つの transaction を与える。
// TestRunTxRollsBackAfterStatementFailure は、文の失敗時に rollback してから返すことを検査する。
// デッドロックとロック待ちタイムアウトは自動再試行しない。任意のメソッドを再実行するには
// Go 側に副作用が無い必要があるが、共通ヘルパーからは各処理が条件を満たすか分からない。
func runTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning MySQL transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rolling back MySQL transaction: %w", rollbackErr))
		}
		return translateError(err)
	}
	if err := tx.Commit(); err != nil {
		return translateError(fmt.Errorf("committing MySQL transaction: %w", err))
	}
	return nil
}

func translateError(err error) error {
	if mysqldb.IsUniqueViolation(err) {
		return fmt.Errorf("%w: %w", store.ErrConflict, err)
	}
	return err
}

type rowQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryRows[T any](ctx context.Context, db rowQuerier, entity, query string, scan func(*sql.Rows) (T, error), args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying %s: %w", entity, err)
	}
	defer func() { _ = rows.Close() }()
	results := make([]T, 0)
	for rows.Next() {
		row, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning %s: %w", entity, err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", entity, err)
	}
	return results, nil
}

func transactionTime(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRowContext(ctx, "SELECT CURRENT_TIMESTAMP(6)").Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("reading transaction time: %w", err)
	}
	return now, nil
}

// runSnapshotTx は読み取り専用の REPEATABLE READ の tx で fn を実行する。
// InnoDB は tx 内で最初に読んだ時点のスナップショットを使うので、fn の全ての読み取りは同じ時点を見る。
// READ COMMITTED の tx は文ごとにスナップショットが変わるため、関係する表を横断して読む export では使えない。
// 読み取りだけなのでロックは取らない。
func runSnapshotTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("beginning MySQL snapshot transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing MySQL snapshot transaction: %w", err)
	}
	return nil
}
