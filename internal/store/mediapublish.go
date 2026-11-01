package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/chapters"
)

// ErrStalePublishPlan は公開中に encoded または派生物の計画が古くなったことを表す。
var ErrStalePublishPlan = errors.New("encoded publish plan is stale")

// ErrPendingMediaAssetCopyMissing は公開前に pending copy の live 行が無くなったことを表す。
var ErrPendingMediaAssetCopyMissing = errors.New("pending media asset copy is no longer live")

// ErrGenerationAdvanced は別の pending または公開済みコピーが観測した世代を使用済みにしたことを表す。
var ErrGenerationAdvanced = errors.New("derived media generation advanced")

// ErrIngestCheckpointLeaseHeld は別の ingest worker が同じ録画の writer lease を持つことを表す。
var ErrIngestCheckpointLeaseHeld = errors.New("ingest checkpoint lease is held")

// ErrIngestCheckpointLeaseLost は writer lease の期限切れまたは所有者・世代の変更を表す。
var ErrIngestCheckpointLeaseLost = errors.New("ingest checkpoint lease lost")

// IngestProgress は原本の転送中に観測した進捗を表す。
type IngestProgress struct {
	RecordingID    int64
	WrittenBytes   int64
	ExpectedBytes  *int64
	TempRelPath    string
	TempGeneration int64
}

// IngestTempState は進捗行が指す temp ファイルと世代を表す。
type IngestTempState struct {
	TempRelPath    string
	TempGeneration int64
}

// IngestCheckpoint は DB に保存した ingest writer の handoff 状態を表す。
type IngestCheckpoint struct {
	WriterGeneration int64
	Resumed          bool
	Offset           int64
	HashState        []byte
}

// IngestWriterState は mirakc record に対応する writer 世代と現在の HLS 表示先を表す。
type IngestWriterState struct {
	RecordingID      int64
	WriterGeneration int64
	TempRelPath      string
	HasProgress      bool
}

// IngestWriterStateReader は ingest orphan 回収が writer 世代を調べる契約である。
type IngestWriterStateReader interface {
	GetIngestWriterState(ctx context.Context, site, recordID string) (IngestWriterState, bool, error)
}

// EncodeProgress は通知するエンコード進捗を表す。
type EncodeProgress struct {
	RecordingID int64
	Profile     string
	Progress    float64
}

// EncodeProgressReporter はエンコード進捗を通知する。
type EncodeProgressReporter interface {
	// ReportEncodeProgress は encode-progress 通知を送る。
	ReportEncodeProgress(ctx context.Context, progress EncodeProgress) error
}

// CommitOriginalInput は原本アセットの公開に必要な値を表す。
type CommitOriginalInput struct {
	RecordingID int64
	RelPath     string
	SizeBytes   int64
	SHA256      []byte
	// WriterOwner と WriterGeneration は、0 世代なら従来の登録経路として省略できる。
	// ingest worker は両方を指定し、link と DB commit を同じ live lease に fenced する。
	WriterOwner      uuid.UUID
	WriterGeneration int64
}

// ReservationPolicyInputs は予約・意図・上書きから方針を解決する入力を表す。
type ReservationPolicyInputs struct {
	ProgramID    int64
	Base         []byte
	Overrides    []byte
	IntentAction *string
}

// EncodePolicyInputs は保持方針を解決するために必要な DB 状態を表す。
type EncodePolicyInputs struct {
	RecordingID int64
	Source      string
	Site        string
	NetworkID   int32
	ServiceID   int32
	EventID     int32
	Reservation *ReservationPolicyInputs
}

// EncodePolicySnapshot は原本の commit と同時に凍結する方針を表す。
type EncodePolicySnapshot struct {
	KeepOriginal   string
	EncodeProfiles []string
	CMDetect       bool
}

// ResolveEncodePolicy は DB 状態から凍結方針を計算する純粋関数である。
type ResolveEncodePolicy func(EncodePolicyInputs) (EncodePolicySnapshot, error)

