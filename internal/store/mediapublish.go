package store

import (
	"context"
	"errors"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
)

// ErrStalePublishPlan は公開中に encoded の計画が古くなったことを表す。
var ErrStalePublishPlan = errors.New("encoded publish plan is stale")

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
	ObservedRelPath   string
	Cut               bool
	KeepRanges        []chapters.Range
	HasRebuildRequest bool
	RequestedKeep     []chapters.Range
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
}

// SeekTilesPublishInput はシークタイルを公開する判断材料を表す。
type SeekTilesPublishInput struct {
	RecordingID       int64
	RelPath           string
	SizeBytes         int64
	DerivedGeneration int64
}

// PendingMediaAssetCopy describes a derived object whose durable key is reserved
// before an object-store PUT begins.
type PendingMediaAssetCopy struct {
	RecordingID       int64
	Kind              string
	Profile           *string
	RelPath           string
	SizeBytes         int64
	DerivedGeneration int64
}

// DerivedMediaPublisher publishes encoded video, thumbnails, and seek tiles.
type DerivedMediaPublisher interface {
	// PreparePendingMediaAssetCopy commits the logical asset and pending physical-copy row before upload.
	PreparePendingMediaAssetCopy(ctx context.Context, input PendingMediaAssetCopy) error
	// PublishEncoded rechecks encode intent and promotes the staged copy when publish succeeds.
	PublishEncoded(ctx context.Context, input EncodedPublishInput, publish func() error) (published bool, err error)
	// PublishThumbnail rechecks thumbnail eligibility and promotes the staged copy.
	PublishThumbnail(ctx context.Context, input ThumbnailPublishInput, publish func() error) (published bool, err error)
	// PublishSeekTiles rechecks seek-tile eligibility and promotes the staged copy.
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
	// GetIngestTempState は進捗行の temp パスと世代を返す。行が無ければ nil, nil を返す。
	GetIngestTempState(ctx context.Context, recordingID int64) (*IngestTempState, error)
	// NextIngestTempGeneration は新しい temp ファイルに割り当てる世代を採番する。
	NextIngestTempGeneration(ctx context.Context) (int64, error)
	// ClearIngestProgress は録画の ingest 進捗行を削除する。行が無くても成功する。
	ClearIngestProgress(ctx context.Context, recordingID int64) error
	// CommitOriginal は原本・進捗の削除・凍結方針を一つの tx で確定する。
	// 進捗行が必要である（原本の世代を進捗行から写す）。行が無ければ原本を作らずエラーを返す。
	// 同じ rel_path の原本が既にあれば ErrConflict を返し、publish は呼ばない。
	// resolvePolicy は同じ tx で読んだ予約・上書き・意図に対して 1 回だけ呼ぶ。
	CommitOriginal(ctx context.Context, input CommitOriginalInput, resolvePolicy ResolveEncodePolicy, publish func() error) error
	// PublishEncoded は試行・desired・作り直し要求・cut 区間を再確認して encoded を公開する。
	// 同じ録画の版の削除・事後追加と直列化する。cut の公開はさらに、チャプター編集・作り直し要求の書き込みとも直列化する。
	// 計画が古くなっていたら ErrStalePublishPlan を返す。公開しないと決めたときは published=false を返す。
	PublishEncoded(ctx context.Context, input EncodedPublishInput, publish func() error) (published bool, err error)
	// PublishThumbnail は録画とサムネイルの状態を再確認して公開する。
	// 同じ録画のごみ箱移動・完全削除と直列化し、ごみ箱か削除済みの録画は公開しない。
	// 置き換えは、同じ録画への別のサムネイル公開とも直列化する。
	PublishThumbnail(ctx context.Context, input ThumbnailPublishInput, publish func() error) (published bool, err error)
	// PublishSeekTiles は録画とシークタイルの状態を再確認して公開する。
	// 同じ録画のごみ箱移動・完全削除と直列化し、ごみ箱か削除済みの録画は公開しない。
	PublishSeekTiles(ctx context.Context, input SeekTilesPublishInput, publish func() error) (published bool, err error)
	// RegisterInPlace は既存ファイルを指す録画とアセットの行を一つの tx で登録する。
	// 同じ rel_path の行が既にあれば、その録画を再利用して冪等に登録する。
	RegisterInPlace(ctx context.Context, input InPlaceInput) (InPlaceResult, error)
	// UpsertIngestProgress は転送中の進捗を記録する。tx を持たず、失敗は ingest を止めない。
	UpsertIngestProgress(ctx context.Context, progress IngestProgress) error
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
	// ObservedRelPath は行の rel_path（state は問わない）。行が無ければ ""。
	ObservedRelPath string
	// Replaces は cut の公開が active な旧版を置き換えることを表す。
	// tombstone や deleting の行は自分のものではないので含めない。
	Replaces bool
	// Skip は公開を飛ばして成功扱いにする判定である。
	Skip bool
	// SkipReason は公開を飛ばす理由で、Skip が false なら空である。
	SkipReason EncodedPublishSkipReason
	// Stale は計画が古いことを表す。行が active のまま rel_path だけが観測時から進み、
	// 区間が自分の keep と違う（cut でだけ起きる）。
	// 成功扱いで飛ばすと新しい keep が黙って消えるので、公開せずに計画をやり直す。
	Stale bool
}

// PlanEncodedPublish はエンコード公開の状態を判定する純粋関数である。
// previous は worker が先に観測した rel_path で、nil なら初回観測として行の進みを判定しない。
// 区間の比較は量子化後の値どうしで行う。keep は chapters.Derive を通った時点で量子化済みで、
// 凍結側は書き込み時に量子化して入れてある。worker のエンコード前の判定と公開 tx が同じ判定を使う。
func PlanEncodedPublish(in EncodedPublishInput, state EncodedPublishState, previous *string) EncodedPublishPlan {
	var p EncodedPublishPlan
	active := state.EncodedAsset != nil && state.EncodedAsset.State == "active"
	if state.EncodedAsset != nil {
		p.ObservedRelPath = state.EncodedAsset.RelPath
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
	moved := previous != nil && *previous != p.ObservedRelPath
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
