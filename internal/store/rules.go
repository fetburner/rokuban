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
	// 同じ tx で、対象 site の ruler 入力の版を 1 進める。sites が空のルールは構成の全 site を対象とする。
	Create(ctx context.Context, in RuleInput) (Rule, error)
	// Update はルール本体と全ての子条件を置き換える。同じルールへの更新は直列化する。
	// 同じ tx で、更新前と更新後の対象 site の両方について ruler 入力の版を 1 進める。
	// どちらかの sites が空なら構成の全 site を進める。
	Update(ctx context.Context, id int64, in RuleInput) (Rule, error)
	// Delete は、意図の無い予約の削除とルールの削除を原子的に行う。
	// 意図のある予約は残し、ルールを削除すると録画と予約からルール参照が外れる。
	// 結果には削除前の対象 site を含める。
	// 同じ tx で、削除前の対象 site の ruler 入力の版を 1 進める（sites が空だったルールは構成の全 site）。
	Delete(ctx context.Context, id int64) (RuleDeleteResult, error)
}

// RuleImport は EPGStation の識別子でルールを冪等に取り込む。
//
// 既存のルールを更新するとき、Description・Priority・DedupeEnabled・DedupeThreshold・
// DedupeWindowSeconds・KeepOriginal・EncodeProfiles は既存の値を残し、RuleInput の
// 同じフィールドは使わない。EPGStation のルールからの変換はこれらの値を作らないので、
// 入力の値で置き換えると、再取り込みのたびに既定値へ戻ってしまう。Metadata は
// EPGStation の識別子で上書きする。Name・Enabled・IsFree・DurationMinMs・DurationMaxMs・
// PeriodStartAt・PeriodEndAt・FilenameTemplate と子条件は入力で置き換える。
type RuleImport interface {
	// ImportEPGStationRule は EPGStation rule ID のルールを作成または更新し、子条件を全置換する。
	// 作成または更新と子条件は 1 つのトランザクションで確定する。
	// 同じ tx で、Create / Update と同じ site の ruler 入力の版を 1 進める。
	ImportEPGStationRule(ctx context.Context, epgstationRuleID int64, in RuleInput) (created bool, err error)
}