// EncodedPublishInput は encoded アセットを公開する判断材料を表す。
type EncodedPublishInput struct {
	RecordingID       int64
	Profile           string
	Attempt           int32
	RelPath           string
	SizeBytes         int64
	DerivedGeneration int64
	// ObservedGeneration は worker が観測した論理行の derived_generation で、nil は観測時に行が無かったことを表す。
	ObservedGeneration *int64
	Cut                bool
	KeepRanges         []chapters.Range
	HasRebuildRequest  bool
	RequestedKeep      []chapters.Range
	// SHA256 は公開するオブジェクト全体の SHA-256。Put 前に計算し、commit の tx で論理行へ書く。
	SHA256 []byte
	// PendingCopy は、このコピーを PreparePendingMediaAssetCopy で先に commit 済みであることを表す。
	// true なら commit の tx で pending を active にし、行を新しく書かない。
	PendingCopy bool
}

// ThumbnailObservation は再選択前に読んだサムネイルを表す。
type ThumbnailObservation struct {
	ID                int64
	RelPath           string
	SeekMs            *int64
	DerivedGeneration int64
}

// ThumbnailPublishInput はサムネイルを公開する判断材料を表す。
type ThumbnailPublishInput struct {
	RecordingID       int64
	RelPath           string
	SizeBytes         int64
	SeekMs            int64
	InputAssetID      int64
	InputStore        string
	DerivedGeneration int64
	Expected          *ThumbnailObservation
	// SHA256 と PendingCopy は EncodedPublishInput と同じ意味である。
	SHA256      []byte
	PendingCopy bool
}

// SeekTilesPublishInput はシークタイルを公開する判断材料を表す。
type SeekTilesPublishInput struct {
	RecordingID       int64
	RelPath           string
	SizeBytes         int64
	DerivedGeneration int64
	// SHA256 と PendingCopy は EncodedPublishInput と同じ意味である。
	SHA256      []byte
	PendingCopy bool
}

// PendingMediaAssetCopy は、派生物の Store.Put 前に commit する pending のコピー行を表す。
// 公開の tx が同じ行を active にするまで、読み手には現行のコピーとして見えない。
type PendingMediaAssetCopy struct {
	RecordingID       int64
	Kind              string
	Profile           *string
	RelPath           string
	SizeBytes         int64
	DerivedGeneration int64
	// ReserveNewGeneration は既存のどのコピーも使っていない世代の予約を要求する。
	// 出力の内容が毎回変わる encode は、公開前に止まった試行の key を再利用してはならない。
	ReserveNewGeneration bool
}

// DerivedMediaPublisher は encoded・サムネイル・シークタイルを派生物置き場へ公開する契約である。
type DerivedMediaPublisher interface {
	// PreparePendingMediaAssetCopy は Store.Put の前に pending のコピー行を commit する。
	// 世代がすでに使われていれば ErrGenerationAdvanced を返す。
	PreparePendingMediaAssetCopy(ctx context.Context, input PendingMediaAssetCopy) error
	// HasLiveMediaAssetCopy は置き場に未削除のコピー行があるかを返す。
	HasLiveMediaAssetCopy(ctx context.Context, relPath string) (bool, error)
	// PublishEncoded は encode の計画を再確認し、Put 済み PendingCopy を active にする。
	PublishEncoded(ctx context.Context, input EncodedPublishInput, publish func() error) (published bool, err error)
	// PublishThumbnail はサムネイルの資格を再確認し、Put 済み PendingCopy を active にする。
	PublishThumbnail(ctx context.Context, input ThumbnailPublishInput, publish func() error) (published bool, err error)
	// PublishSeekTiles はシークタイルの資格を再確認し、Put 済み PendingCopy を active にする。
	PublishSeekTiles(ctx context.Context, input SeekTilesPublishInput, publish func() error) (published bool, err error)
}

