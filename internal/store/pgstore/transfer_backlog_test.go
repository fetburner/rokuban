package pgstore

import "testing"

// advisory lock のキーは固定の値にする。値は fnv64a("rokuban:transfer-backlog-snapshots") で、
// 既存の watcher のリーダー選出（"rokuban:watcher:default" で 112608722860763934）や
// ラベル規則の reconcile の lock（hashtextextended で別の値）と一致しない。
func TestTransferBacklogLockKey_IsFixed(t *testing.T) {
	if transferBacklogLockKey != -5313926783724820846 {
		t.Fatalf("transferBacklogLockKey = %d, want -5313926783724820846", transferBacklogLockKey)
	}
}
