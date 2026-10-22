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
	// EvaluateDedupe は候補ごとの重複排除結果を返す。now は録画の program_start_at と
	// 比べる時刻（時間窓の起点）で、呼び出し側が渡す。
	//
	// 類似度は pg_trgm の similarity() と同じ値で判定する。これは実装依存ではなく契約で、
	// 実装が別の方式に替わっても同じ入力は同じ判定になる。返り値は候補ごとに最良一致
	// 1 件（同率は録画 ID 昇順）で、マッチ無しの候補はマップに現れない。
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
	// InputRevision は読み取り開始時に GetRulerInputRevision で観測した入力の版である。
	InputRevision int64
	// SnapshotProgramIDs は射影の最新値へ追従させる予約・意図の番組 ID である。
	SnapshotProgramIDs []int64
	// ExistingProgramIDs は計画を作る前に読んだ既存予約の番組 ID である。
	ExistingProgramIDs  []int64
	Reservations        []RulerReservationInput
	FulfilledProgramIDs []int64
	DeleteProgramIDs    []int64
	// Now は撤回猶予窓の判定に使う適用時刻である。
	Now               time.Time
	RetractGrace      time.Duration
	MaxDeletesPerPass int
	// BreakerName は削除閾値を超えたとき発動するブレーカー識別子である。
	BreakerName    string
	BreakerTripped bool
}

// RulerPassResult は ApplyPass が commit した書き込みの結果である。
type RulerPassResult struct {
	// InputRevisionChanged は commit 時点の版が plan.InputRevision と違い、何も書かなかったことを表す。
	InputRevisionChanged     bool
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

// RulerInputBumper は ruler の入力が変わったことを記録する。
//
// ルール・意図・上書きの書き込みは、各メソッドの tx の中で版を進める。EPG 同期は
// 予約ドメインの書き込みを持たないので、同期の完了時にこの契約で進める。
type RulerInputBumper interface {
	// BumpRulerInputRevision は site の ruler 入力の版を 1 進める。
	BumpRulerInputRevision(ctx context.Context, site string) error
}

// RulerPass は ruler が予約の導出と適用に使う永続化契約である。
type RulerPass interface {
	RulerDedupe
	// GetRulerInputRevision は site の ruler 入力の版を返す。版を進めたことがない site は 0 とする。
	GetRulerInputRevision(ctx context.Context, site string) (int64, error)
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
	// commit 時点の site の版が plan.InputRevision と違えば、何も書かずに InputRevisionChanged を返す。
	// 予約・スナップショット・ブレーカーのいずれも変わらない。
	ApplyPass(ctx context.Context, site string, plan RulerPassPlan) (RulerPassResult, error)
	// PruneEndedSnapshots は指定 cutoff より前に終了した番組スナップショットを削除する。
	PruneEndedSnapshots(ctx context.Context, before time.Time) (int64, error)
}