// InPlaceRecording は既存ファイル群を登録する録画メタデータを表す。
// Site と放送 identity（NetworkID・ServiceID・EventID）が冪等性キーである。
type InPlaceRecording struct {
	Source            string
	Site              string
	NetworkID         int32
	ServiceID         int32
	EventID           int32
	ServiceName       string
	ChannelType       string
	Channel           string
	Title             string
	ProgramStartAt    time.Time
	ProgramDurationMs int64
	Status            string
	StartedAt         *time.Time
	EndedAt           *time.Time
}

// InPlaceAsset は登録する既存ファイルを表す。RelPath は media_dir からの相対パスである。
// Profile は Kind が encoded のときに限り必須で、original・thumbnail・seek_tiles は持たない。
type InPlaceAsset struct {
	Kind    string
	Profile *string
	RelPath string
	// SizeBytes は inplace.Register がファイルを計測して埋める。呼び出し側が渡した値は使わない。
	SizeBytes int64
}

// InPlaceInput は一つの録画と、その録画に属する既存ファイル群を表す。
// Assets は空にしない。アセットを持たない録画には復旧上の意味がない。
type InPlaceInput struct {
	Recording InPlaceRecording
	Assets    []InPlaceAsset
}

// InPlaceResult は登録した行の識別子を表す。
type InPlaceResult struct {
	RecordingID int64
	AssetIDs    []int64
}

// MediaPublish は原本・派生物の公開と in-place 登録を永続化する。
//
// publish 関数を取るメソッドは、すべて次の規則に従う。
// 呼び出し側は rel_path のファイルロックを持って呼ぶ。store はファイルシステムを知らない。
// store は検査と行の書き込みをすべて終えた後、commit の前に publish を 1 回だけ呼ぶ。
// publish は rename と fsync でファイルを公開する。検査で公開しないと決めたときは呼ばない。
// publish がエラーを返したら、store は rollback してそのエラーを返す。
// publish が成功した後に commit が失敗したら、store はエラーを返し、ファイルは行を持たない孤児として残る。
// 呼び出し側はそのファイルを消さず、孤児の監査に任せる。
type MediaPublish interface {
	// AcquireIngestCheckpoint は録画の writer lease を取り、前の clean handoff を返す。
	// live lease が別にある場合は acquired=false を返す。unclean handoff では新世代から始める。
	AcquireIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID, ttl time.Duration) (checkpoint IngestCheckpoint, acquired bool, err error)
	// ResetIngestCheckpoint は、handed_off のファイルと hash state が一致しないときに新 writer 世代へ進む。
	ResetIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID, writerGeneration int64) (int64, error)
	// SaveIngestCheckpoint は同期・close 済み temp の offset と hash state を clean handoff として保存する。
	SaveIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID, writerGeneration int64, offset int64, hashState []byte) error
	// SaveIngestFinalHandoff は全量の temp を lease を保ったまま保存する。link より前に呼ぶ。
	SaveIngestFinalHandoff(ctx context.Context, recordingID int64, owner uuid.UUID, writerGeneration int64, offset int64, hashState []byte) error
	// ReleaseIngestCheckpoint は失敗で終わる writer の lease を解放する。handed_off には触らない。
	ReleaseIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID) error
	// DiscardIngestCheckpoint は現在の writer が所有する進捗行を削除し、checkpoint は世代を進めて lease を切る。
	DiscardIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID, writerGeneration int64) (bool, error)
	// GetIngestTempState は進捗行の temp パスと世代を返す。行が無ければ nil, nil を返す。
	GetIngestTempState(ctx context.Context, recordingID int64) (*IngestTempState, error)
	// NextIngestTempGeneration は新しい temp ファイルに割り当てる世代を採番する。
	NextIngestTempGeneration(ctx context.Context) (int64, error)
	// ClearIngestProgress は録画の ingest 進捗行を削除する。行が無くても成功する。
	ClearIngestProgress(ctx context.Context, recordingID int64) error
	// CommitOriginal は原本の SHA-256・進捗の削除・凍結方針を一つの tx で確定する。
	// 進捗行が必要である（原本の世代を進捗行から写す）。行が無ければ原本を作らずエラーを返す。
	// 同じ rel_path の原本が既にあれば ErrConflict を返し、publish は呼ばない。
	// resolvePolicy は同じ tx で読んだ予約・上書き・意図に対して 1 回だけ呼ぶ。
	CommitOriginal(ctx context.Context, input CommitOriginalInput, resolvePolicy ResolveEncodePolicy, publish func() error) error
	// PublishEncoded は試行・desired・作り直し要求・cut 区間を再確認し、ファイルの SHA-256 と encoded を公開する。
	// 同じ録画の版の削除・事後追加と直列化する。cut の公開はさらに、チャプター編集・作り直し要求の書き込みとも直列化する。
	// 計画が古くなっていたら ErrStalePublishPlan を返す。公開しないと決めたときは published=false を返す。
	PublishEncoded(ctx context.Context, input EncodedPublishInput, publish func() error) (published bool, err error)
	// PublishThumbnail は録画とサムネイルの状態を再確認し、ファイルの SHA-256 とともに公開する。
	// 同じ録画のごみ箱移動・完全削除と直列化し、ごみ箱か削除済みの録画は公開しない。
	// 置き換えは、同じ録画への別のサムネイル公開とも直列化する。
	PublishThumbnail(ctx context.Context, input ThumbnailPublishInput, publish func() error) (published bool, err error)
	// PublishSeekTiles は録画とシークタイルの状態を再確認し、ファイルの SHA-256 とともに公開する。
	// 同じ録画のごみ箱移動・完全削除と直列化し、ごみ箱か削除済みの録画は公開しない。
	PublishSeekTiles(ctx context.Context, input SeekTilesPublishInput, publish func() error) (published bool, err error)
	// RegisterInPlace は既存ファイルを指す録画とアセットの行を一つの tx で登録する。
	// 同じ rel_path の行が既にあれば、その録画を再利用して冪等に登録する。
	RegisterInPlace(ctx context.Context, input InPlaceInput) (InPlaceResult, error)
	// UpsertIngestProgress は転送中の進捗を記録する。tx を持たず、失敗は ingest を止めない。
	UpsertIngestProgress(ctx context.Context, progress IngestProgress) error
	// UpdateIngestProgress は writer lease を検証して進捗を更新する。
	// 新しい writer の offset が見えている古い offset に追いつく前は、temp path と written_bytes の組を維持する。
	UpdateIngestProgress(ctx context.Context, progress IngestProgress, owner uuid.UUID, writerGeneration int64, ttl time.Duration) error
}

