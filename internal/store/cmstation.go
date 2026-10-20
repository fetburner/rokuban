package store

import (
	"context"
	"errors"
	"time"
)

// ErrCMLogoRecordingNotFound はロゴ枠の解析元録画が存在しないことを表す。
var ErrCMLogoRecordingNotFound = errors.New("CM logo analysis recording not found")

// ErrCMLogoRecordingWrongStation は解析元録画が指定局に属さないことを表す。
var ErrCMLogoRecordingWrongStation = errors.New("CM logo analysis recording belongs to another station")

// ErrCMLogoRecordingOriginalUnavailable は解析元録画に利用可能な原本がないことを表す。
var ErrCMLogoRecordingOriginalUnavailable = errors.New("CM logo analysis recording has no active original")

// CMStationRef は CM ロゴ状態を識別する放送局である。
type CMStationRef struct {
	NetworkID int32
	ServiceID int32
}

// CMLogoArea は局に設定されたロゴ枠を表す。
type CMLogoArea struct {
	X           int32
	Y           int32
	W           int32
	H           int32
	CodedWidth  int32
	CodedHeight int32
	UpdatedAt   time.Time
}

// CMLogo は局で学習済みのロゴを表す。
type CMLogo struct {
	LGD         []byte
	CodedWidth  int32
	CodedHeight int32
	LearnedAt   time.Time
}

// CMLogoCandidate は局のロゴ候補を表す。
type CMLogoCandidate struct {
	State                 string
	Stage                 *string
	Error                 *string
	X                     int32
	Y                     int32
	W                     int32
	H                     int32
	CodedWidth            int32
	CodedHeight           int32
	RecordingID           *int64
	ObservedAreaUpdatedAt time.Time
	LGD                   []byte
	PreviewPNG            []byte
	AttemptedAt           time.Time
}

// CMLogoCandidateSummary は局状態一覧が返す候補の表示用情報を表す。
// 一覧クエリは候補の観測した枠版を選ばないため、その版は含めない。
type CMLogoCandidateSummary struct {
	State       string
	Stage       *string
	Error       *string
	X           int32
	Y           int32
	W           int32
	H           int32
	CodedWidth  int32
	CodedHeight int32
	RecordingID *int64
	PreviewPNG  []byte
	AttemptedAt time.Time
}

// CMLogoState は局ごとのロゴ、枠、候補と CM 検出の集計を表す。
type CMLogoState struct {
	NetworkID          int32
	ServiceID          int32
	ServiceName        string
	Site               string
	RecordingCount     int64
	LearnedAt          *time.Time
	PreviewPNG         []byte
	FailedCount        int64
	PendingCount       int64
	LastFailureStage   string
	DetectedCount      int64
	RedetectableCount  int64
	LearnedCodedWidth  *int32
	LearnedCodedHeight *int32
	Area               *CMLogoArea
	Candidate          *CMLogoCandidateSummary
	FrameRecordingID   int64
}

// CMLogoAreaInput は枠を保存するための画素座標と解析元録画である。
type CMLogoAreaInput struct {
	RecordingID int64
	X           int32
	Y           int32
	W           int32
	H           int32
	CodedWidth  int32
	CodedHeight int32
}

// CMLogoCandidateJob は枠の保存後に候補解析を始めるための commit 後ヒントである。
type CMLogoCandidateJob struct {
	Station             CMStationRef
	RecordingID         int64
	RecordingDurationMs int64
	AreaUpdatedAt       time.Time
}

// CMLogoDeleteResult はロゴ削除後に必要な候補解析ヒントを表す。
// CandidateJob が nil の場合、呼び出し側は reconcile のヒントを投入する。
type CMLogoDeleteResult struct {
	CandidateJob *CMLogoCandidateJob
}

// CMLogoCandidateRecording は候補解析の対象となる録画と原本の状態を表す。
type CMLogoCandidateRecording struct {
	ID              int64
	NetworkID       int32
	ServiceID       int32
	RelPath         *string
	IsTrashed       bool
	OriginalMissing bool
}

// CMLogoAdoptionReason は候補を採用できない理由を表す。
type CMLogoAdoptionReason string

const (
	// CMLogoAdoptionCandidateUnavailable は候補が存在しないことを表す。
	CMLogoAdoptionCandidateUnavailable CMLogoAdoptionReason = "candidate_unavailable"
	// CMLogoAdoptionCandidateNotReady は候補の解析が終端の ready でないことを表す。
	CMLogoAdoptionCandidateNotReady CMLogoAdoptionReason = "candidate_not_ready"
	// CMLogoAdoptionAreaMissing は学習に使った枠が存在しないことを表す。
	CMLogoAdoptionAreaMissing CMLogoAdoptionReason = "area_missing"
	// CMLogoAdoptionAreaChanged は学習後に枠が変更されたことを表す。
	CMLogoAdoptionAreaChanged CMLogoAdoptionReason = "area_changed"
	// CMLogoAdoptionCandidateUnusable は候補に採用可能な録画またはロゴがないことを表す。
	CMLogoAdoptionCandidateUnusable CMLogoAdoptionReason = "candidate_unusable"
)

