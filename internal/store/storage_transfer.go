package store

import (
	"context"
	"time"
)

// MediaAssetTransferCandidate は desired store にコピーが必要な論理資産を表す。
type MediaAssetTransferCandidate struct {
	MediaAssetID int64
	Store        string
}

// MediaAssetTransfer は 1 回の転送に必要な、ロック後の論理資産とコピーの状態を表す。
type MediaAssetTransfer struct {
	MediaAssetID  int64
	RecordingID   int64
	Generation    int64
	RelPath       string
	SizeBytes     int64
	Kind          string
	SHA256        []byte
	DesiredStore  string
	SourceStore   string
	SourceRelPath string
	SourceCopyID  int64
	CopyID        int64
}

// MediaAssetCopyDeleteCandidate は配置規則から外れた物理コピーの削除候補を表す。
type MediaAssetCopyDeleteCandidate struct {
	CopyID       int64
	MediaAssetID int64
	Store        string
	RelPath      string
	State        string
	SizeBytes    int64
	Kind         string
	Generation   int64
}

// TransferBandwidthLease は転送元置き場の排他を保持する、pool から借りた DB 接続を表す。
type TransferBandwidthLease interface {
	// Release は advisory lock を解放して接続を pool に戻す。
	// 解放に失敗した接続は lock を持ったままの恐れがあるので、pool に戻さず閉じる。
	Release(ctx context.Context) error
}

// TransferBacklogSnapshot は、ある置き場について encode reconcile が直近に保存した転送待ちの件数と、数え始めた時刻である。
type TransferBacklogSnapshot struct {
	Assets    int64
	CountedAt time.Time
}

// TransferBacklogWrite は、RecordTransferBacklog の結果を置き場ごとに表す。
type TransferBacklogWrite struct {
	// Current は書き込み後に DB に残っている件数。自分の件数か、保存済みのより新しい件数のどちらか。
	// 止めるかの判断は、ゲージと同じこの値で行う。
	Current int64
	// Applied は、この呼び出しの件数を書いたか。保存済みのほうが新しいときは false。
	Applied bool
	// HadPrevious は、Applied のとき、書き換える前の行があったか。
	HadPrevious bool
	// Previous は、HadPrevious のときの、書き換える前の件数。
	Previous int64
}

// TransferBacklogReader は、encode reconcile が直近に数えた置き場ごとの転送待ちの件数を読む契約である。
// 常駐プロセスの scrape と、毎回空から始まる worker の両方が使う。
type TransferBacklogReader interface {
	// ReadTransferBacklog は置き場ごとの直近の件数と時刻を返す。数えていない置き場は含まない。
	ReadTransferBacklog(ctx context.Context) (map[string]TransferBacklogSnapshot, error)
}

