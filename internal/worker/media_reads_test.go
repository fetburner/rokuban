package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
)

// planningSpy はエンコード前の判定が読む状態だけを差し替える。
type planningSpy struct {
	store.MediaReconcile

	result            store.EncodedPublishState
	calls             int
	cut               bool
	includeCurrentCut bool
}

func (s *planningSpy) GetEncodedPublishState(
	_ context.Context,
	_ int64,
	_ string,
	cut bool,
	includeCurrentCut bool,
) (store.EncodedPublishState, error) {
	s.calls++
	s.cut = cut
	s.includeCurrentCut = includeCurrentCut
	return s.result, nil
}

// TestEncodeWorkerPreflightUsesSnapshotAndLogsReasons は、エンコード前の判定が 1 回の
// 読み取りで得た状態を純関数に渡し、飛ばす理由を運用ログへ出すことを固定する。
func TestEncodeWorkerPreflightUsesSnapshotAndLogsReasons(t *testing.T) {
	keepA := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	keepB := []chapters.Range{{StartMs: 0, EndMs: 2000}}
	requestedKeep, err := json.Marshal(keepA)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name           string
		profile        config.EncodeProfile
		requested      json.RawMessage
		state          store.EncodedPublishState
		wantReason     store.EncodedPublishSkipReason
		wantCut        bool
		wantCurrentCut bool
	}{
		{
			name:       "missing rebuild request",
			profile:    config.EncodeProfile{Name: "cut", Cut: true},
			requested:  requestedKeep,
			state:      store.EncodedPublishState{Desired: true},
			wantReason: store.EncodedPublishSkipRebuildRequestMissing,
			wantCut:    true,
		},
		{
			name:      "overwritten rebuild request",
			profile:   config.EncodeProfile{Name: "cut", Cut: true},
			requested: requestedKeep,
			state: store.EncodedPublishState{
				Desired: true, HasRebuildRequest: true, RebuildRequestKeep: keepB,
			},
			wantReason: store.EncodedPublishSkipRebuildRequestOverwritten,
			wantCut:    true,
		},
		{
			name:       "desired profile excluded",
			profile:    config.EncodeProfile{Name: "h264"},
			state:      store.EncodedPublishState{},
			wantReason: store.EncodedPublishSkipDesiredProfileExcluded,
		},
		{
			name:       "encoded asset already satisfies request",
			profile:    config.EncodeProfile{Name: "h264"},
			state:      store.EncodedPublishState{Desired: true, EncodedAsset: &store.EncodedAssetObservation{RelPath: "encoded.mp4", State: "active"}},
			wantReason: store.EncodedPublishSkipAssetSatisfied,
		},
		{
			name:    "ordinary cut does not replace active cut",
			profile: config.EncodeProfile{Name: "cut", Cut: true},
			state: store.EncodedPublishState{
				Desired: true, ChapterOwned: true, CurrentCutKeep: keepA,
				EncodedAsset: &store.EncodedAssetObservation{RelPath: "cut.mp4", State: "active", KeepRanges: keepB},
			},
			wantReason:     store.EncodedPublishSkipActiveCutProtected,
			wantCut:        true,
			wantCurrentCut: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			media := &planningSpy{result: tt.state}
			w := &EncodeWorker{
				Media:    media,
				Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{tt.profile}},
			}
			var logOutput bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logOutput, nil))
			observed, _, _, skip, err := w.observeEncodePlan(context.Background(), jobs.EncodeJobArgs{
				RecordingID:      17,
				Profile:          tt.profile.Name,
				RequestedCutKeep: tt.requested,
			}, logger)
			if err != nil {
				t.Fatalf("observeEncodePlan() error = %v", err)
			}
			if !skip || !observed.skip {
				t.Fatalf("observeEncodePlan() skip = %v, plan = %+v; want skip", skip, observed)
			}
			if observed.skipReason != tt.wantReason {
				t.Errorf("skip reason = %q, want %q", observed.skipReason, tt.wantReason)
			}
			if !strings.Contains(logOutput.String(), string(tt.wantReason)) {
				t.Errorf("log %q does not include reason %q", logOutput.String(), tt.wantReason)
			}
			if media.calls != 1 {
				t.Errorf("GetEncodedPublishState calls = %d, want 1", media.calls)
			}
			if media.cut != tt.wantCut || media.includeCurrentCut != tt.wantCurrentCut {
				t.Errorf("state read flags = (%v, %v), want (%v, %v)", media.cut, media.includeCurrentCut, tt.wantCut, tt.wantCurrentCut)
			}
		})
	}
}
