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

// Tuners はチューナー同期・一覧・容量読み取りを PostgreSQL で実装する。
type Tuners struct {
	pool *pgxpool.Pool
}

// NewTuners は PostgreSQL を使う Tuners を作る。
func NewTuners(pool *pgxpool.Pool) *Tuners {
	return &Tuners{pool: pool}
}

// SweepMark は stale 判定の基準となる DB 時刻を返す。
func (s *Tuners) SweepMark(ctx context.Context) (time.Time, error) {
	mark, err := sqlcgen.New(s.pool).TunerSweepMark(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("getting tuner sweep mark: %w", err)
	}
	return mark, nil
}

// Upsert は site のチューナー一覧を 1 tx のバッチで反映する。
func (s *Tuners) Upsert(ctx context.Context, site string, tuners []store.TunerInput) error {
	if len(tuners) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning tuner upsert for site %s: %w", site, err)
	}
	defer rollback(tx)

	params := make([]sqlcgen.UpsertTunerSyncParams, 0, len(tuners))
	for _, tuner := range tuners {
		params = append(params, sqlcgen.UpsertTunerSyncParams{
			Site:        site,
			TunerIndex:  tuner.Index,
			Name:        tuner.Name,
			Types:       tuner.Types,
			IsAvailable: tuner.IsAvailable,
			IsFault:     tuner.IsFault,
		})
	}
	if err := execTunerBatch(sqlcgen.New(tx).UpsertTunerSync(ctx, params)); err != nil {
		return fmt.Errorf("upserting tuners for site %s: %w", site, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing tuner upsert for site %s: %w", site, err)
	}
	return nil
}

// DeleteStale は observedBefore より古い site の行を削除する。
func (s *Tuners) DeleteStale(ctx context.Context, site string, observedBefore time.Time) (int64, error) {
	deleted, err := sqlcgen.New(s.pool).DeleteStaleTunerSync(ctx, sqlcgen.DeleteStaleTunerSyncParams{
		Site:       site,
		ObservedAt: observedBefore,
	})
	if err != nil {
		return 0, fmt.Errorf("deleting stale tuners for site %s: %w", site, err)
	}
	return deleted, nil
}

// List は site の tuner_sync 行を index 順で返す。
func (s *Tuners) List(ctx context.Context, site string) ([]store.Tuner, error) {
	rows, err := sqlcgen.New(s.pool).ListTunerSync(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing tuner projection for site %s: %w", site, err)
	}
	return tunersFromRows(rows), nil
}