// CMLogoAdoptionError は候補の採用を拒否した理由を型付きで返す。
type CMLogoAdoptionError struct {
	Reason CMLogoAdoptionReason
	State  string
}

// Error は候補採用エラーの文脈を返す。
func (e *CMLogoAdoptionError) Error() string {
	if e.State != "" {
		return "CM logo candidate cannot be adopted: " + e.State
	}
	return "CM logo candidate cannot be adopted: " + string(e.Reason)
}

// CMStation は CM ロゴの局単位状態と候補解析を永続化する。
// 同じ局について、ロゴ・枠・候補を書くメソッド（SaveLogoArea・DeleteLogoArea・DeleteLogo・
// DeleteLogoCandidate・AdoptLogoCandidate・BeginLogoCandidate）は、互いに直列化される。
// CM 検出の結果を保存する操作とロゴの学習を保存する操作とも直列化される。
// 例外は MarkLogoCandidateReady・MarkLogoCandidateFailed・DiscardLogoCandidate で、
// 直列化されず、版の照合で守る。
// 結果を保存する操作は、前提にした状態が書き込み時点でも同じかを検査し、違えば書かない。
// ロゴの学習の保存は枠の版（updated_at）を、CM 検出の結果の保存はロゴの版（learned_at）を照合する。
// MarkLogoCandidateReady は、候補行の試行時刻と、候補が読んだ枠の版が現在の枠の版と同じかを照合する。
// MarkLogoCandidateFailed と DiscardLogoCandidate は、現在の枠を見ず、
// 候補行の枠の版と試行時刻が渡された値と一致する running 候補だけを対象にする。
// 各メソッドは自身のトランザクションを完了してから返す。
type CMStation interface {
	// ListCMLogoStates はロゴ、枠、候補および CM 検出の局別状態を返す。
	ListCMLogoStates(ctx context.Context) ([]CMLogoState, error)
	// GetLogoArea は局のロゴ枠を返す。
	GetLogoArea(ctx context.Context, station CMStationRef) (CMLogoArea, error)
	// GetLogo は局の学習済みロゴを返す。
	GetLogo(ctx context.Context, station CMStationRef) (CMLogo, error)
	// GetLogoCandidate は局の現在の候補を返す。
	GetLogoCandidate(ctx context.Context, station CMStationRef) (CMLogoCandidate, error)
	// GetCandidateRecording は候補解析用の録画と原本状態を返す。
	GetCandidateRecording(ctx context.Context, recordingID int64) (CMLogoCandidateRecording, error)
	// SaveLogoArea は枠を保存し既存候補を消して、候補解析用ヒントの宛先を返す。
	// 同じ局の他の書き込みと、CM 検出の結果・ロゴの学習結果の保存に直列化される。
	SaveLogoArea(ctx context.Context, station CMStationRef, input CMLogoAreaInput) (CMLogoCandidateJob, error)
	// DeleteLogoArea は採用待ちの試行とロゴ枠を原子的に消す。
	// 同じ局の他の書き込みと、CM 検出の結果・ロゴの学習結果の保存に直列化される。
	DeleteLogoArea(ctx context.Context, station CMStationRef) error
	// DeleteLogo はロゴを消し、次に投入する候補解析の宛先を返す。
	// 同じ局の他の書き込みと、CM 検出の結果・ロゴの学習結果の保存に直列化される。
	DeleteLogo(ctx context.Context, station CMStationRef) (CMLogoDeleteResult, error)
	// DeleteLogoCandidate は局の現在の候補を消す。同じ局の他の書き込みと、CM 検出の結果・ロゴの学習結果の保存に直列化される。
	DeleteLogoCandidate(ctx context.Context, station CMStationRef) error
	// AdoptLogoCandidate は ready な候補を採用し、redetect が真なら有効な録画の結果も消す。
	// 同じ局の他の書き込みと、CM 検出の結果・ロゴの学習結果の保存に直列化される。
	AdoptLogoCandidate(ctx context.Context, station CMStationRef, redetect bool) error
	// BeginLogoCandidate は現在の枠が desired の場合に候補の running 試行を作る。
	// 同じ局の他の書き込みと、CM 検出の結果・ロゴの学習結果の保存に直列化される。
	BeginLogoCandidate(ctx context.Context, station CMStationRef, recordingID int64, areaUpdatedAt time.Time) (attemptedAt time.Time, started bool, err error)
	// MarkLogoCandidateReady は枠版と試行時刻が一致する running 候補だけを ready にする。
	// 現在の枠版が areaUpdatedAt と違えば書かない。
	MarkLogoCandidateReady(ctx context.Context, station CMStationRef, areaUpdatedAt, attemptedAt time.Time, lgd, previewPNG []byte) (bool, error)
	// MarkLogoCandidateFailed は候補行の枠版と試行時刻が一致する running 候補だけを failed にする。
	MarkLogoCandidateFailed(ctx context.Context, station CMStationRef, areaUpdatedAt, attemptedAt time.Time, stage *string, message string) (bool, error)
	// DiscardLogoCandidate は候補行の枠版と試行時刻が一致する running 候補を消す。
	DiscardLogoCandidate(ctx context.Context, station CMStationRef, areaUpdatedAt, attemptedAt time.Time) error
}
