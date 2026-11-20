package store

import (
	"context"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
)

// CMDetectionWorkItem は CM 検出 worker が録画から読む入力を表す。
type CMDetectionWorkItem struct {
	ID              int64
	Station         CMStationRef
	RelPath         *string
	Enabled         bool
	IsTrashed       bool
	OriginalMissing bool
	Detected        bool
}

// CMDetectionAttempt は CM 検出の試行と、解析に使う局ロゴ版を表す。
// SaveDetection は AttemptCount と PolicyGeneration をフェンスに使う。
type CMDetectionAttempt struct {
	RecordingID      int64
	Station          CMStationRef
	AttemptCount     int32
	PolicyGeneration int64
	// LogoLearnedAt is the version used to analyze. Nil means the attempt had no saved logo.
	LogoLearnedAt *time.Time
}

// CMDetectionLogo は worker が新しく学習した局ロゴの永続化入力を表す。
type CMDetectionLogo struct {
	LGD         []byte
	PreviewPNG  []byte
	CodedWidth  int32
	CodedHeight int32
}

// CMDetectionJob は reconcile が CM 検出ジョブを投入するための入力を表す。
type CMDetectionJob struct {
	RecordingID         int64
	RecordingDurationMs int64
}

// CMDetectionRetry は再試行の commit 後に必要な応答とヒントの情報を表す。
type CMDetectionRetry struct {
	ActiveOriginal      bool
	Desired             bool
	RecordingDurationMs int64
}

// CMDetection は CM 検出 worker・reconcile・再試行 API の永続化契約である。
// 各メソッドは自身のトランザクションを完了してから返す。
// SaveDetection は局、録画、試行の順で書き手を直列化し、SaveLearnedLogo は局、試行の順で直列化する。
type CMDetection interface {
	// GetWorkItem は録画と有効な原本の検出入力を返す。録画が無ければ ErrNotFound を返す。
	// 生きた原本のコピーが無ければ RelPath は nil で、エラーにはしない。
	GetWorkItem(ctx context.Context, recordingID int64) (CMDetectionWorkItem, error)
	// GetLogoArea は局のロゴ枠を返す。
	GetLogoArea(ctx context.Context, station CMStationRef) (CMLogoArea, error)
	// GetLogo は局で学習済みのロゴを返す。
	GetLogo(ctx context.Context, station CMStationRef) (CMLogo, error)
	// IsDetectionDesired は録画の現在の CM 検出が desired かを返す。
	IsDetectionDesired(ctx context.Context, recordingID int64) (bool, error)
	// GetRecordingDuration は録画の実尺または番組尺をミリ秒で返す。
	GetRecordingDuration(ctx context.Context, recordingID int64) (int64, error)
	// BeginAttempt は現在の方針世代で試行を開始し、実行するかを返す。
	// 生きた原本のコピーが無い録画では試行行を作らず、shouldRun は false・err は nil を返す。
	BeginAttempt(ctx context.Context, recordingID int64, maxAttempts int32) (attempt CMDetectionAttempt, shouldRun bool, err error)
	// FailAttempt は一致する running 試行を失敗状態にし、更新したかを返す。
	FailAttempt(ctx context.Context, attempt CMDetectionAttempt, state string, stage *string, message string) (bool, error)
	// CancelAttempt は一致する2回目以降の running 試行を予算を消費しない状態に戻す。
	CancelAttempt(ctx context.Context, attempt CMDetectionAttempt) error
	// AbortAttempt は一致する初回の running 試行を削除する。
	AbortAttempt(ctx context.Context, attempt CMDetectionAttempt) error
	// SaveDetection は一致する現在世代の試行について、正規化した CM 区間を保存する。
	// 保存の時点で、試行の世代と試行回数が現在のものか、検出が desired か、試行がロゴを使っていた場合はロゴの版（learned_at）が現在のロゴと一致するかを確かめ直す。
	// 検出が desired でなければ試行を消して false を返す。ロゴの版が違えば、試行を消して保存しない（試行を消すことは契約テストでは未検証）。
	// ロゴを使わずに始まった試行は、後から現れたロゴとは照合しない。
	// 検査: RunCMDetection の次のテスト。
	//   - 「old generation cannot commit after a current attempt starts」（世代の照合）
	//   - 「trash during analysis clears the attempt budget」「result discards the attempt when the original became missing」（desired でないとき試行を消す）
	//   - 「result rejects a changed learned logo version」「result rejects a deleted logo the attempt started with」（ロゴの版の照合）
	//   - 「result started without a logo is saved even if a logo appears later」（ロゴを使わない試行）
	// 試行回数の照合は契約テストでは未検証。
	// 保存は同じ局のロゴ操作と、同じ録画のチャプター編集に直列化される。録画との直列化は RunCMDetection の「result waits for chapter edit row lock」で検査する。局との直列化は契約テストでは未検証。
	SaveDetection(ctx context.Context, attempt CMDetectionAttempt, ranges []chapters.Range) (bool, error)
	// SaveLearnedLogo は局の枠版と試行が一致する場合にロゴを保存し、保存したかを返す。
	// 保存は同じ局の枠操作と直列化される。検査: RunCMDetection の「learned logo waits and rejects the changed area version」（枠の版の照合と局ロックでの待ち）。局にロゴが既にあれば保存しない（契約テストでは未検証）。
	SaveLearnedLogo(ctx context.Context, attempt CMDetectionAttempt, logo CMDetectionLogo, observedAreaVersion *time.Time) (saved bool, err error)
	// RetryDetection は結果と試行を消し、原本がある場合に再試行情報を返す。
	RetryDetection(ctx context.Context, recordingID int64) (CMDetectionRetry, error)
	// DeleteStaleAttempts は現在の方針世代でない試行を削除する。
	DeleteStaleAttempts(ctx context.Context) error
	// ListMissingDetections は desired な検出を recording ID 順に先頭から limit 件返す。
	ListMissingDetections(ctx context.Context, limit int32) ([]CMDetectionJob, error)
	// ListMissingLogoCandidates は desired な候補解析を (network_id, service_id) 順に先頭から limit 件返す。
	ListMissingLogoCandidates(ctx context.Context, limit int32) ([]CMLogoCandidateJob, error)
	// FailOrphanLogoCandidates は worker の締切と recoveryGrace を過ぎた running 候補を失敗にする。
	// timeoutFor は録画尺に対するジョブの締切を返す。判定には store が記録した DB 時刻を使う。
	FailOrphanLogoCandidates(ctx context.Context, recoveryGrace time.Duration, timeoutFor func(recordingDurationMs int64) time.Duration) error
}
