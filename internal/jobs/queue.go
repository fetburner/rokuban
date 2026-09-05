// Package jobs は River ジョブのメッセージ契約を提供する。
//
// ジョブ引数の型とキューの規約は、ジョブを実行する worker だけでなく、
// ジョブを投入する api・watcher・CLI も共有する。実行側の依存を持たない
// このパッケージに契約を置くことで、投入側が worker の実装（mirakc・
// ファイルシステム・ffmpeg）を依存グラフへ取り込まないようにする。
package jobs

import (
	"slices"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db"
)

const (
	// DefaultQueue は River の既定キュー名。
	DefaultQueue = river.QueueDefault

	// IngestQueue は録画取り込みジョブのキュー名。
	IngestQueue = "ingest"
	// EpgQueue は EPG・チューナー同期ジョブのキュー名。
	EpgQueue = "epg"
	// RulerQueue は ruler パスのキュー名。
	RulerQueue = "ruler"
	// ReconcilerQueue は reconciler パスのキュー名。
	ReconcilerQueue = "reconciler"
	// RecordSweepQueue は watcher の record sweep ジョブのキュー名。
	RecordSweepQueue = "watcher"
	// EncodeQueue はエンコード関連ジョブのキュー名。
	EncodeQueue = "encode"
	// ThumbnailQueue はサムネイルジョブのキュー名。
	ThumbnailQueue = "thumbnail"
	// CleanupQueue は物理削除・カタログ出力ジョブのキュー名。
	CleanupQueue = "cleanup"
	// StorageQueue はストレージ観測ジョブのキュー名。
	StorageQueue = "storage"

	// UniqueByQueue はキュー名を River の一意性キーへ含める設定。
	// site 修飾やキューのリネーム後も、旧キューのジョブが新キューへの投入を
	// 塞がないようにする。
	UniqueByQueue = true

	// RiverQueueNameMaxLen は River が許容するキュー名の最大長。
	RiverQueueNameMaxLen = 64
)

// pendingJobStates は「まだ終わっていない」ジョブの状態。
//
// UniqueOpts.ByState に渡して、一意化の対象を実行前・実行中に限定する。
// River の既定（rivertype.UniqueOptsByStateDefault）は completed と discarded を
// 含むため、既定のままだと「一度成功した引数のジョブは二度と投入できない」に
// なってしまう。定期ジョブは実質ワンショットになり、失敗して破棄されたジョブも
// 再投入できない。
//
// 同時実行を防ぐのが目的なので、終わったジョブは一意性の判定から外す。
// 「もう処理済みか」は River のジョブ履歴ではなく DB の状態（media_assets 行の
// 有無など）が真実である。
var pendingJobStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

// siteBoundQueueNames は mirakc への到達を必要とする、site 単位の論理キュー名。
// ingest・epg・reconciler・watcher の 4 つで、挿入側と購読側の両方がこの集合を
// 共有する。
var siteBoundQueueNames = []string{
	IngestQueue,
	EpgQueue,
	ReconcilerQueue,
	RecordSweepQueue,
}

// PendingJobStates は一意性の対象にする未完了ジョブの状態を返す。
//
// 呼び出し側が返り値を変更しても契約の共有状態を壊さないよう、コピーを返す。
func PendingJobStates() []rivertype.JobState {
	return append([]rivertype.JobState(nil), pendingJobStates...)
}

// qualifyQueueName は site 単位のキューの物理名を組み立てる。
//
// **直接呼ばない。site 修飾するかどうかの判定は PhysicalQueueName の 1 系統
// だけに通す**（判定が insert 側と subscribe 側に分かれると挿入先と購読先が
// 食い違う。issue #185）。
//
// 区切り文字は `_`。site が空文字列なら db.DefaultSite に解決する。これは
// キュー名の規約上の解決であり、ジョブ引数の site を正規化するものではない。
func qualifyQueueName(base, site string) string {
	if site == "" {
		site = db.DefaultSite
	}
	return base + "_" + site
}

// PhysicalQueueName は論理キュー名から、このプロセスが実際に River へ Insert
// / 購読する物理キュー名を返す。site 単位のキューだけ site 修飾し、それ以外は
// 論理名をそのまま返す。
//
// InsertOpts と worker の購読設定の両方がこの関数を通ることで、site 単位の判定が
// 2 箇所に分かれて食い違う経路を作らない。
func PhysicalQueueName(logical, boundSite string) string {
	if !slices.Contains(siteBoundQueueNames, logical) {
		return logical
	}
	return qualifyQueueName(logical, boundSite)
}

// AllQueueNames はこのプロセスが知っている全キューの論理名をソートして返す。
// worker.queues の許可リストと CLI の入力検証が同じ契約を使えるよう、キューを
// 実行する worker ではなくこのパッケージに置く。
func AllQueueNames() []string {
	names := []string{
		DefaultQueue,
		IngestQueue,
		EpgQueue,
		RulerQueue,
		ReconcilerQueue,
		RecordSweepQueue,
		EncodeQueue,
		ThumbnailQueue,
		CleanupQueue,
		StorageQueue,
	}
	slices.Sort(names)
	return names
}

// RequiresEncodeTools は、指定した論理キュー集合が encode または thumbnail を
// 含むかを返す。空スライスは「全キュー購読」を意味するので true を返す。
func RequiresEncodeTools(queues []string) bool {
	if len(queues) == 0 {
		return true
	}
	return slices.Contains(queues, EncodeQueue) || slices.Contains(queues, ThumbnailQueue)
}

// RequiresSiteBinding は、指定した論理キュー集合が site 単位のキューを含むかを
// 返す。空スライスは「全キュー購読」を意味するので true を返す。
func RequiresSiteBinding(queues []string) bool {
	if len(queues) == 0 {
		return true
	}
	for _, queue := range siteBoundQueueNames {
		if slices.Contains(queues, queue) {
			return true
		}
	}
	return false
}
