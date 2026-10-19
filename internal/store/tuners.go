package store

import (
	"context"
	"encoding/json"
	"time"
)

// Tuner は tuner_sync に投影されたチューナーを表す。
type Tuner struct {
	Site        string
	Index       int32
	Name        string
	Types       []string
	IsAvailable bool
	IsFault     bool
	ObservedAt  time.Time
}

// TunerInput は mirakc から観測したチューナーの状態を表す。
type TunerInput struct {
	Index       int32
	Name        string
	Types       []string
	IsAvailable bool
	IsFault     bool
}

// CapacityDemand は予約と番組スナップショットから読んだ容量需要を表す。
type CapacityDemand struct {
	Site string
	// RuleID は予約を作ったルール。ルールの無い予約は nil。
	RuleID       *int64
	ChannelType  string
	Channel      string
	StartAt      time.Time
	EndAt        time.Time
	Base         json.RawMessage
	IntentAction *string
	Overrides    json.RawMessage
}

// CapacityInput は容量判定に渡す需要とチューナーを表す。
type CapacityInput struct {
	Demands []CapacityDemand
	Tuners  []Tuner
}

// CapacityProgramRef は検索で一致した番組を site と program ID で指す。
type CapacityProgramRef struct {
	Site      string
	ProgramID int64
}

// CapacityPreviewRule は保存前プレビューの対象ルールに必要な状態を表す。
type CapacityPreviewRule struct {
	Enabled       bool
	DedupeEnabled bool
}

// CapacityPreviewCandidate は新たな容量需要になりうる番組を表す。
type CapacityPreviewCandidate struct {
	Site         string
	ProgramID    int64
	IntentAction *string
	ChannelType  string
	Channel      string
	StartAt      time.Time
	EndAt        time.Time
}

// TunerSync は mirakc の tuner 一覧を tuner_sync へ投影する。
//
// 各メソッドは個別に完了する。SweepMark と Upsert の間に mirakc への問い合わせが
// 入るので、同期全体を 1 つの tx に束ねない。
type TunerSync interface {
	// SweepMark は stale 判定の基準となる DB 時刻を返す。
	SweepMark(ctx context.Context) (time.Time, error)
	// Upsert は site のチューナー一覧を 1 バッチで原子的に反映する。
	// 1 件でも失敗したら全件を反映せず、空の一覧は何もしない。
	Upsert(ctx context.Context, site string, tuners []TunerInput) error
	// DeleteStale は observedBefore より古い site の行を削除し、削除件数を返す。
	// observedBefore には、Upsert より前に SweepMark が返した値を渡す。呼び出し側の
	// 時計を使うと、時計のずれで射影全体を消しうる。
	DeleteStale(ctx context.Context, site string, observedBefore time.Time) (int64, error)
}

// TunerReader は site のチューナー射影を一覧する。
type TunerReader interface {
	// List は利用可否や故障状態で絞らず、site の投影行を index 順で返す。
	List(ctx context.Context, site string) ([]Tuner, error)
}

// Capacity は容量判定に必要な予約・番組・意図・チューナーの読み取りを提供する。
type Capacity interface {
	// ReadSite は site の需要とチューナーを返す。
	ReadSite(ctx context.Context, site string) (CapacityInput, error)
	// ReadAllSites は全 site の需要とチューナーを返す。
	ReadAllSites(ctx context.Context) (CapacityInput, error)
	// PreviewRule は保存前プレビューの対象ルールの状態を返す。
	// 対象が無ければ ErrNotFound を返す。
	PreviewRule(ctx context.Context, ruleID int64) (CapacityPreviewRule, error)
	// PreviewCandidates は refs のうち、容量需要になりうる候補を返す。
	// ruleID は編集中のルールで、そのルールが今持つ予約は除外の対象に数えない。
	// ルールの存在は確かめない。
	PreviewCandidates(ctx context.Context, refs []CapacityProgramRef, ruleID *int64) ([]CapacityPreviewCandidate, error)
}
