package store

import (
	"context"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
)

// MediaAsset は配信または解析に使う確定済みメディアアセットを表す。
type MediaAsset struct {
	ID        int64
	Store     string
	RelPath   string
	SizeBytes int64
}

// OriginalVODTarget はサイト内で配信できる原本を表す。
type OriginalVODTarget struct {
	MediaAsset
	Site string
}

// RecordingHLSInputKind は世代付き録画 HLS の入力種別を表す。
type RecordingHLSInputKind uint8

const (
	// RecordingHLSInputOriginal は ingest が commit した原本を表す。
	RecordingHLSInputOriginal RecordingHLSInputKind = iota
	// RecordingHLSInputTemp は ingest が書き込み中の temp を表す。
	RecordingHLSInputTemp
)

// RecordingHLSInputTarget は世代付き録画 HLS が読む入力と DB 上の状態を表す。
type RecordingHLSInputTarget struct {
	Kind             RecordingHLSInputKind
	ID               int64
	RelPath          string
	StoreName        string
	SizeBytes        int64
	TempFinalized    bool
	IngestGeneration int64
	StartedAt        *time.Time
	RecordStatus     string
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
	// 録画完了済み・ごみ箱/ purge 前・active original のみを開始可能とし、site は URL の
	// site-scoped streamer と照合する。既存セッションの再取得では使わず、セッション開始前と
	// rel_path lock 取得後に呼ぶ。
	GetOriginalVODTarget(ctx context.Context, recordingID int64, site string) (OriginalVODTarget, error)
	// GetRecordingHLSTarget は配信可能な原本か ingest temp と世代を返す。
	GetRecordingHLSTarget(ctx context.Context, recordingID int64) (RecordingHLSInputTarget, error)
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

// MissingEncodeProfile は不足または作り直しが必要な録画とプロファイルを表す。
type MissingEncodeProfile struct {
	RecordingID   int64
	Profile       string
	IsRebuild     bool
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
	ID                int64
	RelPath           string
	State             string
	DerivedGeneration int64
	KeepRanges        []chapters.Range
}

// EncodedPublishState はエンコード公開の判断に使う、同じ時点の DB 状態を表す。
type EncodedPublishState struct {
	Desired            bool
	HasRebuildRequest  bool
	RebuildRequestKeep []chapters.Range
	ChapterOwned       bool
	CurrentCutKeep     []chapters.Range
	EncodedAsset       *EncodedAssetObservation
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
// CMRanges / UserSpans / EncodedAssets は、実装が DB の集約結果を型へ戻した値である。
// 戻せない行があれば、ListThumbnailReselectCandidates がその呼び出し全体を失敗させる。
type ThumbnailReselectCandidate struct {
	RecordingID           int64
	Trashed               bool
	ProgramDurationMs     int64
	Detected              bool
	Owned                 bool
	CMRanges              []chapters.Range
	UserSpans             []chapters.Span
	OriginalMediaAssetID  int64
	OriginalRelPath       string
	EncodedAssets         []ThumbnailEncodedAsset
	ThumbnailMediaAssetID *int64
	ThumbnailRelPath      *string
	ThumbnailGeneration   int64
	SeekMs                *int64
}

// MediaAssetRelPathViolation はサイト名空間を満たさない原本または encoded を表す。
type MediaAssetRelPathViolation struct {
	ID      int64
	Kind    string
	RelPath string
}

// RecordingEncodeTimes はエンコード締切の計算に使う録画時刻を表す。
type RecordingEncodeTimes struct {
	StartedAt         *time.Time
	EndedAt           *time.Time
	ProgramDurationMs int64
}

// RecordingNotifySubject はエンコード通知に載せる録画の識別情報を表す。
type RecordingNotifySubject struct {
	Site  string
	Title string
}

// MediaReconcile は worker のメディア処理入力、media_assets の reconcile と profile 検証の読み取り契約である。
type MediaReconcile interface {
	// GetCurrentCutKeep は確認済みチャプターから現在の keep 区間を導出する。
	// 所有状態と区間は同じ時点の 1 回の読み取りで返す。
	// 確認されていなければ owned=false、purged の録画なら ErrNotFound を返す。
	GetCurrentCutKeep(ctx context.Context, recordingID int64) (keep []chapters.Range, owned bool, err error)
	// GetRecordingEncodeTimes はエンコード締切の計算に使う録画時刻を返す。
	// 録画が無ければ ErrNotFound を返す。
	GetRecordingEncodeTimes(ctx context.Context, recordingID int64) (RecordingEncodeTimes, error)
	// GetRecordingNotifySubject はエンコード通知に載せる局と番組名を返す。
	// purged の録画も返し、録画が無ければ ErrNotFound を返す。
	GetRecordingNotifySubject(ctx context.Context, recordingID int64) (RecordingNotifySubject, error)
	// GetActiveOriginalMediaAsset はメディア処理の入力に使う active な原本を返す。
	GetActiveOriginalMediaAsset(ctx context.Context, recordingID int64) (MediaAsset, error)
	// HasActiveThumbnailMediaAsset は active なサムネイルがあれば true を返す。
	HasActiveThumbnailMediaAsset(ctx context.Context, recordingID int64) (bool, error)
	// HasActiveSeekTilesMediaAsset は active なシークタイルがあれば true を返す。
	HasActiveSeekTilesMediaAsset(ctx context.Context, recordingID int64) (bool, error)
	// GetEncodedPublishState は desired、作り直し要求、encoded 行と必要なカット版の状態を、
	// ロックを取らずに同じ時点の値として返す（エンコード前の判定用）。
	// cut=true なら encoded 行の凍結 keep も読む。includeCurrentCut=true かつ profile が
	// desired のときだけ、現在の章所有と keep 区間も読む（要求に由来しないカット版ジョブ用）。
	GetEncodedPublishState(ctx context.Context, recordingID int64, profile string, cut, includeCurrentCut bool) (EncodedPublishState, error)
	// ListMissingEncodeProfiles は不足と満たされていない作り直しを合わせた候補をページ単位で返す。
	ListMissingEncodeProfiles(ctx context.Context, params MissingEncodeProfilesParams) ([]MissingEncodeProfile, error)
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
	// GetDerivedMediaAssetGeneration は派生アセットの現在世代を返す。
	// その kind の行が無ければ found=false を返す。
	GetDerivedMediaAssetGeneration(ctx context.Context, recordingID int64, kind string) (generation int64, found bool, err error)
	// ListMissingSeekTilesRecordings はシークタイルが無い録画をページ単位で返す。
	ListMissingSeekTilesRecordings(ctx context.Context, page MediaReconcilePage) ([]int64, error)
	// FindInvalidMediaAssetRelPath は予約されたサイト名空間の外にある原本または encoded を返す。
	// 違反が無ければ nil, nil を返す。
	FindInvalidMediaAssetRelPath(ctx context.Context) (*MediaAssetRelPathViolation, error)
}

// IngestPrecheck は転送前の冪等性・衝突確認に使う読み取り契約である。
type IngestPrecheck interface {
	// HasOriginalMediaAsset は kind='original' の media_asset 行があれば true を返す。
	// state は問わない。行の存在が原本のコミットを表す。
	HasOriginalMediaAsset(ctx context.Context, recordingID int64) (bool, error)
	// GetLiveMediaAssetRecordingID は削除済みでない行の recording_id を返す。
	// 転送前のヒントであり、一意性の最終判定ではない。述語は部分一意索引と同じで、
	// state='deleting' の行も衝突に数える。
	GetLiveMediaAssetRecordingID(ctx context.Context, relPath string) (int64, error)
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