// EncodedPublishSkipReason はエンコード公開を飛ばす理由を表す。
type EncodedPublishSkipReason string

const (
	// EncodedPublishSkipDesiredProfileExcluded は profile が現在の desired から外れたことを表す。
	EncodedPublishSkipDesiredProfileExcluded EncodedPublishSkipReason = "desired_profile_excluded"
	// EncodedPublishSkipRebuildRequestMissing は要求に束縛されたジョブの要求行が無いことを表す。
	EncodedPublishSkipRebuildRequestMissing EncodedPublishSkipReason = "rebuild_request_missing"
	// EncodedPublishSkipRebuildRequestOverwritten は要求行の keep がジョブの keep から変わったことを表す。
	EncodedPublishSkipRebuildRequestOverwritten EncodedPublishSkipReason = "rebuild_request_overwritten"
	// EncodedPublishSkipCutOwnershipMissing は通常のカット版ジョブが章の所有を失ったことを表す。
	EncodedPublishSkipCutOwnershipMissing EncodedPublishSkipReason = "cut_ownership_missing"
	// EncodedPublishSkipCutKeepChanged は通常のカット版ジョブと現在の keep が異なることを表す。
	EncodedPublishSkipCutKeepChanged EncodedPublishSkipReason = "cut_keep_changed"
	// EncodedPublishSkipAssetSatisfied は active asset が現在の要求を満たすことを表す。
	EncodedPublishSkipAssetSatisfied EncodedPublishSkipReason = "asset_satisfied"
	// EncodedPublishSkipAssetAdvanced は前回観測後に行が進み、既に active ではないことを表す。
	EncodedPublishSkipAssetAdvanced EncodedPublishSkipReason = "asset_advanced"
	// EncodedPublishSkipActiveCutProtected は要求に由来しないジョブが active なカット版を置き換えないことを表す。
	EncodedPublishSkipActiveCutProtected EncodedPublishSkipReason = "active_cut_protected"
)

