package api

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
)

// encodeQueueRow は待機列の 1 行（録画 × プロファイル）。running でなければ queued。
type encodeQueueRow struct {
	recordingID int64
	running     bool
}

// loadEncodeQueueRows は desired で未観測のエンコードを ListEncodeQueue で引く。
// 件数（GET /api/encode-queue）と録画一覧の encodeState 絞り込みは同じ行から作る
// （TestGetEncodeQueueSummaryFromDomainState / TestListRecordingsEncodeStateMatchesQueueCounts）。
func (h *Server) loadEncodeQueueRows(ctx context.Context) ([]encodeQueueRow, error) {
	// SQL の ANY は NULL 配列を空配列と異なる意味にするため、nil は空にする。
	rows, err := sqlcgen.New(h.pool).ListEncodeQueue(ctx, sqlcgen.ListEncodeQueueParams{
		KnownProfiles: append([]string{}, h.encodeProfileNames...),
		CutProfiles:   slices.AppendSeq([]string{}, maps.Keys(h.cutProfiles)),
		AttemptLimit:  jobs.EncodeAttemptLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing encode queue: %w", err)
	}
	out := make([]encodeQueueRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, encodeQueueRow{recordingID: r.RecordingID, running: r.Running})
	}
	return out, nil
}

// GetEncodeQueue は現在 desired で未観測のエンコードを、実行中の試行がある数と
// ない数に分けて返す。
func (h *Server) GetEncodeQueue(ctx context.Context, _ GetEncodeQueueRequestObject) (GetEncodeQueueResponseObject, error) {
	rows, err := h.loadEncodeQueueRows(ctx)
	if err != nil {
		return nil, err
	}
	var queued, running int64
	for _, r := range rows {
		if r.running {
			running++
		} else {
			queued++
		}
	}
	return GetEncodeQueue200JSONResponse{Queued: queued, Running: running}, nil
}

// encodeQueueRecordingIDs は待機列のうち running が一致する行の録画 ID を重複なしで返す。
func encodeQueueRecordingIDs(rows []encodeQueueRow, running bool) []int64 {
	ids := []int64{}
	for _, r := range rows {
		// 行は ORDER BY recording_id なので、重複は直前の ID との比較で落とせる。
		if r.running == running && (len(ids) == 0 || ids[len(ids)-1] != r.recordingID) {
			ids = append(ids, r.recordingID)
		}
	}
	return ids
}
