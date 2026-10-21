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
	RecordingID   int64
	WrittenBytes  int64
	ExpectedBytes *int64
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
	ObservedRelPath   string
	Cut               bool
	KeepRanges        []chapters.Range
	HasRebuildRequest bool
	RequestedKeep     []chapters.Range
}

// ThumbnailObservation は再選択前に読んだサムネイルを表す。
type ThumbnailObservation struct {
	ID      int64
	RelPath string
	SeekMs  *int64
}

// ThumbnailPublishInput はサムネイルを公開する判断材料を表す。
type ThumbnailPublishInput struct {
	RecordingID  int64
	RelPath      string
	SizeBytes    int64
	SeekMs       int64
	InputAssetID int64
	Expected     *ThumbnailObservation
}

// SeekTilesPublishInput はシークタイルを公開する判断材料を表す。
type SeekTilesPublishInput struct {
	RecordingID int64
	RelPath     string
	SizeBytes   int64
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
	// ClearIngestProgress は録画の ingest 進捗行を削除する。行が無くても成功する。
	ClearIngestProgress(ctx context.Context, recordingID int64) error
	// CommitOriginal は原本・進捗の削除・凍結方針を一つの tx で確定する。
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

// EncodedPublishPlan は encoded 行の 1 回の観測から導いた、公開の判断である。
type EncodedPublishPlan struct {
	// ObservedRelPath は行の rel_path（state は問わない）。行が無ければ ""。
	ObservedRelPath string
	// Replaces は cut の公開が active な旧版を置き換えることを表す。
	// tombstone や deleting の行は自分のものではないので含めない。
	Replaces bool
	// Skip は公開を飛ばして成功扱いにする判定である。
	// 行が active で（cut なら）凍結区間が keep と一致する、または
	// 行が active でないまま rel_path が観測時から進んだときに立つ。
	Skip bool
	// Stale は計画が古いことを表す。行が active のまま rel_path だけが観測時から進み、
	// 区間が自分の keep と違う（cut でだけ起きる）。
	// 成功扱いで飛ばすと新しい keep が黙って消えるので、公開せずに計画をやり直す。
	Stale bool
}

// PlanEncodedPublish は encoded 行の観測 asset（行なしは nil）から公開の判断を導く純粋関数である。
// previous は前回観測した rel_path で、nil なら初回の観測として rel_path の進みを判定しない。
// 区間の比較は量子化後の値どうしで行う。keep は chapters.Derive を通った時点で量子化済みで、
// 凍結側は書き込み時に量子化して入れてある。
// worker の初回観測と、store の公開 tx 内の再確認が同じ判定を使う。
func PlanEncodedPublish(asset *EncodedAssetObservation, cut bool, keep []chapters.Range, previous *string) EncodedPublishPlan {
	var p EncodedPublishPlan
	exists := asset != nil
	active := exists && asset.State == "active"
	if exists {
		p.ObservedRelPath = asset.RelPath
	}
	p.Replaces = cut && active
	committed := active
	if committed && cut {
		committed = chapters.SameRanges(asset.KeepRanges, keep)
	}
	moved := previous != nil && *previous != p.ObservedRelPath
	p.Stale = moved && !committed && active
	p.Skip = committed || (moved && !p.Stale)
	return p
}
