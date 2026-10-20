package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// EPG は番組表の同期・読み取り・検索を PostgreSQL で実装する。
type EPG struct {
	pool *pgxpool.Pool
}

// NewEPG は PostgreSQL を使う EPG store を作る。
func NewEPG(pool *pgxpool.Pool) *EPG {
	return &EPG{pool: pool}
}

// SweepMark は EPG 同期の基準となる DB 時刻を返す。
func (s *EPG) SweepMark(ctx context.Context) (time.Time, error) {
	mark, err := sqlcgen.New(s.pool).EpgSweepMark(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("getting EPG sweep mark: %w", err)
	}
	return mark, nil
}

// UpsertServices はサービス射影の 1 バッチを書き込む。
func (s *EPG) UpsertServices(ctx context.Context, services []store.EPGServiceUpsert) error {
	if len(services) == 0 {
		return nil
	}
	params := make([]sqlcgen.UpsertEpgServiceParams, 0, len(services))
	for _, service := range services {
		params = append(params, sqlcgen.UpsertEpgServiceParams{
			Site:               service.Site,
			NetworkID:          service.NetworkID,
			ServiceID:          service.ServiceID,
			Type:               service.Type,
			LogoID:             service.LogoID,
			RemoteControlKeyID: service.RemoteControlKeyID,
			Name:               service.Name,
			ChannelType:        service.ChannelType,
			Channel:            service.Channel,
			HasLogoData:        service.HasLogoData,
		})
	}
	if err := execMappedBatch(sqlcgen.New(s.pool).UpsertEpgService(ctx, params)); err != nil {
		return fmt.Errorf("upserting EPG services: %w", err)
	}
	return nil
}

// DeleteStaleServices は今回観測しなかったサービス射影を削除する。
func (s *EPG) DeleteStaleServices(ctx context.Context, site string, mark time.Time) (int64, error) {
	deleted, err := sqlcgen.New(s.pool).DeleteStaleEpgServices(ctx, sqlcgen.DeleteStaleEpgServicesParams{
		Site:       site,
		ObservedAt: mark,
	})
	if err != nil {
		return 0, fmt.Errorf("deleting stale EPG services for site %q: %w", site, err)
	}
	return deleted, nil
}

// UpsertPrograms は番組射影の 1 バッチを書き込む。
func (s *EPG) UpsertPrograms(ctx context.Context, programs []store.EPGProgramUpsert) error {
	if len(programs) == 0 {
		return nil
	}
	params := make([]sqlcgen.UpsertEpgProgramParams, 0, len(programs))
	for _, program := range programs {
		params = append(params, sqlcgen.UpsertEpgProgramParams{
			Site:        program.Site,
			ProgramID:   program.ProgramID,
			NetworkID:   program.NetworkID,
			ServiceID:   program.ServiceID,
			EventID:     program.EventID,
			StartAt:     program.StartAt,
			DurationMs:  program.DurationMs,
			EndAt:       program.EndAt,
			IsFree:      program.IsFree,
			Name:        program.Name,
			Description: program.Description,
			Extended:    program.Extended,
			Genres:      program.Genres,
			Video:       program.Video,
			Audios:      program.Audios,
		})
	}
	if err := execMappedBatch(sqlcgen.New(s.pool).UpsertEpgProgram(ctx, params)); err != nil {
		return fmt.Errorf("upserting EPG programs: %w", err)
	}
	return nil
}

// DeleteStalePrograms は観測済みチャンネル群の古い番組射影を削除する。
func (s *EPG) DeleteStalePrograms(ctx context.Context, site string, mark time.Time, networkID int32, serviceIDs []int32) (int64, error) {
	deleted, err := sqlcgen.New(s.pool).DeleteStaleEpgProgramsForServices(ctx, sqlcgen.DeleteStaleEpgProgramsForServicesParams{
		Site:       site,
		ObservedAt: mark,
		NetworkID:  networkID,
		ServiceIds: serviceIDs,
	})
	if err != nil {
		return 0, fmt.Errorf("deleting stale EPG programs for site %q network %d: %w", site, networkID, err)
	}
	return deleted, nil
}

// PrunePrograms は保持期間を過ぎた番組射影を削除する。
func (s *EPG) PrunePrograms(ctx context.Context, site string, endAt time.Time) (int64, error) {
	deleted, err := sqlcgen.New(s.pool).PruneEpgPrograms(ctx, sqlcgen.PruneEpgProgramsParams{
		Site:  site,
		EndAt: endAt,
	})
	if err != nil {
		return 0, fmt.Errorf("pruning aired EPG programs for site %q: %w", site, err)
	}
	return deleted, nil
}

// NotifyProgramsUpdated は番組表更新を購読者へ通知する。
func (s *EPG) NotifyProgramsUpdated(ctx context.Context) error {
	if err := sqlcgen.New(s.pool).NotifyTopic(ctx, "epg"); err != nil {
		return fmt.Errorf("notifying EPG programs updated: %w", err)
	}
	return nil
}

