package pgstore

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/testutil"
)

// TestTransferBandwidthLeaseHijacksOnUnlockFailure は、unlock に失敗した接続が pool に戻らないことを確かめる。
// 戻ると、その接続が lock を持ったまま他の処理に配られる。lock を持つ接続自身は同じ鍵を再取得できてしまうので、
// pool の外の別接続で pg_try_advisory_lock を試す。
func TestTransferBandwidthLeaseHijacksOnUnlockFailure(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "test-store", "")
	lease, acquired, err := media.AcquireTransferSourceBandwidth(context.Background(), "test-store")
	if err != nil || !acquired {
		t.Fatalf("AcquireTransferSourceBandwidth = (acquired=%t, %v), want acquired", acquired, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lease.Release(canceled); err == nil {
		t.Fatal("Release with a canceled context succeeded, want the unlock failure")
	}

	outside, err := pgx.ConnectConfig(context.Background(), pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connecting outside the pool: %v", err)
	}
	defer func() { _ = outside.Close(context.Background()) }()
	var got bool
	if err := outside.QueryRow(context.Background(), `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, "rokuban-storage-transfer:test-store").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("the transfer lock is still held after a failed unlock: the connection went back to the pool")
	}
}
