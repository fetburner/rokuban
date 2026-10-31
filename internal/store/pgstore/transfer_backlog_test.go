package pgstore

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
)

var backlogBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// 保存と読み出し: 書いた置き場の前の状態が返り、counts に無い置き場の行は消える。
func TestRecordTransferBacklog_ReturnsPreviousAndDropsOthers(t *testing.T) {
	pool := testutil.SetupDB(t)
	m := NewMediaWithStores(pool, "", "", "")
	ctx := t.Context()

	w, err := m.RecordTransferBacklog(ctx, map[string]int64{"edge": 3, "gone": 1}, backlogBase)
	if err != nil || len(w) != 2 || w["edge"].HadPrevious || w["gone"].HadPrevious {
		t.Fatalf("first record = %v, %v; want two writes without previous", w, err)
	}
	w, err = m.RecordTransferBacklog(ctx, map[string]int64{"edge": 5}, backlogBase.Add(time.Second))
	if err != nil || len(w) != 1 || !w["edge"].HadPrevious || w["edge"].Previous != 3 {
		t.Fatalf("second record = %v, %v; want only edge with previous 3", w, err)
	}
	got, err := m.ReadTransferBacklog(ctx)
	if err != nil || len(got) != 1 || got["edge"].Assets != 5 || !got["edge"].CountedAt.Equal(backlogBase.Add(time.Second)) {
		t.Fatalf("read = %v, %v; want only edge 5 counted at base+1s", got, err)
	}
}

// 古い counted_at の保存は、新しい件数を上書きせず、書いたとも返さない。
func TestRecordTransferBacklog_StaleCountDoesNotOverwrite(t *testing.T) {
	pool := testutil.SetupDB(t)
	m := NewMediaWithStores(pool, "", "", "")
	ctx := t.Context()
	if _, err := m.RecordTransferBacklog(ctx, map[string]int64{"edge": 9}, backlogBase.Add(time.Minute)); err != nil {
		t.Fatalf("recording newer count: %v", err)
	}
	for name, at := range map[string]time.Time{"older": backlogBase, "same time": backlogBase.Add(time.Minute)} {
		w, err := m.RecordTransferBacklog(ctx, map[string]int64{"edge": 1}, at)
		if err != nil || len(w) != 0 {
			t.Fatalf("%s: write = %v, %v; want none", name, w, err)
		}
	}
	got, _ := m.ReadTransferBacklog(ctx)
	if got["edge"].Assets != 9 || !got["edge"].CountedAt.Equal(backlogBase.Add(time.Minute)) {
		t.Fatalf("saved = %v, want edge 9 counted at base+1m", got)
	}
}

// 並行する呼び出しのうち、「前の行が無い」を見るのは 1 つだけ。全員を同時に走らせ、何度か繰り返す。
// 時刻は呼び出しごとに別にし、古い時刻の呼び出しが捨てられても、書いた呼び出しの中で前の行が無いのは 1 つだけになる。
func TestRecordTransferBacklog_ConcurrentCallsSeeTheChangeOnce(t *testing.T) {
	pool := testutil.SetupDB(t)
	m := NewMediaWithStores(pool, "", "", "")
	for round := 0; round < 10; round++ {
		if _, err := m.RecordTransferBacklog(t.Context(), nil, backlogBase); err != nil {
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
				w, err := m.RecordTransferBacklog(t.Context(), map[string]int64{"edge": 7}, backlogBase.Add(time.Duration(i+1)*time.Second))
				if err != nil {
					t.Errorf("RecordTransferBacklog: %v", err)
					return
				}
				if write, ok := w["edge"]; ok && !write.HadPrevious {
					sawNoRow.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if got := sawNoRow.Load(); got != 1 {
			t.Fatalf("round %d: writes that saw no previous row = %d, want 1", round, got)
		}
	}
}