// EncodedPublishPlan は encoded 公開前に観測した DB 状態から導いた判断である。
type EncodedPublishPlan struct {
	// ObservedGeneration は行の derived_generation（active かは問わない）。nil は行が無いことを表す。
	ObservedGeneration *int64
	// Replaces は cut の公開が active な旧版を置き換えることを表す。
	// tombstone や deleting の行は自分のものではないので含めない。
	Replaces bool
	// Skip は公開を飛ばして成功扱いにする判定である。
	Skip bool
	// SkipReason は公開を飛ばす理由で、Skip が false なら空である。
	SkipReason EncodedPublishSkipReason
	// Stale は計画が古いことを表す。行が active のまま derived_generation だけが観測時から進み、
	// 区間が自分の keep と違う（cut でだけ起きる）。
	// 成功扱いで飛ばすと新しい keep が黙って消えるので、公開せずに計画をやり直す。
	Stale bool
}

// PlanEncodedPublish はエンコード公開の状態を判定する純粋関数である。
// compareObserved が true のとき、in.ObservedGeneration（worker が先に観測した値。nil は行が無かった）と
// 今の観測が違えば行が進んだと判定する。false なら初回観測として行の進みを判定しない。
// 区間の比較は量子化後の値どうしで行う。keep は chapters.Derive を通った時点で量子化済みで、
// 凍結側は書き込み時に量子化して入れてある。worker のエンコード前の判定と公開 tx が同じ判定を使う。
func PlanEncodedPublish(in EncodedPublishInput, state EncodedPublishState, compareObserved bool) EncodedPublishPlan {
	var p EncodedPublishPlan
	active := state.EncodedAsset != nil && state.EncodedAsset.Active
	if state.EncodedAsset != nil {
		generation := state.EncodedAsset.DerivedGeneration
		p.ObservedGeneration = &generation
	}
	p.Replaces = in.Cut && active

	if !state.Desired {
		p.Skip = true
		p.SkipReason = EncodedPublishSkipDesiredProfileExcluded
		return p
	}
	if in.HasRebuildRequest {
		if !state.HasRebuildRequest {
			p.Skip = true
			p.SkipReason = EncodedPublishSkipRebuildRequestMissing
			return p
		}
		if !chapters.SameRanges(state.RebuildRequestKeep, in.RequestedKeep) {
			p.Skip = true
			p.SkipReason = EncodedPublishSkipRebuildRequestOverwritten
			return p
		}
	} else if in.Cut {
		if !state.ChapterOwned {
			p.Skip = true
			p.SkipReason = EncodedPublishSkipCutOwnershipMissing
			return p
		}
		if !chapters.SameRanges(state.CurrentCutKeep, in.KeepRanges) {
			p.Skip = true
			p.SkipReason = EncodedPublishSkipCutKeepChanged
			return p
		}
	}

	committed := active
	if committed && in.Cut {
		committed = chapters.SameRanges(state.EncodedAsset.KeepRanges, in.KeepRanges)
	}
	moved := compareObserved && !sameGeneration(in.ObservedGeneration, p.ObservedGeneration)
	p.Stale = moved && !committed && active
	if p.Stale {
		return p
	}
	if committed {
		p.Skip = true
		p.SkipReason = EncodedPublishSkipAssetSatisfied
		return p
	}
	if moved {
		p.Skip = true
		p.SkipReason = EncodedPublishSkipAssetAdvanced
		return p
	}
	if in.Cut && !in.HasRebuildRequest && active {
		p.Skip = true
		p.SkipReason = EncodedPublishSkipActiveCutProtected
	}
	return p
}

// sameGeneration は 2 つの観測世代を比べる。両方 nil（行が無い）なら等しく、片方だけ nil なら異なる。
func sameGeneration(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
