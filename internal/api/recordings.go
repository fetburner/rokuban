package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// recordingListFields は ListRecordings / ListTrashRecordings が共有する射影。
// sqlc はクエリごとに別 struct を生成するので、ここで共通化してマッピングする。
type recordingListFields struct {
	ID                       int64
	ReservationID            *int64
	RuleID                   *int64
	Source                   string
	ServiceName              string
	ChannelType              string
	Channel                  string
	NetworkID                int32
	ServiceID                int32
	EventID                  int32
	Title                    string
	Description              *string
	ProgramStartAt           time.Time
	ProgramDurationMs        int64
	Status                   string
	StartedAt                *time.Time
	EndedAt                  *time.Time
	QualityEvents            json.RawMessage
	DeletedAt                *time.Time
	CreatedAt                time.Time
	OriginalSizeBytes        *int64
	DropPackets              int64
	DropDrops                int64
	DropErrors               int64
	DropScrambled            int64
	AvailableEncodedProfiles []string
}

// recordingFromListFields は一覧行を API の Recording に写す。
// includeDeletedAt が true のときだけ deletedAt を載せる（ごみ箱一覧向け）。
func recordingFromListFields(r recordingListFields, includeDeletedAt bool) (Recording, error) {
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
	if includeDeletedAt {
		rec.DeletedAt = r.DeletedAt
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
	// 再生可能な encoded 派生物（observed）。空なら省略（omitempty）。
	if len(r.AvailableEncodedProfiles) > 0 {
		profiles := append([]string(nil), r.AvailableEncodedProfiles...)
		rec.EncodedProfiles = &profiles
	}
	if len(r.QualityEvents) > 0 {
		var events []map[string]any
		if err := json.Unmarshal(r.QualityEvents, &events); err != nil {
			return Recording{}, fmt.Errorf("decoding quality_events for recording %d: %w", r.ID, err)
		}
		if len(events) > 0 {
			rec.QualityEvents = &events
		}
	}
	return rec, nil
}

// ListRecordings は録画履歴を新しい順に返す。
// trash=true のときごみ箱（deleted_at IS NOT NULL）を返す。
func (h *Server) ListRecordings(ctx context.Context, req ListRecordingsRequestObject) (ListRecordingsResponseObject, error) {
	trash := req.Params.Trash != nil && *req.Params.Trash
	q := sqlcgen.New(h.pool)

	var result []Recording
	if trash {
		rows, err := q.ListTrashRecordings(ctx, defaultSite)
		if err != nil {
			return nil, fmt.Errorf("listing trash recordings: %w", err)
		}
		result = make([]Recording, 0, len(rows))
		for _, r := range rows {
			rec, err := recordingFromListFields(recordingListFields{
				ID: r.ID, ReservationID: r.ReservationID, RuleID: r.RuleID,
				Source: r.Source, ServiceName: r.ServiceName, ChannelType: r.ChannelType,
				Channel: r.Channel, NetworkID: r.NetworkID, ServiceID: r.ServiceID,
				EventID: r.EventID, Title: r.Title, Description: r.Description,
				ProgramStartAt: r.ProgramStartAt, ProgramDurationMs: r.ProgramDurationMs,
				Status: r.Status, StartedAt: r.StartedAt, EndedAt: r.EndedAt,
				QualityEvents: r.QualityEvents, DeletedAt: r.DeletedAt, CreatedAt: r.CreatedAt,
				OriginalSizeBytes: r.OriginalSizeBytes,
				DropPackets:       r.DropPackets, DropDrops: r.DropDrops,
				DropErrors: r.DropErrors, DropScrambled: r.DropScrambled,
			}, true)
			if err != nil {
				return nil, err
			}
			result = append(result, rec)
		}
	} else {
		rows, err := q.ListRecordings(ctx, defaultSite)
		if err != nil {
			return nil, fmt.Errorf("listing recordings: %w", err)
		}
		result = make([]Recording, 0, len(rows))
		for _, r := range rows {
			rec, err := recordingFromListFields(recordingListFields{
				ID: r.ID, ReservationID: r.ReservationID, RuleID: r.RuleID,
				Source: r.Source, ServiceName: r.ServiceName, ChannelType: r.ChannelType,
				Channel: r.Channel, NetworkID: r.NetworkID, ServiceID: r.ServiceID,
				EventID: r.EventID, Title: r.Title, Description: r.Description,
				ProgramStartAt: r.ProgramStartAt, ProgramDurationMs: r.ProgramDurationMs,
				Status: r.Status, StartedAt: r.StartedAt, EndedAt: r.EndedAt,
				QualityEvents: r.QualityEvents, DeletedAt: r.DeletedAt, CreatedAt: r.CreatedAt,
				OriginalSizeBytes: r.OriginalSizeBytes,
				DropPackets:       r.DropPackets, DropDrops: r.DropDrops,
				DropErrors: r.DropErrors, DropScrambled: r.DropScrambled,
				AvailableEncodedProfiles: r.AvailableEncodedProfiles,
			}, false)
			if err != nil {
				return nil, err
			}
			result = append(result, rec)
		}
	}
	return ListRecordings200JSONResponse(result), nil
}

// DeleteRecording は録画を論理削除する（ごみ箱へ）。
// deleted_at を立てるだけでファイルには触れない。既に削除済みでも冪等に 204。
func (h *Server) DeleteRecording(ctx context.Context, req DeleteRecordingRequestObject) (DeleteRecordingResponseObject, error) {
	_, err := sqlcgen.New(h.pool).SoftDeleteRecording(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeleteRecording404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("soft-deleting recording %d: %w", req.Id, err)
	}
	return DeleteRecording204Response{}, nil
}

// RestoreRecording はごみ箱から録画を復元する。
// deleted_at と purge_after を消すだけ（ファイル操作ゼロ）。
// 同一イベントに生きている録画があると 409。
func (h *Server) RestoreRecording(ctx context.Context, req RestoreRecordingRequestObject) (RestoreRecordingResponseObject, error) {
	_, err := sqlcgen.New(h.pool).RestoreRecording(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RestoreRecording404JSONResponse{Error: "recording not in trash"}, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return RestoreRecording409JSONResponse{
				Error: "active recording already exists for the same event",
			}, nil
		}
		return nil, fmt.Errorf("restoring recording %d: %w", req.Id, err)
	}
	return RestoreRecording204Response{}, nil
}

