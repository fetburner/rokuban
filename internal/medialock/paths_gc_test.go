package medialock

import (
	"context"
	"testing"
)

// LockPaths は GC を 1 回だけ回す。path ごとに回すと保持済み lock を毎回走査して O(N²) になる。
func TestLockPathsRunsGCOnce(t *testing.T) {
	calls := 0
	onGCMediaRelPathLockFiles = func() { calls++ }
	defer func() { onGCMediaRelPathLockFiles = func() {} }()
	release, err := LockPaths(context.Background(), t.TempDir(), []string{"a", "b", "c", "d", "e"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	if calls != 1 {
		t.Fatalf("GC calls = %d, want 1", calls)
	}
}
