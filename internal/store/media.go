package store

import (
	"context"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
)

// MediaAsset は配信または解析に使う確定済みメディアアセットを表す。
type MediaAsset struct {
	ID        int64
	RelPath   string
	SizeBytes int64
}

// OriginalVODTarget はサイト内で配信できる原本を表す。
type OriginalVODTarget struct {
	MediaAsset
	Site string
}

// RecordingRemovalState は原本 VOD の継続中セッションが確認する録画状態を表す。
type RecordingRemovalState struct {
	DeletedAt    *time.Time
	PurgedAt     *time.Time
	SupersededAt *time.Time
}

// ChaseTarget は追っかけ再生の録画と mirakc record の対応を表す。
type ChaseTarget struct {
	Site             string
	RecordID         string
	Status           string
	RecordingStatus  string
	HasOriginalAsset bool
}

// MediaServing は streamer が使うメディアと録画状態の読み取り契約である。
type MediaServing interface {
	// GetOriginalMediaAssetForServing は配信可能な原本を返す。無ければ ErrNotFound を返す。
	GetOriginalMediaAssetForServing(ctx context.Context, recordingID int64) (MediaAsset, error)
	// GetEncodedMediaAssetForServing は指定プロファイルの配信可能な encoded を返す。
	GetEncodedMediaAssetForServing(ctx context.Context, recordingID int64, profile string) (MediaAsset, error)
	// GetThumbnailMediaAssetForServing は配信可能なサムネイルを返す。
	GetThumbnailMediaAssetForServing(ctx context.Context, recordingID int64) (MediaAsset, error)
	// GetSeekTilesMediaAssetForServing は配信可能なシークタイルを返す。
	GetSeekTilesMediaAssetForServing(ctx context.Context, recordingID int64) (MediaAsset, error)
	// GetActiveOriginalMediaAsset は追っかけ配信に使う active な原本を返す。
	GetActiveOriginalMediaAsset(ctx context.Context, recordingID int64) (MediaAsset, error)
	// GetOriginalVODTarget はサイトに属する完了済み録画の配信可能な原本を返す。
	GetOriginalVODTarget(ctx context.Context, recordingID int64, site string) (OriginalVODTarget, error)
	// GetRecordingRemovalState は録画の削除・purge・supersede の状態を返す。
	GetRecordingRemovalState(ctx context.Context, recordingID int64) (RecordingRemovalState, error)
	// GetChaseTarget は追っかけ再生に必要な mirakc record と録画状態を返す。
	GetChaseTarget(ctx context.Context, recordingID int64) (ChaseTarget, error)
}

// MissingEncodeProfilesParams は未生成 encoded のページ条件を表す。
type MissingEncodeProfilesParams struct {
	AfterRecordingID int64
	RecordingID      *int64
	KnownProfiles    []string
	CutProfiles      []string
	AttemptLimit     int32
	RowLimit         int32
}

// MissingEncodeProfile は desired だが active な成果物が無い録画とプロファイルを表す。
type MissingEncodeProfile struct {
	RecordingID int64
	Profile     string
}

// EncodeAssetRebuildRequest は押された時点で固定された cut 範囲の作り直し要求を表す。
type EncodeAssetRebuildRequest struct {
	RecordingID   int64
	Profile       string
	RequestedKeep []chapters.Range
	DecodeErr     error
}

// EncodeProfileCount はプロファイルごとの録画数を表す。
type EncodeProfileCount struct {
	Profile    string
	Recordings int64
}

// EncodedMeaningEvidence は過去にプロファイル名が持った成果物の意味を表す。
type EncodedMeaningEvidence struct {
	Profile string
	RelPath string
}

// EncodedAssetObservation はエンコード計画が観測した encoded アセットを表す。
// KeepRanges は cut プロファイルの要求で凍結された区間で、凍結されていなければ空である。
type EncodedAssetObservation struct {
	ID         int64
	RelPath    string
	State      string
	KeepRanges []chapters.Range
}

// MediaReconcilePage は録画 ID で巡回するメディア reconcile のページ条件を表す。
type MediaReconcilePage struct {
	AfterRecordingID int64
	RecordingID      *int64
	RowLimit         int32
}

// ThumbnailEncodedAsset はサムネイルを切り出せる encoded 1 件を表す。
// Cut の行は、その encoded を作った時点で凍結した keep 区間を持つ。
type ThumbnailEncodedAsset struct {
	MediaAssetID int64
	Profile      string
	RelPath      string
	Cut          bool
	KeepRanges   []chapters.Range
}

// ThumbnailReselectCandidate はサムネイル位置を再評価する入力を表す。
//
// CMRanges / UserSpans / EncodedAssets は DB の JSON を実装が型へ戻した値で、
// 録画に CM 検出もユーザーのチャプターも無い行（Detected も Owned も false）では読まず、空のままにする。
// 戻せなかった行は、その段階の TimelineDecodeErr / InputsDecodeErr に原因を持つ。
// 1 行の不整合でページ全体を失敗させず、利用側が行ごとに扱えるようにするためである。
type ThumbnailReselectCandidate struct {
	RecordingID           int64
	Trashed               bool
	ProgramDurationMs     int64
	Detected              bool
	Owned                 bool
	CMRanges              []chapters.Range
	UserSpans             []chapters.Span
	TimelineDecodeErr     error
	OriginalMediaAssetID  int64
	OriginalRelPath       string
	EncodedAssets         []ThumbnailEncodedAsset
	InputsDecodeErr       error
	ThumbnailMediaAssetID *int64
	ThumbnailRelPath      *string
	SeekMs                *int64
}