// PurgeRecording は即時物理削除の要求印を立てる。
// purge_after = now() を書き、未 soft-delete なら deleted_at も立てる。
// ファイルは消さない（M3-8 の削除 reconcile が拾う）。
func (h *Server) PurgeRecording(ctx context.Context, req PurgeRecordingRequestObject) (PurgeRecordingResponseObject, error) {
	_, err := sqlcgen.New(h.pool).MarkRecordingPurgeAfter(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PurgeRecording404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("marking recording %d for purge: %w", req.Id, err)
	}
	return PurgeRecording204Response{}, nil
}

// ListRecordingDropStats は録画の PID 別ドロップ統計を返す。
func (h *Server) ListRecordingDropStats(ctx context.Context, req ListRecordingDropStatsRequestObject) (ListRecordingDropStatsResponseObject, error) {
	rows, err := sqlcgen.New(h.pool).ListRecordingDropStats(ctx, req.Id)
	if err != nil {
		return nil, err
	}

	result := make([]DropStat, 0, len(rows))
	for _, d := range rows {
		stat := DropStat{
			Pid:       int(d.Pid),
			Packets:   d.Packets,
			Drops:     d.Drops,
			Errors:    d.Errors,
			Scrambled: d.Scrambled,
		}
		// 分類できなかった PID では pidType を省略する（M2-13, issue #24）。
		if d.PidType != nil && *d.PidType != "" {
			stat.PidType = d.PidType
		}
		result = append(result, stat)
	}
	return ListRecordingDropStats200JSONResponse(result), nil
}