// ListServices は site のサービス射影を返す。
func (s *EPG) ListServices(ctx context.Context, site string) ([]store.EPGService, error) {
	rows, err := sqlcgen.New(s.pool).ListEpgServices(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing EPG services for site %q: %w", site, err)
	}
	services := make([]store.EPGService, len(rows))
	for i, row := range rows {
		services[i] = store.EPGService{
			Site:               row.Site,
			NetworkID:          row.NetworkID,
			ServiceID:          row.ServiceID,
			Name:               row.Name,
			ChannelType:        row.ChannelType,
			Channel:            row.Channel,
			RemoteControlKeyID: row.RemoteControlKeyID,
			HasLogoData:        row.HasLogoData,
			HasPrograms:        row.HasPrograms,
		}
	}
	return services, nil
}

// ListPrograms は時間窓と任意の NID/SID 集合で番組射影を絞る。
func (s *EPG) ListPrograms(ctx context.Context, query store.EPGProgramListQuery) ([]store.EPGProgramListItem, error) {
	networkIDs := make([]int32, len(query.Services))
	serviceIDs := make([]int32, len(query.Services))
	for i, service := range query.Services {
		networkIDs[i] = service.NetworkID
		serviceIDs[i] = service.ServiceID
	}
	rows, err := sqlcgen.New(s.pool).ListEpgProgramsForList(ctx, sqlcgen.ListEpgProgramsForListParams{
		Site:            query.Site,
		WindowStart:     query.WindowStart,
		WindowEnd:       query.WindowEnd,
		ExactNetworkIds: networkIDs,
		ExactServiceIds: serviceIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("listing EPG programs for site %q: %w", query.Site, err)
	}
	programs := make([]store.EPGProgramListItem, len(rows))
	for i, row := range rows {
		programs[i] = store.EPGProgramListItem{
			Site:         row.Site,
			ProgramID:    row.ProgramID,
			NetworkID:    row.NetworkID,
			ServiceID:    row.ServiceID,
			EventID:      row.EventID,
			StartAt:      row.StartAt,
			DurationMs:   row.DurationMs,
			EndAt:        row.EndAt,
			IsFree:       row.IsFree,
			Name:         row.Name,
			Description:  row.Description,
			GenreLv1:     row.GenreLv1,
			IntentAction: row.IntentAction,
			RecordingID:  row.RecordingID,
		}
	}
	return programs, nil
}

// GetProgram は site の programID の詳細射影を返す。
func (s *EPG) GetProgram(ctx context.Context, site string, programID int64) (store.EPGProgramDetail, error) {
	row, err := sqlcgen.New(s.pool).GetEpgProgram(ctx, sqlcgen.GetEpgProgramParams{Site: site, ProgramID: programID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.EPGProgramDetail{}, fmt.Errorf("program %d for site %q: %w", programID, site, store.ErrNotFound)
		}
		return store.EPGProgramDetail{}, fmt.Errorf("getting EPG program %d for site %q: %w", programID, site, err)
	}
	return store.EPGProgramDetail{
		Program:      epgProgram(row.EpgProgram),
		IntentAction: row.IntentAction,
	}, nil
}

// ListUpcomingProgramsBySeries は録画の実効シリーズに対応する今後の番組を返す。
func (s *EPG) ListUpcomingProgramsBySeries(ctx context.Context, recordingID int64) ([]store.ProgramMatch, error) {
	rows, err := sqlcgen.New(s.pool).ListUpcomingProgramsBySeries(ctx, recordingID)
	if err != nil {
		return nil, fmt.Errorf("listing upcoming programs for recording %d: %w", recordingID, err)
	}
	programs := make([]store.ProgramMatch, len(rows))
	for i, row := range rows {
		programs[i] = store.ProgramMatch{
			Site:       row.Site,
			ProgramID:  row.ProgramID,
			NetworkID:  row.NetworkID,
			ServiceID:  row.ServiceID,
			StartAt:    row.StartAt,
			DurationMs: row.DurationMs,
			Name:       row.Name,
			IsFree:     row.IsFree,
		}
	}
	return programs, nil
}

func epgProgram(program sqlcgen.EpgProgram) store.EPGProgram {
	return store.EPGProgram{
		Site:        program.Site,
		ProgramID:   program.ProgramID,
		NetworkID:   program.NetworkID,
		ServiceID:   program.ServiceID,
		EventID:     program.EventID,
		StartAt:     program.StartAt,
		DurationMs:  program.DurationMs,
		EndAt:       program.EndAt,
		IsFree:      program.IsFree,
		Name:        program.Name,
		Description: program.Description,
		Extended:    program.Extended,
		Genres:      program.Genres,
		Video:       program.Video,
		Audios:      program.Audios,
		GenreLv1:    program.GenreLv1,
	}
}

// execMappedBatch は既存の execBatch の結果に mapDatabaseError を通す。
// EPG の upsert が一意違反を起こしたとき store.ErrConflict を返す（バッチ内の失敗は
// execBatch が %w で包むので errors.As で PgError が取れる）。
func execMappedBatch(results batchResults) error {
	return mapDatabaseError(execBatch(results))
}

var _ store.EPGSync = (*EPG)(nil)
var _ store.EPGReader = (*EPG)(nil)
var _ store.ProgramSearch = (*EPG)(nil)
