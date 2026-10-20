package store

import (
	"context"
	"encoding/json"
	"time"
)

// RulerProgramIDReuse は program_id の再利用が疑われるスナップショットと射影の時刻を表す。
type RulerProgramIDReuse struct {
	ProgramID         int64
	SnapshotStartAt   time.Time
	ProjectionStartAt time.Time
}

// RulerProgramIntent は ruler が判定に使う番組の意図である。
type RulerProgramIntent struct {
	ProgramID int64
	Action    string
}

// RulerDedupeCandidate は重複排除の評価対象である。
type RulerDedupeCandidate struct {
	ProgramID int64
	RuleID    int64
}

// RulerDedupeMatch は候補に最もよく一致した録画とその類似度である。
type RulerDedupeMatch struct {
	RecordingID int64
	Similarity  float32
}

// RulerDedupe は録画候補に対する重複排除の読み取り契約である。
type RulerDedupe interface {
	// EvaluateDedupe は候補ごとの重複排除結果を返す。now は録画開始時刻との比較に使う。
	EvaluateDedupe(ctx context.Context, site string, candidates []RulerDedupeCandidate, now time.Time) (map[int64]RulerDedupeMatch, error)
}

// RulerReservationInput は ruler が 1 件の予約へ導出した書き込み値である。
type RulerReservationInput struct {
	ProgramID             int64
	RuleID                *int64
	Base                  json.RawMessage
	DedupMatchRecordingID *int64
	DedupSimilarity       *float32
}

// RulerPassPlan は ruler が 1 サイトについて評価した書き込み計画である。
type RulerPassPlan struct {
	// SnapshotProgramIDs は射影の最新値へ追従させる予約・意図の番組 ID である。
	SnapshotProgramIDs []int64
	// ExistingProgramIDs は計画を作る前に読んだ既存予約の番組 ID である。
	ExistingProgramIDs  []int64
	Reservations        []RulerReservationInput
	FulfilledProgramIDs []int64
	DeleteProgramIDs    []int64
	Now                 time.Time
	RetractGrace        time.Duration
	MaxDeletesPerPass   int
	// BreakerName は削除閾値を超えたとき発動するブレーカー識別子である。
	BreakerName    string
	BreakerTripped bool
}

// RulerPassResult は ApplyPass が commit した書き込みの結果である。
type RulerPassResult struct {
	Created                  int
	Updated                  int
	UnmaterializedProgramIDs []int64
	FulfilledDeleted         int64
	ReleasedProgramIDs       []int64
	Deleted                  int64
	GraceProtected           int
	BreakerLatched           bool
	BreakerTripped           bool
	NewlyTripped             bool
	BreakerTrippedAt         time.Time
	BreakerSample            BreakerSample
}

// RulerPass は ruler が予約の導出と適用に使う永続化契約である。
type RulerPass interface {
	RulerDedupe
	// ListEnabled は有効なルール本体を優先度降順、ID 昇順で返す。子条件は含まない。
	ListEnabled(ctx context.Context) ([]Rule, error)
	// ListProgramIDReuses は指定時刻に終了済みスナップショットと射影を比べ、再利用が疑われる番組を返す。
	ListProgramIDReuses(ctx context.Context, site string, now time.Time) ([]RulerProgramIDReuse, error)
	// ListProgramIntents は site の番組意図を返す。
	ListProgramIntents(ctx context.Context, site string) ([]RulerProgramIntent, error)
	// ListInvestmentProgramIDs は record 意図または上書きを持つ番組 ID を返す。
	ListInvestmentProgramIDs(ctx context.Context, site string) ([]int64, error)
	// ListFulfilledProgramIDs は原本の取り込みが完了した放送イベントの番組 ID を返す。
	ListFulfilledProgramIDs(ctx context.Context, site string) ([]int64, error)
	// ListReservationProgramIDs は site の既存予約の番組 ID を返す。
	ListReservationProgramIDs(ctx context.Context, site string) ([]int64, error)
	// ListProjectedProgramIDs は指定した番組 ID のうち現在の EPG 射影にあるものを返す。
	ListProjectedProgramIDs(ctx context.Context, site string, programIDs []int64) ([]int64, error)
	// ApplyPass は予約の upsert、fulfilled・released・導出削除、snapshot 追従、必要なブレーカー発動を
	// 1 つのトランザクションで確定する。削除時には現在の program investment を再評価する。
	ApplyPass(ctx context.Context, site string, plan RulerPassPlan) (RulerPassResult, error)
	// PruneEndedSnapshots は指定 cutoff より前に終了した番組スナップショットを削除する。
	PruneEndedSnapshots(ctx context.Context, before time.Time) (int64, error)
}
