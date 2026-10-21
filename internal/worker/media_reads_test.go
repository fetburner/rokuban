package worker

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// planningSpy は GetEncodedAssetForPlan の戻り値だけを差し替える。
type planningSpy struct {
	store.MediaReconcile

	result store.EncodedAssetObservation
	calls  int
}

func (s *planningSpy) GetEncodedAssetForPlan(context.Context, int64, string) (store.EncodedAssetObservation, error) {
	s.calls++
	return s.result, nil
}

// TestEncodeWorkerPlansFromStoreObservation は tx 外の初回観測が store 契約の戻り値
// （凍結区間を含む）だけで決まることを固定する。公開 tx 内の照合は別に DB を読む。
// DB 側は keep と一致する区間を持つのに、store が違う区間を返したとき stale になるかを
// 見る。この「観測は store、公開 tx は DB」の分離は、呼び出しを差し替える spy でしか確かめられないので、
// 振る舞いの網羅は contracttest.RunMedia と既存の encode テストに任せ、これだけを残す。
func TestEncodeWorkerPlansFromStoreObservation(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "media-reads/plan.m2ts", nil, []byte("original"))
	profile := "cut"
	assetID := seedEncodedAsset(t, pool, recordingID, profile, "media-reads/plan-cut.mp4")
	keep := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	if err := setFrozenCuts(t, pool, assetID, keep); err != nil {
		t.Fatalf("seeding frozen keep ranges: %v", err)
	}
	media := &planningSpy{
		MediaReconcile: newTestMediaStore(pool),
		result: store.EncodedAssetObservation{
			ID: assetID, RelPath: "media-reads/plan-cut.mp4", State: "active",
			KeepRanges: []chapters.Range{{StartMs: 0, EndMs: 900}},
		},
	}
	w := &EncodeWorker{Media: media}
	observed, err := w.planEncodeAsset(context.Background(), recordingID, profile, &cutContext{keep: keep}, nil)
	if err != nil {
		t.Fatalf("planEncodeAsset() error = %v", err)
	}
	if observed.skip {
		t.Fatalf("planEncodeAsset() = %+v, want store-provided frozen ranges to keep asset stale", observed)
	}
	if media.calls != 1 {
		t.Fatalf("GetEncodedAssetForPlan calls = %d, want 1", media.calls)
	}
}
