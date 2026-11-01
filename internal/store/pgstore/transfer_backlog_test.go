package pgstore

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var backlogBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// 保存と読み出し: 書いた置き場の前の状態が返り、counts に無い置き場の行は消える。
func TestRecordTransferBacklog_ReturnsPreviousAndDropsOthers(t *testing.T) {
	pool := testutil.SetupDB(t)
	m := NewMediaWithStores(pool, "", "", "")
	ctx := t.Context()

	w, err := m.RecordTransferBacklog(ctx, map[string]int64{"edge": 3, "gone": 1}, backlogBase)
	if err != nil || len(w) != 2 || !w["edge"].Applied || w["edge"].HadPrevious || w["edge"].Current != 3 || w["gone"].HadPrevious {
		t.Fatalf("first record = %v, %v; want two writes without previous", w, err)
	}
	w, err = m.RecordTransferBacklog(ctx, map[string]int64{"edge": 5}, backlogBase.Add(time.Second))
	if err != nil || len(w) != 1 || !w["edge"].Applied || !w["edge"].HadPrevious || w["edge"].Previous != 3 || w["edge"].Current != 5 {
		t.Fatalf("second record = %v, %v; want only edge with previous 3", w, err)
	}
	got, err := m.ReadTransferBacklog(ctx)
	if err != nil || len(got) != 1 || got["edge"].Assets != 5 || !got["edge"].CountedAt.Equal(backlogBase.Add(time.Second)) {
		t.Fatalf("read = %v, %v; want only edge 5 counted at base+1s", got, err)
	}
}

// 古い counted_at の保存は、新しい件数を上書きせず、書いていないと返し、DB に残っている新しい件数を返す。
func TestRecordTransferBacklog_StaleCountDoesNotOverwrite(t *testing.T) {
	pool := testutil.SetupDB(t)
	m := NewMediaWithStores(pool, "", "", "")
	ctx := t.Context()
	if _, err := m.RecordTransferBacklog(ctx, map[string]int64{"edge": 9}, backlogBase.Add(time.Minute)); err != nil {
		t.Fatalf("recording newer count: %v", err)
	}
	for name, at := range map[string]time.Time{"older": backlogBase, "same time": backlogBase.Add(time.Minute)} {
		w, err := m.RecordTransferBacklog(ctx, map[string]int64{"edge": 1}, at)
		if got := w["edge"]; err != nil || got.Applied || got.Current != 9 {
			t.Fatalf("%s: write = %v, %v; want not applied with current 9", name, w, err)
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
				if write := w["edge"]; write.Applied && !write.HadPrevious {
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

func TestCountMediaAssetTransferBacklogBySourceStoreMatchesCandidates(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMediaWithStores(pool, "test-store", "edge", "derived")
	ctx := t.Context()
	rules := []byte(`[{"match":{"site":"tiered"},"store":"archive"},{"match":{"site":"remote"},"store":"remote"}]`)
	fsStores := []string{"edge", "backup", "archive"}
	sourceStores := []string{"edge", "backup"}
	const attemptLimit int32 = 10

	assetWithSource := func(site, sourceStore string) (int64, int64) {
		recordingID := dbtest.Recording(t, pool, func(params *dbtest.RecordingParams) {
			params.Site = site
		})
		assetID := dbtest.MediaAsset(t, pool, recordingID)
		_, err := pool.Exec(ctx, `
UPDATE media_asset_copies
SET store = $2,
    rel_path = 'backlog-test/' || $1::text || '/' || $2 || '.ts'
WHERE media_asset_id = $1 AND state = 'active'`, assetID, sourceStore)
		if err != nil {
			t.Fatalf("moving source copy for asset %d to %q: %v", assetID, sourceStore, err)
		}
		var copyID int64
		if err := pool.QueryRow(ctx, `
SELECT id FROM media_asset_copies
WHERE media_asset_id = $1 AND store = $2 AND state = 'active'`, assetID, sourceStore).Scan(&copyID); err != nil {
			t.Fatalf("reading source copy for asset %d in %q: %v", assetID, sourceStore, err)
		}
		return assetID, copyID
	}

	addCopy := func(assetID int64, store, state string) int64 {
		var copyID int64
		if err := pool.QueryRow(ctx, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES ($1, $2, $3, 0, $4)
RETURNING id`, assetID, store, fmt.Sprintf("backlog-test/%d/%s.ts", assetID, store), state).Scan(&copyID); err != nil {
			t.Fatalf("adding %q copy for asset %d: %v", store, assetID, err)
		}
		return copyID
	}

	assetWithSource("tiered", "edge")
	assetWithSource("tiered", "backup")
	assetWithSource("recent", "edge")

	destinationPresent, _ := assetWithSource("tiered", "edge")
	addCopy(destinationPresent, "archive", "active")

	missingSource, missingCopyID := assetWithSource("tiered", "edge")
	if _, err := pool.Exec(ctx, `INSERT INTO missing_media_assets (media_asset_copy_id) VALUES ($1)`, missingCopyID); err != nil {
		t.Fatalf("marking source copy for asset %d missing: %v", missingSource, err)
	}

	exhaustedAsset, _ := assetWithSource("tiered", "edge")
	exhaustedCopyID := addCopy(exhaustedAsset, "archive", "pending")
	if _, err := pool.Exec(ctx, `
INSERT INTO media_asset_transfer_attempts (copy_id, attempt_count, last_error)
VALUES ($1, $2, 'exhausted')`, exhaustedCopyID, attemptLimit); err != nil {
		t.Fatalf("marking destination copy for asset %d exhausted: %v", exhaustedAsset, err)
	}

	assetWithSource("remote", "edge")
	deletedAsset, _ := assetWithSource("tiered", "edge")
	if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() WHERE id = (SELECT recording_id FROM media_assets WHERE id = $1)`, deletedAsset); err != nil {
		t.Fatalf("deleting recording for asset %d: %v", deletedAsset, err)
	}

	candidateCount, _, err := media.CountMediaAssetTransferCandidates(ctx, rules, fsStores, "derived", attemptLimit)
	if err != nil {
		t.Fatalf("counting transfer candidates: %v", err)
	}
	byStore, err := media.CountMediaAssetTransferBacklogBySourceStore(ctx, rules, fsStores, sourceStores, "derived", attemptLimit)
	if err != nil {
		t.Fatalf("counting backlog by source store: %v", err)
	}
	var backlogCount int64
	for _, count := range byStore {
		backlogCount += count
	}
	if candidateCount != 2 || backlogCount != candidateCount || byStore["edge"] != 1 || byStore["backup"] != 1 {
		t.Fatalf("candidate count = %d, backlog by source store = %v (sum %d); want 2 candidates split edge=1 and backup=1", candidateCount, byStore, backlogCount)
	}
}

// advisory lock のキーは固定の値にする。値は fnv64a("rokuban:transfer-backlog-snapshots") で、
// 既存の watcher のリーダー選出（"rokuban:watcher:default" で 112608722860763934）や
// ラベル規則の reconcile の lock（hashtextextended で別の値）と一致しない。
func TestTransferBacklogLockKey_IsFixed(t *testing.T) {
	if transferBacklogLockKey != -5313926783724820846 {
		t.Fatalf("transferBacklogLockKey = %d, want -5313926783724820846", transferBacklogLockKey)
	}
}