// StorageTransfer は desired state と物理コピーの reconcile を永続化する契約である。
type StorageTransfer interface {
	TransferBacklogReader
	// DatabaseClock は DB の時計の現在時刻を返す。RecordTransferBacklog の countedAt の出所は、
	// プロセスごとに違う時計ではなく、この 1 つにする。
	DatabaseClock(ctx context.Context) (time.Time, error)
	// RecordTransferBacklog は、上限付きの置き場ごとの件数 counts を、数え始めた時刻 countedAt で 1 つのトランザクションに保存する。
	// 保存済みの時刻より新しい置き場だけを書く。返り値は counts の置き場ごとに、書き込み後に DB に残っている件数と、
	// 自分が書いたか、書いたなら書き換える前の状態を持つ。古い件数は捨てて、残っている新しい件数を返す。
	// counts に無い置き場の行は消す。並行する呼び出しは直列になるので、前の状態は呼び出しごとに別の値になる。
	// 呼び出し側は、前の状態と今の件数の比較で状態の変化を 1 度だけ検出できる。
	RecordTransferBacklog(ctx context.Context, counts map[string]int64, countedAt time.Time) (written map[string]TransferBacklogWrite, err error)
	// ListMediaAssetTransferCandidates は active で desired store にコピーが無い資産を返す。
	ListMediaAssetTransferCandidates(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string, attemptLimit, limit int32) ([]MediaAssetTransferCandidate, error)
	// CountMediaAssetTransferCandidates は現在の転送待ちの件数とバイト数を返す。
	CountMediaAssetTransferCandidates(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string, attemptLimit int32) (assetCount, byteCount int64, err error)
	// CountMediaAssetTransferBacklogBySourceStore は、sourceStores のうち転送元のアクティブなコピーを持つ置き場ごとに、
	// 転送待ちの論理資産の件数を返す。件数の無い置き場は含まない。
	CountMediaAssetTransferBacklogBySourceStore(ctx context.Context, originalRules []byte, fsStores, sourceStores []string, derivedStore string, attemptLimit int32) (map[string]int64, error)
	// CountMediaAssetTransferExhausted は失敗上限に達して reconcile から外れている件数を返す。
	CountMediaAssetTransferExhausted(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string, attemptLimit int32) (int64, error)
	// ClearObsoleteMediaAssetTransferAttempts は現在の desired / generation から外れた失敗行を消す。
	ClearObsoleteMediaAssetTransferAttempts(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string) error
	// AcquireTransferSourceBandwidth は転送元置き場ごとの advisory lock を取る。
	AcquireTransferSourceBandwidth(ctx context.Context, sourceStore string) (lease TransferBandwidthLease, acquired bool, err error)
	// PrepareMediaAssetTransfer は行をロックし、現在も desired な宛先コピーを pending にする。
	// sourceStores は転送元にできる置き場（マウント済み ∪ internal_url を持つ置き場）、fsStores はマウント済みの FS 置き場。
	// 派生物の転送元は fsStores に限る。mountedStores は、この worker がマウントしている置き場で、転送元の優先に使う。
	PrepareMediaAssetTransfer(ctx context.Context, mediaAssetID int64, targetStore string, originalRules []byte, fsStores, sourceStores, mountedStores []string, derivedStore string, attemptLimit int32) (MediaAssetTransfer, bool, error)
	// CompleteMediaAssetTransfer は SHA-256 を観測し、destination copy が active になったかを返す。
	// 同じ copy を別 worker が先に active にした場合も active=true を返す。
	CompleteMediaAssetTransfer(ctx context.Context, transfer MediaAssetTransfer, targetStore, derivedStore string, originalRules, sha256 []byte) (active bool, rejection string, err error)
	// RecordMediaAssetTransferFailure は転送失敗を記録する。
	RecordMediaAssetTransferFailure(ctx context.Context, copyID int64, message string) error
	// MarkMediaAssetTransferObjectOwned は、pending 転送が新規作成した destination key を記録する。
	MarkMediaAssetTransferObjectOwned(ctx context.Context, copyID int64, storeName, relPath string) (pending bool, err error)
	// ListMediaAssetTransferObjectsOwned は、pending 転送が新規作成した destination key を返す。
	ListMediaAssetTransferObjectsOwned(ctx context.Context, copyID int64, storeName string) ([]string, error)
	// DeleteMediaAssetTransferObjectOwned は、pending 転送が不要になった owned key の記録を消す。
	DeleteMediaAssetTransferObjectOwned(ctx context.Context, copyID int64, storeName, relPath string) error
	// MarkMediaAssetCopyCorrupt は、破損が確認された派生コピーを deleting にする。
	// 録画の行、派生の論理行の順にロックし、同じ録画に active・実体ありの原本コピーがあるときだけ遷移させる。
	// unlink と deleted への確定は呼び出し側が行う。
	MarkMediaAssetCopyCorrupt(ctx context.Context, recordingID, mediaAssetID, copyID int64, storeName string) (bool, error)
	// DiscardPendingMediaAssetCopy は失敗した転送先コピーを deleted にする。
	DiscardPendingMediaAssetCopy(ctx context.Context, copyID int64) error
	// ListObsoletePendingMediaAssetTransferCopies は trash・generation・placement により不要になった pending コピーを返す。
	ListObsoletePendingMediaAssetTransferCopies(ctx context.Context, originalRules []byte, fsStores []string, derivedStore string, limit int32) ([]MediaAssetCopyDeleteCandidate, error)
	// ReclaimObsoletePendingMediaAssetTransferCopy は path lock の下で pending の obsolete 条件を再評価し、物理削除して deleted にする。
	ReclaimObsoletePendingMediaAssetTransferCopy(ctx context.Context, candidate MediaAssetCopyDeleteCandidate, originalRules []byte, fsStores []string, derivedStore string, deleteFile func(ownedRelPaths []string) error) (reclaimed bool, err error)
	// ListMediaAssetCopiesToDelete は、現行 policy の desired copy を確認できる active の stale copy を返す。
	ListMediaAssetCopiesToDelete(ctx context.Context, originalRules []byte, derivedStore string, overlapCutoff time.Time, limit int32) ([]MediaAssetCopyDeleteCandidate, error)
	// GetMediaAssetCopyForDeletion はジョブ引数の copy ID から現在のパスを返す。
	GetMediaAssetCopyForDeletion(ctx context.Context, candidate MediaAssetCopyDeleteCandidate) (MediaAssetCopyDeleteCandidate, bool, error)
	// MarkMediaAssetCopyDeleting は media_assets をロックし直し、削除条件を再評価して path を返す。
	MarkMediaAssetCopyDeleting(ctx context.Context, candidate MediaAssetCopyDeleteCandidate, originalRules []byte, derivedStore string, overlapCutoff time.Time) (relPath string, marked bool, err error)
	// MarkMediaAssetCopyDeleted は unlink 済みの deleting copy を deleted にする。
	MarkMediaAssetCopyDeleted(ctx context.Context, candidate MediaAssetCopyDeleteCandidate) (bool, error)
}
