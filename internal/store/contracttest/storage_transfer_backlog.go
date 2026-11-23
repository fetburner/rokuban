package contracttest

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// 保存と読み出し: 書いた置き場の前の状態が返り、counts に無い置き場の行は消える。
func runRecordTransferBacklog_ReturnsPreviousAndDropsOthers(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	backlogBase := transferDatabaseClock(t, media)
	ctx := t.Context()

	w, err := media.RecordTransferBacklog(ctx, map[string]int64{"edge": 3, "gone": 1}, backlogBase)
	if err != nil || len(w) != 2 || !w["edge"].Applied || w["edge"].HadPrevious || w["edge"].Current != 3 || w["gone"].HadPrevious {
		t.Fatalf("first record = %v, %v; want two writes without previous", w, err)
	}
	w, err = media.RecordTransferBacklog(ctx, map[string]int64{"edge": 5}, backlogBase.Add(time.Second))
	if err != nil || len(w) != 1 || !w["edge"].Applied || !w["edge"].HadPrevious || w["edge"].Previous != 3 || w["edge"].Current != 5 {
		t.Fatalf("second record = %v, %v; want only edge with previous 3", w, err)
	}
	got, err := media.ReadTransferBacklog(ctx)
	if err != nil || len(got) != 1 || got["edge"].Assets != 5 || !got["edge"].CountedAt.Equal(backlogBase.Add(time.Second)) {
		t.Fatalf("read = %v, %v; want only edge 5 counted at base+1s", got, err)
	}
}

// 古い counted_at の保存は、新しい件数を上書きせず、書いていないと返し、DB に残っている新しい件数を返す。
func runRecordTransferBacklog_StaleCountDoesNotOverwrite(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	backlogBase := transferDatabaseClock(t, media)
	ctx := t.Context()
	if _, err := media.RecordTransferBacklog(ctx, map[string]int64{"edge": 9}, backlogBase.Add(time.Minute)); err != nil {
		t.Fatalf("recording newer count: %v", err)
	}
	for name, at := range map[string]time.Time{"older": backlogBase, "same time": backlogBase.Add(time.Minute)} {
		w, err := media.RecordTransferBacklog(ctx, map[string]int64{"edge": 1}, at)
		if got := w["edge"]; err != nil || got.Applied || got.Current != 9 {
			t.Fatalf("%s: write = %v, %v; want not applied with current 9", name, w, err)
		}
	}
	got, err := media.ReadTransferBacklog(ctx)
	if err != nil || len(got) != 1 || got["edge"].Assets != 9 || !got["edge"].CountedAt.Equal(backlogBase.Add(time.Minute)) {
		t.Fatalf("saved = %v, %v; want edge 9 counted at base+1m", got, err)
	}
}

// 並行する呼び出しのうち、「前の行が無い」を見るのは 1 つだけ。全員を同時に走らせ、何度か繰り返す。
// 時刻は呼び出しごとに別にし、古い時刻の呼び出しが捨てられても、書いた呼び出しの中で前の行が無いのは 1 つだけになる。
func runRecordTransferBacklog_ConcurrentCallsSeeTheChangeOnce(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	media := f.Store
	backlogBase := transferDatabaseClock(t, media)
	for round := 0; round < 10; round++ {
		if _, err := media.RecordTransferBacklog(t.Context(), nil, backlogBase); err != nil {
			t.Fatalf("clearing snapshots: %v", err)
		}
		// 最初の回は 2 つの transaction が重なったことを確認してから書き込みを許す。
		var endHold func()
		if round == 0 {
			endHold = f.BeginBacklogWriteHold(t)
		}
		var sawNoRow atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				w, err := media.RecordTransferBacklog(t.Context(), map[string]int64{"edge": 7}, backlogBase.Add(time.Duration(i+1)*time.Second))
				if err != nil {
					t.Errorf("RecordTransferBacklog: %v", err)
					return
				}
				write := w["edge"]
				if len(w) != 1 || write.Current != 7 || (write.Applied && write.HadPrevious && write.Previous != 7) {
					t.Errorf("RecordTransferBacklog = %+v, want current 7 and previous 7 if replaced", w)
				}
				if write.Applied && !write.HadPrevious {
					sawNoRow.Add(1)
				}
			}()
		}
		close(start)
		if endHold != nil {
			f.WaitForBlockedTransfers(t, 2, nil)
			endHold()
		}
		wg.Wait()
		if got := sawNoRow.Load(); got != 1 {
			t.Fatalf("round %d: writes that saw no previous row = %d, want 1", round, got)
		}
	}
}
func transferDatabaseClock(t *testing.T, media store.StorageTransfer) time.Time {
	t.Helper()
	now, err := media.DatabaseClock(t.Context())
	if err != nil || now.IsZero() {
		t.Fatalf("DatabaseClock = (%v, %v), want a DB timestamp", now, err)
	}
	return now
}
