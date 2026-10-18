package store

import (
	"context"
	"time"
)

// LabelRule は録画のシリーズ分類ルールを表す。
type LabelRule struct {
	ID        int64
	Key       string
	Value     string
	Keyword   string
	Priority  int32
	ValueKey  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// LabelRules はシリーズ分類ルールを永続化する。
//
// 各メソッドは自身の DB 操作を完了してから返す。Create / Update / Delete は
// それぞれの変更を原子的に commit する。
type LabelRules interface {
	// List は勝者順（priority 降順、id 昇順）でルールを返す。
	List(ctx context.Context) ([]LabelRule, error)
	// Get は id のルールを返す。存在しないときは ErrNotFound を返す。
	Get(ctx context.Context, id int64) (LabelRule, error)
	// Create は指定された内容のルールを作成して返す。
	Create(ctx context.Context, key, value, keyword string, priority int32) (LabelRule, error)
	// Update は id のルールを置き換えて返す。存在しないときは ErrNotFound を返す。
	Update(ctx context.Context, id int64, value, keyword string, priority int32) (LabelRule, error)
	// Delete は id のルールを削除する。存在しないときは ErrNotFound を返す。
	Delete(ctx context.Context, id int64) error
	// SeriesKey は DB の series_key 関数で value を正規化する。キーが作れない場合は nil を返す。
	SeriesKey(ctx context.Context, value string) (*string, error)
}

// LabelRuleReevaluation は、録画ごとの分類ルールの当たりを全件再評価する。
type LabelRuleReevaluation interface {
	// ReevaluateAll は全録画の当たりを差分適用し、変更した行数を返す。
	// 同時に実行できる再評価は 1 本だけである。後から始まった再評価は、先に
	// commit されたルールの集合で評価する。
	ReevaluateAll(ctx context.Context) (changed int64, err error)
}