// ReadSite は site の需要とチューナーを同じ DB スナップショットから返す。
func (s *Tuners) ReadSite(ctx context.Context, site string) (store.CapacityInput, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return store.CapacityInput{}, fmt.Errorf("beginning capacity read for site %s: %w", site, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	demandRows, err := q.ListCapacityDemand(ctx, site)
	if err != nil {
		return store.CapacityInput{}, fmt.Errorf("listing capacity demand for site %s: %w", site, err)
	}
	tunerRows, err := q.ListTunerSync(ctx, site)
	if err != nil {
		return store.CapacityInput{}, fmt.Errorf("listing tuner projection for site %s: %w", site, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.CapacityInput{}, fmt.Errorf("committing capacity read for site %s: %w", site, err)
	}
	return store.CapacityInput{
		Demands: capacityDemandsFromSiteRows(demandRows),
		Tuners:  tunersFromRows(tunerRows),
	}, nil
}

// ReadAllSites は全 site の需要とチューナーを同じ DB スナップショットから返す。
func (s *Tuners) ReadAllSites(ctx context.Context) (store.CapacityInput, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return store.CapacityInput{}, fmt.Errorf("beginning all-site capacity read: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	demandRows, err := q.ListCapacityDemandAllSites(ctx)
	if err != nil {
		return store.CapacityInput{}, fmt.Errorf("listing capacity demand for all sites: %w", err)
	}
	tunerRows, err := q.ListTunerSyncAllSites(ctx)
	if err != nil {
		return store.CapacityInput{}, fmt.Errorf("listing tuner projection for all sites: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.CapacityInput{}, fmt.Errorf("committing all-site capacity read: %w", err)
	}
	return store.CapacityInput{
		Demands: capacityDemandsFromAllSiteRows(demandRows),
		Tuners:  tunersFromRows(tunerRows),
	}, nil
}

// PreviewCandidates は対象ルールの状態と、容量需要になりうる候補を返す。
func (s *Tuners) PreviewCandidates(ctx context.Context, refs []store.CapacityProgramRef, ruleID *int64) (store.CapacityPreview, error) {
	if len(refs) == 0 && ruleID == nil {
		return store.CapacityPreview{}, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return store.CapacityPreview{}, fmt.Errorf("beginning capacity preview read: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	var preview store.CapacityPreview
	if ruleID != nil {
		rule, err := q.GetRule(ctx, *ruleID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.CapacityPreview{}, fmt.Errorf("rule %d: %w", *ruleID, store.ErrNotFound)
			}
			return store.CapacityPreview{}, fmt.Errorf("loading rule %d for capacity preview: %w", *ruleID, err)
		}
		preview.Rule = &store.CapacityPreviewRule{Enabled: rule.Enabled, DedupeEnabled: rule.DedupeEnabled}
	}

	if len(refs) > 0 {
		sites := make([]string, len(refs))
		programIDs := make([]int64, len(refs))
		for i, ref := range refs {
			sites[i] = ref.Site
			programIDs[i] = ref.ProgramID
		}
		rows, err := q.ListCapacityPreviewCandidates(ctx, sqlcgen.ListCapacityPreviewCandidatesParams{
			Sites: sites, ProgramIds: programIDs, RuleID: ruleID,
		})
		if err != nil {
			return store.CapacityPreview{}, fmt.Errorf("listing capacity preview candidates: %w", err)
		}
		preview.Candidates = make([]store.CapacityPreviewCandidate, len(rows))
		for i, row := range rows {
			preview.Candidates[i] = store.CapacityPreviewCandidate{
				Site: row.Site, ProgramID: row.ProgramID, IntentAction: row.IntentAction,
				ChannelType: row.ChannelType, Channel: row.Channel,
				StartAt: row.StartAt, EndAt: row.EndAt,
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return store.CapacityPreview{}, fmt.Errorf("committing capacity preview read: %w", err)
	}
	return preview, nil
}

func tunersFromRows(rows []sqlcgen.TunerSync) []store.Tuner {
	tuners := make([]store.Tuner, 0, len(rows))
	for _, row := range rows {
		tuners = append(tuners, store.Tuner{
			Site: row.Site, Index: row.TunerIndex, Name: row.Name, Types: row.Types,
			IsAvailable: row.IsAvailable, IsFault: row.IsFault, ObservedAt: row.ObservedAt,
		})
	}
	return tuners
}

func capacityDemandsFromSiteRows(rows []sqlcgen.ListCapacityDemandRow) []store.CapacityDemand {
	demands := make([]store.CapacityDemand, 0, len(rows))
	for _, row := range rows {
		demands = append(demands, store.CapacityDemand{
			Site: row.Site, ChannelType: row.ChannelType, Channel: row.Channel,
			StartAt: row.ProgramStartAt, EndAt: row.ProgramEndAt,
			Base: row.Base, IntentAction: row.IntentAction, Overrides: row.Overrides,
		})
	}
	return demands
}

func capacityDemandsFromAllSiteRows(rows []sqlcgen.ListCapacityDemandAllSitesRow) []store.CapacityDemand {
	demands := make([]store.CapacityDemand, 0, len(rows))
	for _, row := range rows {
		demands = append(demands, store.CapacityDemand{
			Site: row.Site, RuleID: row.RuleID, ChannelType: row.ChannelType, Channel: row.Channel,
			StartAt: row.ProgramStartAt, EndAt: row.ProgramEndAt,
			Base: row.Base, IntentAction: row.IntentAction, Overrides: row.Overrides,
		})
	}
	return demands
}

type tunerBatchResults interface {
	Exec(func(int, error))
	Close() error
}

func execTunerBatch(results tunerBatchResults) error {
	var firstErr error
	results.Exec(func(index int, err error) {
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("batch item %d: %w", index, err)
		}
	})
	if err := results.Close(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("closing batch: %w", err)
	}
	return firstErr
}
