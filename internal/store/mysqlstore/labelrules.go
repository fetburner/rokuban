package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/fetburner/rokuban/internal/serieskey"
	"github.com/fetburner/rokuban/internal/store"
)

// labelRuleReconcileLock は全件の再評価を直列化する名前付きロックの行の名前。
// 行は migration 00012 で置く。
const labelRuleReconcileLock = "label_rule_reconcile"

// LabelRules は MySQL で分類ルールの CRUD と全件の再評価を実装する。
// 再評価は labelRuleReconcileLock の行を FOR UPDATE で取り、直列化する。
// 録画の書き込みによる当たりの維持は、migration 00012 のトリガーが行う。
type LabelRules struct {
	db *sql.DB
}

var _ store.LabelRules = (*LabelRules)(nil)
var _ store.LabelRuleReevaluation = (*LabelRules)(nil)

// NewLabelRules は MySQL を使う LabelRules を作る。
func NewLabelRules(db *sql.DB) *LabelRules {
	return &LabelRules{db: db}
}

const labelRuleColumns = "id, `key`, value, keyword, priority, value_key, created_at, updated_at"

// List は勝者順で分類ルールを返す。
func (s *LabelRules) List(ctx context.Context) ([]store.LabelRule, error) {
	rules, err := queryRows(ctx, s.db, "label rules",
		"SELECT "+labelRuleColumns+" FROM label_rules ORDER BY priority DESC, id ASC",
		scanLabelRule)
	if err != nil {
		return nil, err
	}
	return rules, nil
}

// Get は id の分類ルールを返す。
func (s *LabelRules) Get(ctx context.Context, id int64) (store.LabelRule, error) {
	return getLabelRule(ctx, s.db, id)
}

// Create は指定された内容の分類ルールを作成して返す。value_key は value から Go で求めて書く。
func (s *LabelRules) Create(ctx context.Context, key, value, keyword string, priority int32) (store.LabelRule, error) {
	var created store.LabelRule
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		valueKey, err := labelRuleValueKey(value)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
 INSERT INTO label_rules (`+"`key`"+`, value, keyword, priority, value_key) VALUES (?, ?, ?, ?, ?)
 `, key, value, keyword, priority, valueKey)
		if err != nil {
			return fmt.Errorf("creating label rule: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("reading created label rule id: %w", err)
		}
		created, err = getLabelRule(ctx, tx, id)
		return err
	})
	if err != nil {
		return store.LabelRule{}, err
	}
	return created, nil
}

// Update は id の分類ルールを置き換えて返す。value_key は新しい value から書き直す。
func (s *LabelRules) Update(ctx context.Context, id int64, value, keyword string, priority int32) (store.LabelRule, error) {
	var updated store.LabelRule
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockLabelRule(ctx, tx, id); err != nil {
			return err
		}
		valueKey, err := labelRuleValueKey(value)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
 UPDATE label_rules SET value = ?, keyword = ?, priority = ?, value_key = ?, updated_at = CURRENT_TIMESTAMP(6)
 WHERE id = ?
 `, value, keyword, priority, valueKey, id); err != nil {
			return fmt.Errorf("updating label rule %d: %w", id, err)
		}
		updated, err = getLabelRule(ctx, tx, id)
		return err
	})
	if err != nil {
		return store.LabelRule{}, err
	}
	return updated, nil
}

// Delete は id の分類ルールを削除する。当たりは FK の CASCADE で消え、次点への移動は再評価が行う。
func (s *LabelRules) Delete(ctx context.Context, id int64) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockLabelRule(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM label_rules WHERE id = ?`, id); err != nil {
			return fmt.Errorf("deleting label rule %d: %w", id, err)
		}
		return nil
	})
}

// SeriesKey は value を Go の series_key で正規化する。キーが無い値は nil を返す。
func (s *LabelRules) SeriesKey(_ context.Context, value string) (*string, error) {
	key, ok := serieskey.SeriesKey(value)
	if !ok {
		return nil, nil
	}
	return &key, nil
}

// ReevaluateAll は分類ルールの当たりを全録画へ差分適用する。
// 再評価は名前付きロックの行を保持したまま、評価と適用を 1 tx で行う。
// 全件の再評価と、ルールの編集（CRUD）は、コミットされた順に評価される。
func (s *LabelRules) ReevaluateAll(ctx context.Context) (int64, error) {
	var changed int64
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockLabelRuleReconcile(ctx, tx); err != nil {
			return err
		}
		n, err := reevaluateLabelRules(ctx, tx)
		if err != nil {
			return err
		}
		changed = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	return changed, nil
}

// lockLabelRuleReconcile は全件の再評価の名前付きロックを取る。
// 取った tx が commit / rollback するまで、他の再評価は待つ。
func lockLabelRuleReconcile(ctx context.Context, tx *sql.Tx) error {
	var locked string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM named_locks WHERE name = ? FOR UPDATE`, labelRuleReconcileLock).Scan(&locked); err != nil {
		return fmt.Errorf("acquiring label rule re-evaluation lock: %w", err)
	}
	return nil
}

