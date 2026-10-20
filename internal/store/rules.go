package store

import (
	"context"
	"time"
)

// RuleTextMatch は、ルールが番組の文字列項目に適用する条件である。
type RuleTextMatch struct {
	Target        string
	Mode          string
	Value         string
	CaseSensitive bool
	Negate        bool
}

// RuleService は、ルールが対象とする放送サービスである。
type RuleService struct {
	NetworkID int32
	ServiceID int32
}

// RuleTimeWindow は、ルールが対象とする曜日と時刻の範囲である。
type RuleTimeWindow struct {
	Weekdays int32
	StartSec int32
	EndSec   int32
}

// RuleInput は、ルール本体と子条件の保存内容である。
//
// ポインタのフィールドが nil のときは、ルールの既定値を使う。子条件の nil と空は
// 同じ意味で、どちらも子行を作らない。
type RuleInput struct {
	Name                string
	Description         *string
	Enabled             *bool
	Priority            *int32
	IsFree              *bool
	DurationMinMs       *int64
	DurationMaxMs       *int64
	PeriodStartAt       *time.Time
	PeriodEndAt         *time.Time
	DedupeEnabled       *bool
	DedupeThreshold     *float32
	DedupeWindowSeconds *int64
	KeepOriginal        *string
	EncodeProfiles      *[]string
	FilenameTemplate    *string
	Metadata            *map[string]any
	TextMatches         []RuleTextMatch
	Services            []RuleService
	ChannelTypes        []string
	Genres              []int16
	Times               []RuleTimeWindow
	Sites               []string
}

// Rule は、ルール本体とその子条件を表す。
type Rule struct {
	ID                  int64
	Name                string
	Description         string
	Enabled             bool
	Priority            int32
	IsFree              *bool
	DurationMinMs       *int64
	DurationMaxMs       *int64
	PeriodStartAt       *time.Time
	PeriodEndAt         *time.Time
	DedupeEnabled       bool
	DedupeThreshold     *float32
	DedupeWindowSeconds *int64
	KeepOriginal        string
	EncodeProfiles      []string
	FilenameTemplate    string
	Metadata            map[string]any
	TextMatches         []RuleTextMatch
	Services            []RuleService
	ChannelTypes        []string
	Genres              []int16
	Times               []RuleTimeWindow
	Sites               []string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// RuleDeleteResult はルール削除時の予約の内訳と、ruler を起動する対象サイトを表す。
type RuleDeleteResult struct {
	DeletedReservations  int64
	DetachedReservations int64
	Sites                []string
}

// Rules は録画ルールの読み書きを提供する。
//
// 各書き込みメソッドはルール本体と子表を 1 つのトランザクションで確定する。
// Delete は意図の無い予約の削除とルールの削除も同じトランザクションで行う。
type Rules interface {
	// List は子条件を含むルールを優先度降順、ID 昇順で返す。
	List(ctx context.Context) ([]Rule, error)
	// Get は子条件を含むルールを返す。存在しないときは ErrNotFound を返す。
	Get(ctx context.Context, id int64) (Rule, error)
	// Sites は保存済みの対象 site を返す。ルールが無い場合は空の一覧を返す。
	Sites(ctx context.Context, id int64) ([]string, error)
	// Create はルール本体と全ての子条件を原子的に作成する。
	Create(ctx context.Context, in RuleInput) (Rule, error)
	// Update はルール本体と全ての子条件を置き換える。同じルールへの更新は直列化する。
	Update(ctx context.Context, id int64, in RuleInput) (Rule, error)
	// Delete は、意図の無い予約の削除とルールの削除を原子的に行う。
	// 意図のある予約は残し、ルールを削除すると録画と予約からルール参照が外れる。
	// 結果には削除前の対象 site を含める。
	Delete(ctx context.Context, id int64) (RuleDeleteResult, error)
}

// RuleImport は EPGStation の識別子でルールを冪等に取り込む。
//
// 既存のルールを更新するとき、Description・Priority・DedupeEnabled・DedupeThreshold・
// DedupeWindowSeconds・KeepOriginal・EncodeProfiles は既存の値を残す。これらは取り込み後に
// rokuban 側で編集される値だからである。Metadata は EPGStation の識別子で上書きする。
// そのため RuleInput のこれらのフィールドは更新では使わない（nil の意味が Create の既定値とは
// 違う）。名前・有効・条件・FilenameTemplate・子条件は入力で置き換える。
type RuleImport interface {
	// ImportEPGStationRule は EPGStation rule ID のルールを作成または更新し、子条件を全置換する。
	// 作成または更新と子条件は 1 つのトランザクションで確定する。
	ImportEPGStationRule(ctx context.Context, epgstationRuleID int64, in RuleInput) (created bool, err error)
}
