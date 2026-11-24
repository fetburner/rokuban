package mysqlstore

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
)

func countNotifications(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notifications`).Scan(&n); err != nil {
		t.Fatalf("counting notifications: %v", err)
	}
	return n
}

func TestNotifyTopicWritesRow(t *testing.T) {
	db := mysqltest.SetupDB(t)
	if err := NotifyTopic(context.Background(), db, "recordings"); err != nil {
		t.Fatalf("NotifyTopic: %v", err)
	}
	var payload string
	if err := db.QueryRowContext(context.Background(), `SELECT payload FROM notifications`).Scan(&payload); err != nil {
		t.Fatalf("reading notification: %v", err)
	}
	if payload != "recordings" {
		t.Errorf("payload = %q, want recordings", payload)
	}
}

// TestNotifyTopicRollsBackWithTransaction は、通知がデータと同じ tx に入ることを検査する。
// tx の commit を外して auto-commit で書く変異は、rollback 後の行数で落ちる。
func TestNotifyTopicRollsBackWithTransaction(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	err := runTx(ctx, db, func(tx *sql.Tx) error {
		if err := NotifyTopic(ctx, tx, "recordings"); err != nil {
			return err
		}
		return context.Canceled // 強制 rollback
	})
	if err == nil {
		t.Fatal("runTx returned nil, want the forced error")
	}
	if n := countNotifications(t, db); n != 0 {
		t.Errorf("rows after rollback = %d, want 0", n)
	}
}

func TestNotifyTopicRejectsOversizedPayload(t *testing.T) {
	db := mysqltest.SetupDB(t)
	err := NotifyTopic(context.Background(), db, strings.Repeat("a", maxNotificationBytes+1))
	if err == nil {
		t.Fatal("NotifyTopic accepted a payload over the byte limit")
	}
	if n := countNotifications(t, db); n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
}

// TestCircuitBreakerTriggersNotify は、circuit_breakers の INSERT・UPDATE・DELETE それぞれが
// "breakers" を 1 行ずつ書くことを検査する。3 本のうち 1 本を外すと、対応する操作の行数で落ちる。
func TestCircuitBreakerTriggersNotify(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	breakers := NewBreakers(db)
	sample := store.BreakerSample{Total: 1}

	if _, err := breakers.Trip(ctx, "site", "a", 3, sample); err != nil {
		t.Fatalf("Trip insert: %v", err)
	}
	if n := countNotifications(t, db); n != 1 {
		t.Fatalf("rows after INSERT = %d, want 1", n)
	}
	if _, err := breakers.Trip(ctx, "site", "a", 3, store.BreakerSample{Total: 2}); err != nil {
		t.Fatalf("Trip update: %v", err)
	}
	if n := countNotifications(t, db); n != 2 {
		t.Fatalf("rows after UPDATE = %d, want 2", n)
	}
	if err := breakers.Resume(ctx, "site", "a"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if n := countNotifications(t, db); n != 3 {
		t.Fatalf("rows after DELETE = %d, want 3", n)
	}
	var topics int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE payload = 'breakers'`).Scan(&topics); err != nil {
		t.Fatalf("counting breakers topics: %v", err)
	}
	if topics != 3 {
		t.Errorf("breakers topics = %d, want 3", topics)
	}
}

// TestPruneNotificationsRemovesOnlyOldRows は、掃除が保持期間より古い行だけを消すことを検査する。
// 行数が保持期間ぶんで頭打ちになる性質は、古い行が消え新しい行が残ることで確かめる。
func TestPruneNotificationsRemovesOnlyOldRows(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	for range 3 {
		if err := NotifyTopic(ctx, db, "rules"); err != nil {
			t.Fatalf("NotifyTopic: %v", err)
		}
	}
	deleted, err := PruneNotifications(ctx, db, time.Hour)
	if err != nil {
		t.Fatalf("PruneNotifications(1h): %v", err)
	}
	if deleted != 0 || countNotifications(t, db) != 3 {
		t.Errorf("1h retention deleted %d, rows = %d; want 0 deleted and 3 rows", deleted, countNotifications(t, db))
	}
	// 古い行は保持期間より十分前、新しい行は保持期間より十分後に作る。境界の近くで検査すると揺れる。
	const retention = 250 * time.Millisecond
	time.Sleep(2 * retention)
	if err := NotifyTopic(ctx, db, "rules"); err != nil {
		t.Fatalf("NotifyTopic: %v", err)
	}
	deleted, err = PruneNotifications(ctx, db, retention)
	if err != nil {
		t.Fatalf("PruneNotifications(5ms): %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want the 3 old rows", deleted)
	}
	if n := countNotifications(t, db); n != 1 {
		t.Errorf("rows = %d, want only the newest row", n)
	}
}