// reevaluateLabelRules は当たりが変わる録画だけを書き直し、変わった録画数を返す。
// 呼び出し側の tx が再評価のロックを保持している前提である。
// 変化の数え方は、勝者が変わった録画の数（Postgres の upsert と removed の合計と同じ）。
func reevaluateLabelRules(ctx context.Context, tx *sql.Tx) (int64, error) {
	var changed int64
	if err := tx.QueryRowContext(ctx, `
 SELECT COUNT(*)
 FROM label_rule_winners w
 LEFT JOIN label_rule_hits h ON h.recording_id = w.recording_id
 WHERE NOT (h.label_rule_id <=> w.label_rule_id)
 `).Scan(&changed); err != nil {
		return 0, fmt.Errorf("counting changed label rule hits: %w", err)
	}
	if changed == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, `
 DELETE h FROM label_rule_hits h
 JOIN label_rule_winners w ON w.recording_id = h.recording_id
 WHERE NOT (h.label_rule_id <=> w.label_rule_id)
 `); err != nil {
		return 0, fmt.Errorf("removing stale label rule hits: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
 INSERT INTO label_rule_hits (recording_id, label_rule_id)
 SELECT w.recording_id, w.label_rule_id
 FROM label_rule_winners w
 WHERE w.label_rule_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM label_rule_hits h WHERE h.recording_id = w.recording_id)
 `); err != nil {
		return 0, fmt.Errorf("inserting label rule hits: %w", err)
	}
	if err := NotifyTopic(ctx, tx, "recordings"); err != nil {
		return 0, fmt.Errorf("notifying recordings after label rule re-evaluation: %w", err)
	}
	return changed, nil
}

// labelRuleValueKey は value の series_key を求める。キーが無い値は label_rules の制約（value_key が NULL でない）に反するので拒む。
func labelRuleValueKey(value string) (string, error) {
	key, ok := serieskey.SeriesKey(value)
	if !ok {
		return "", fmt.Errorf("label rule value %q has no series key", value)
	}
	return key, nil
}

// lockLabelRule は id の分類ルールの行を FOR UPDATE で取る。無ければ ErrNotFound。
func lockLabelRule(ctx context.Context, tx *sql.Tx, id int64) error {
	var locked int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM label_rules WHERE id = ? FOR UPDATE`, id).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("label rule %d: %w", id, store.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("locking label rule %d: %w", id, err)
	}
	return nil
}

// getLabelRule は id の分類ルールを 1 件読む。tx を渡せば、その tx の書き込みが見える。
func getLabelRule(ctx context.Context, db rowQuerier, id int64) (store.LabelRule, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+labelRuleColumns+" FROM label_rules WHERE id = ?", id)
	if err != nil {
		return store.LabelRule{}, fmt.Errorf("getting label rule %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return store.LabelRule{}, fmt.Errorf("getting label rule %d: %w", id, err)
		}
		return store.LabelRule{}, fmt.Errorf("label rule %d: %w", id, store.ErrNotFound)
	}
	rule, err := scanLabelRule(rows)
	if err != nil {
		return store.LabelRule{}, fmt.Errorf("scanning label rule %d: %w", id, err)
	}
	return rule, nil
}

func scanLabelRule(rows *sql.Rows) (store.LabelRule, error) {
	var r store.LabelRule
	err := rows.Scan(&r.ID, &r.Key, &r.Value, &r.Keyword, &r.Priority, &r.ValueKey, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}
