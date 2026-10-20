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

// InPlaceAsset は登録する既存ファイルを表す。
type InPlaceAsset struct {
	Kind      string
	Profile   *string
	RelPath   string
	SizeBytes int64
}

// InPlaceInput は一つの録画と、その録画に属する既存ファイル群を表す。
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
type MediaPublish interface {
	// CommitOriginal は原本、進捗の削除、凍結方針を一つの tx で確定する。
	// 呼び出し側が rel_path のファイルロックを持って呼ぶ。publish は DB の検査と行作成後、commit 前に一度だけ呼ぶ。
	CommitOriginal(ctx context.Context, input CommitOriginalInput, resolvePolicy ResolveEncodePolicy, publish func() error) error
	// PublishEncoded は試行・desired・作り直し要求・cut 区間を再確認して encoded を公開する。
	// cut の場合は同じ録画のチャプター編集および作り直し要求と直列化する。
	// 呼び出し側が rel_path のファイルロックを持って呼び、publish は成功時に一度だけ呼ぶ。
	PublishEncoded(ctx context.Context, input EncodedPublishInput, publish func() error) (published bool, err error)
	// PublishThumbnail は録画とサムネイルの状態を再確認して公開する。
	// 呼び出し側が rel_path のファイルロックを持って呼び、publish は成功時に一度だけ呼ぶ。
	PublishThumbnail(ctx context.Context, input ThumbnailPublishInput, publish func() error) (published bool, err error)
	// PublishSeekTiles は録画とシークタイルの状態を再確認して公開する。
	// 呼び出し側が rel_path のファイルロックを持って呼び、publish は成功時に一度だけ呼ぶ。
	PublishSeekTiles(ctx context.Context, input SeekTilesPublishInput, publish func() error) (published bool, err error)
	// RegisterInPlace は既存ファイルを指す録画とアセットの行を一つの tx で登録する。
	RegisterInPlace(ctx context.Context, input InPlaceInput) (InPlaceResult, error)
	// UpsertIngestProgress は転送中の進捗を記録する。失敗は ingest を止めない。
	UpsertIngestProgress(ctx context.Context, progress IngestProgress) error
}
