package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

const labelRuleRecordingsTopic = "recordings"

// LabelRules はシリーズ分類ルールのドメインを PostgreSQL で実装する。
// pool を持ち、API のルール管理と再評価の interface を満たす。
type LabelRules struct {
	pool *pgxpool.Pool
}

// NewLabelRules は PostgreSQL を使う LabelRules を作る。
func NewLabelRules(pool *pgxpool.Pool) *LabelRules {
	return &LabelRules{pool: pool}
}

// List は勝者順で分類ルールを返す。
func (s *LabelRules) List(ctx context.Context) ([]store.LabelRule, error) {
	rows, err := sqlcgen.New(s.pool).ListLabelRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing label rules: %w", err)
	}
	out := make([]store.LabelRule, 0, len(rows))
	for _, row := range rows {
		out = append(out, labelRuleFromSQL(row))
	}
	return out, nil
}

// Get は id の分類ルールを返す。
func (s *LabelRules) Get(ctx context.Context, id int64) (store.LabelRule, error) {
	row, err := sqlcgen.New(s.pool).GetLabelRule(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.LabelRule{}, fmt.Errorf("label rule %d: %w", id, store.ErrNotFound)
		}
		return store.LabelRule{}, fmt.Errorf("getting label rule %d: %w", id, err)
	}
	return labelRuleFromSQL(row), nil
}

// Create は指定された内容の分類ルールを作成して返す。
func (s *LabelRules) Create(ctx context.Context, key, value, keyword string, priority int32) (store.LabelRule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.LabelRule{}, fmt.Errorf("beginning label rule creation: %w", err)
	}
	defer rollback(tx)

	row, err := sqlcgen.New(tx).CreateLabelRule(ctx, sqlcgen.CreateLabelRuleParams{
		Key: key, Value: value, Keyword: keyword, Priority: priority,
	})
	if err != nil {
		return store.LabelRule{}, fmt.Errorf("creating label rule: %w", mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return store.LabelRule{}, fmt.Errorf("committing label rule creation: %w", mapDatabaseError(err))
	}
	return labelRuleFromSQL(row), nil
}

// Update は id の分類ルールを置き換えて返す。
func (s *LabelRules) Update(ctx context.Context, id int64, value, keyword string, priority int32) (store.LabelRule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.LabelRule{}, fmt.Errorf("beginning label rule update: %w", err)
	}
	defer rollback(tx)

	row, err := sqlcgen.New(tx).UpdateLabelRule(ctx, sqlcgen.UpdateLabelRuleParams{
		ID: id, Value: value, Keyword: keyword, Priority: priority,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.LabelRule{}, fmt.Errorf("label rule %d: %w", id, store.ErrNotFound)
		}
		return store.LabelRule{}, fmt.Errorf("updating label rule %d: %w", id, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return store.LabelRule{}, fmt.Errorf("committing label rule update %d: %w", id, mapDatabaseError(err))
	}
	return labelRuleFromSQL(row), nil
}

// Delete は id の分類ルールを削除する。
func (s *LabelRules) Delete(ctx context.Context, id int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning label rule deletion: %w", err)
	}
	defer rollback(tx)

	deleted, err := sqlcgen.New(tx).DeleteLabelRule(ctx, id)
	if err != nil {
		return fmt.Errorf("deleting label rule %d: %w", id, mapDatabaseError(err))
	}
	if deleted == 0 {
		return fmt.Errorf("label rule %d: %w", id, store.ErrNotFound)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing label rule deletion %d: %w", id, mapDatabaseError(err))
	}
	return nil
}

// SeriesKey は DB の series_key 関数で value を正規化する。
func (s *LabelRules) SeriesKey(ctx context.Context, value string) (*string, error) {
	var key *string
	if err := s.pool.QueryRow(ctx, "SELECT public.series_key($1)", value).Scan(&key); err != nil {
		return nil, fmt.Errorf("normalizing label rule value %q: %w", value, err)
	}
	return key, nil
}

// ReevaluateAll は分類ルールの当たりを全録画へ差分適用する。
func (s *LabelRules) ReevaluateAll(ctx context.Context) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("beginning label rule re-evaluation: %w", err)
	}
	defer rollback(tx)

	if err := sqlcgen.New(tx).LockLabelRuleReevaluation(ctx); err != nil {
		return 0, fmt.Errorf("acquiring label rule re-evaluation lock: %w", err)
	}
	changed, err := reevaluateLabelRulesTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing label rule re-evaluation: %w", err)
	}
	return changed, nil
}

// reevaluateLabelRulesTx は差分適用と変更時の通知を tx 内で行う。
// 呼び出し側が tx を所有し、評価前に再評価ロックを取る。
func reevaluateLabelRulesTx(ctx context.Context, tx pgx.Tx) (int64, error) {
	q := sqlcgen.New(tx)
	changed, err := q.ApplyLabelRuleReevaluation(ctx)
	if err != nil {
		return 0, fmt.Errorf("re-evaluating label rules: %w", err)
	}
	if changed > 0 {
		if err := q.NotifyTopic(ctx, labelRuleRecordingsTopic); err != nil {
			return 0, fmt.Errorf("notifying recordings after label rule re-evaluation: %w", err)
		}
	}
	return int64(changed), nil
}

func labelRuleFromSQL(row sqlcgen.LabelRule) store.LabelRule {
	valueKey := ""
	if row.ValueKey != nil {
		valueKey = *row.ValueKey
	}
	return store.LabelRule{
		ID: row.ID, Key: row.Key, Value: row.Value, Keyword: row.Keyword,
		Priority: row.Priority, ValueKey: valueKey,
		CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
}