// MediaAssetRelPathViolation はサイト名空間を満たさない原本または encoded を表す。
type MediaAssetRelPathViolation struct {
	ID      int64
	Kind    string
	RelPath string
}

// MediaReconcile は worker のメディア処理入力、media_assets の reconcile と profile 検証の読み取り契約である。
type MediaReconcile interface {
	// GetActiveOriginalMediaAsset はメディア処理の入力に使う active な原本を返す。
	GetActiveOriginalMediaAsset(ctx context.Context, recordingID int64) (MediaAsset, error)
	// HasActiveThumbnailMediaAsset は active なサムネイルがあれば true を返す。
	HasActiveThumbnailMediaAsset(ctx context.Context, recordingID int64) (bool, error)
	// GetEncodedAssetForPlan はエンコード計画の比較に使う encoded 行を、凍結された keep 区間を含めて state を問わず返す。
	// 行と区間は 1 つの読み取りで同じ時点の値を返す。無ければ ErrNotFound を返す。
	GetEncodedAssetForPlan(ctx context.Context, recordingID int64, profile string) (EncodedAssetObservation, error)
	// EncodedAssetRebuildRequestMatches は要求が残り、要求時の keep 区間が一致するかを返す。
	EncodedAssetRebuildRequestMatches(ctx context.Context, recordingID int64, profile string, keep []chapters.Range) (bool, error)
	// ListMissingEncodeProfiles は未生成 encoded の候補をページ単位で返す。
	ListMissingEncodeProfiles(ctx context.Context, params MissingEncodeProfilesParams) ([]MissingEncodeProfile, error)
	// ListEncodedAssetRebuildRequests は満たされていない作り直し要求を返す。
	ListEncodedAssetRebuildRequests(ctx context.Context, recordingID *int64) ([]EncodeAssetRebuildRequest, error)
	// ListUnsatisfiableEncodeProfiles は現在の設定に無い desired profile と録画数を返す。
	ListUnsatisfiableEncodeProfiles(ctx context.Context, knownProfiles []string) ([]EncodeProfileCount, error)
	// ListCutAwaitingReview は確認待ちの cut profile と録画数を返す。
	ListCutAwaitingReview(ctx context.Context, profiles []string) ([]EncodeProfileCount, error)
	// ListEncodedMeaningEvidence は過去の encoded と作り直し要求から意味の証拠を返す。
	ListEncodedMeaningEvidence(ctx context.Context) ([]EncodedMeaningEvidence, error)
	// ListMissingThumbnailRecordings はサムネイルが無い録画をページ単位で返す。
	ListMissingThumbnailRecordings(ctx context.Context, page MediaReconcilePage) ([]int64, error)
	// ListThumbnailReselectCandidates は既存サムネイルの再選択候補を返す。
	ListThumbnailReselectCandidates(ctx context.Context, page MediaReconcilePage) ([]ThumbnailReselectCandidate, error)
	// ListMissingSeekTilesRecordings はシークタイルが無い録画をページ単位で返す。
	ListMissingSeekTilesRecordings(ctx context.Context, page MediaReconcilePage) ([]int64, error)
	// GetActiveOriginalForTSScan は TS scan の対象になる active 原本を返す。
	GetActiveOriginalForTSScan(ctx context.Context, recordingID int64) (MediaAsset, error)
	// ListMissingTSScanRecordings は TS scan が必要な録画をページ単位で返す。
	ListMissingTSScanRecordings(ctx context.Context, afterRecordingID int64, rowLimit int32) ([]int64, error)
	// FindInvalidMediaAssetRelPath は予約されたサイト名空間の外にある原本または encoded を返す。
	// 違反が無ければ nil, nil を返す。
	FindInvalidMediaAssetRelPath(ctx context.Context) (*MediaAssetRelPathViolation, error)
}

// EncodeQueueParams は API が encode 待機列を作る条件を表す。
type EncodeQueueParams struct {
	KnownProfiles []string
	CutProfiles   []string
	AttemptLimit  int32
}

// EncodeQueueEntry は待機中または実行中の encoded 1 件を表す。
type EncodeQueueEntry struct {
	RecordingID int64
	Running     bool
}

// RecordingDropStat は原本の PID 別ドロップ統計を表す。
type RecordingDropStat struct {
	Pid       int32
	Packets   int64
	Drops     int64
	Errors    int64
	Scrambled int64
	PidType   *string
}

// RecordingDropPosition は録画内でドロップを観測した位置を表す。
type RecordingDropPosition struct {
	Pid        int32
	ByteOffset int64
	ElapsedMs  *int64
}

// MediaReader は API がメディア関連の読み取りに使う契約である。
type MediaReader interface {
	// ListEncodeQueue は現在の設定で未生成の encoded と実行状態を返す。
	ListEncodeQueue(ctx context.Context, params EncodeQueueParams) ([]EncodeQueueEntry, error)
	// ListRecordingDropStats は録画の PID 別ドロップ統計を返す。
	ListRecordingDropStats(ctx context.Context, recordingID int64) ([]RecordingDropStat, error)
	// ListRecordingDropPositions は録画のドロップ観測位置を PID 別に返す。
	ListRecordingDropPositions(ctx context.Context, recordingID int64) ([]RecordingDropPosition, error)
}
