package pgstore

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fetburner/rokuban/internal/testutil"
)

// 保存と読み出し: 前の値が返り、counts に無い置き場の行は消える。
func TestRecordTransferBacklog_ReturnsPreviousAndDropsOthers(t *testing.T) {
	pool := testutil.SetupDB(t)
	m := NewMediaWithStores(pool, "", "", "")
	ctx := t.Context()

	prev, err := m.RecordTransferBacklog(ctx, map[string]int64{"edge": 3, "gone": 1})
	if err != nil || len(prev) != 0 {
		t.Fatalf("first record = %v, %v; want empty previous", prev, err)
	}
	prev, err = m.RecordTransferBacklog(ctx, map[string]int64{"edge": 5})
	if err != nil || len(prev) != 1 || prev["edge"] != 3 {
		t.Fatalf("second record previous = %v, %v; want only edge 3", prev, err)
	}
	got, err := m.ReadTransferBacklog(ctx)
	if err != nil || len(got) != 1 || got["edge"] != 5 {
		t.Fatalf("read = %v, %v; want only edge 5", got, err)
	}
}

// 並行する呼び出しのうち、「前の行が無い」を見るのは 1 つだけ。全員を同時に走らせ、何度か繰り返す。
func TestRecordTransferBacklog_ConcurrentCallsSeeTheChangeOnce(t *testing.T) {
	pool := testutil.SetupDB(t)
	m := NewMediaWithStores(pool, "", "", "")
	for round := 0; round < 10; round++ {
		if _, err := m.RecordTransferBacklog(t.Context(), nil); err != nil {
			t.Fatalf("clearing snapshots: %v", err)
		}
		var sawNoRow atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				prev, err := m.RecordTransferBacklog(t.Context(), map[string]int64{"edge": 7})
				if err != nil {
					t.Errorf("RecordTransferBacklog: %v", err)
					return
				}
				if _, ok := prev["edge"]; !ok {
					sawNoRow.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if got := sawNoRow.Load(); got != 1 {
			t.Fatalf("round %d: calls that saw no previous row = %d, want 1", round, got)
		}
	}
}
