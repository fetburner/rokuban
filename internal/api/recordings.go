package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ListRecordings は録画履歴を新しい順に返す。
func (h *Server) ListRecordings(ctx context.Context, _ ListRecordingsRequestObject) (ListRecordingsResponseObject, error) {
	rows, err := sqlcgen.New(h.pool).ListRecordings(ctx, defaultSite)
	if err != nil {
		return nil, err
	}

	result := make([]Recording, 0, len(rows))
	for _, r := range rows {
		rec := Recording{
			Id:            r.ID,
			ReservationId: r.ReservationID,
			RuleId:        r.RuleID,
			Source:        RecordingSource(r.Source),
			ServiceName:   r.ServiceName,
			ChannelType:   RecordingChannelType(r.ChannelType),
			Channel:       r.Channel,
			NetworkId:     int(r.NetworkID),
			ServiceId:     int(r.ServiceID),
			EventId:       int(r.EventID),
			Title:         r.Title,
			Description:   r.Description,
			StartAt:       r.ProgramStartAt,
			DurationMs:    r.ProgramDurationMs,
			Status:        RecordingStatus(r.Status),
			StartedAt:     r.StartedAt,
			EndedAt:       r.EndedAt,
			SizeBytes:     r.OriginalSizeBytes,
			CreatedAt:     r.CreatedAt,
		}
		// ドロップ統計は ingest 済み（media_assets 行がある）録画にしか存在しない。
		// 未 ingest と「統計が全部 0」を区別できるよう、原本が無ければ省略する。
		if r.OriginalSizeBytes != nil {
			rec.DropSummary = &DropSummary{
				Packets:   r.DropPackets,
				Drops:     r.DropDrops,
				Errors:    r.DropErrors,
				Scrambled: r.DropScrambled,
			}
		}
		if len(r.QualityEvents) > 0 {
			var events []map[string]any
			if err := json.Unmarshal(r.QualityEvents, &events); err != nil {
				return nil, fmt.Errorf("decoding quality_events for recording %d: %w", r.ID, err)
			}
			if len(events) > 0 {
				rec.QualityEvents = &events
			}
		}
		result = append(result, rec)
	}
	return ListRecordings200JSONResponse(result), nil
}

// ListRecordingDropStats は録画の PID 別ドロップ統計を返す。
func (h *Server) ListRecordingDropStats(ctx context.Context, req ListRecordingDropStatsRequestObject) (ListRecordingDropStatsResponseObject, error) {
	rows, err := sqlcgen.New(h.pool).ListRecordingDropStats(ctx, req.Id)
	if err != nil {
		return nil, err
	}

	result := make([]DropStat, 0, len(rows))
	for _, d := range rows {
		result = append(result, DropStat{
			Pid:       int(d.Pid),
			Packets:   d.Packets,
			Drops:     d.Drops,
			Errors:    d.Errors,
			Scrambled: d.Scrambled,
		})
	}
	return ListRecordingDropStats200JSONResponse(result), nil
}
