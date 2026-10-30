// Package jobs は非同期ジョブの引数とキューの契約を提供する。
//
// ジョブ引数の型とキューの規約は、ジョブを実行する worker だけでなく、
// ジョブを投入する api・watcher・CLI も共有する。実行側の依存を持たない
// このパッケージに契約を置くことで、投入側が worker の実装（mirakc・
// ファイルシステム・ffmpeg）を依存グラフへ取り込まないようにする。
package jobs

import (
	"slices"

	"github.com/fetburner/rokuban/internal/store"
)

const (
	// EncodeAttemptLimit は 1 つの (録画, プロファイル) に許すドメイン上の encode
	// 試行回数。worker が上限判定に使い、api が待機列から「もう作られない」行を
	// 除くのに同じ値を使う。
	EncodeAttemptLimit int32 = 25

	// DefaultQueue は既定キュー名。
	DefaultQueue = "default"

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
	// ThumbnailQueue はサムネイル / シークタイルジョブのキュー名。
	ThumbnailQueue = "thumbnail"
	// CMDetectQueue は CM 区間の検出・検出候補 reconcile 用キュー名。
	CMDetectQueue = "cm_detect"
	// CleanupQueue は物理削除・カタログ出力・分類ルール全件再評価（site 非依存の DB ジョブ）のキュー名。
	//
	// 未解決: 既定（`worker.queues` 未指定 = 全キュー購読）のサイト束縛 worker は
	// このキューも掴む。`worker.queues` を明示すれば除外できるが、単一サイト
	// 構成でそれを外すと delete_reconcile が誰にも走らなくなるため、既定を変える
	// かどうかは設計判断が必要で未実装。
	CleanupQueue = "cleanup"
	// StorageQueue はストレージ観測ジョブのキュー名。
	StorageQueue = "storage"
	// TransferQueue は desired store への物理コピー転送ジョブの論理キュー名。
	TransferQueue = "transfer"
	// TSScanQueue は原本 TS の解析ジョブを site 非依存で処理するキュー名。
	TSScanQueue = "ts_scan"
)

// siteBoundQueueNames は mirakc への到達を必要とする、site 単位の論理キュー名。
// ingest・epg・reconciler・watcher の 4 つで、挿入側と購読側の両方がこの集合を
// 共有する。
//
// **この 1 変数が意図的に 2 役目を持つ**: PhysicalQueueName の修飾対象判定と、
// RequiresSiteBinding の起動時ガードの両方がここを参照する。分けると 2 つの
// 判定が食い違う経路ができる（issue #185）。
//
// **ruler はここに入らない**: mirakc に触れず DB のみで完結する仕事なので
// site 修飾も起動時の site 束縛ガードも要らない（不変条件 1、issue #138 の決定）。
var siteBoundQueueNames = []string{
	IngestQueue,
	EpgQueue,
	ReconcilerQueue,
	RecordSweepQueue,
}

// qualifyQueueName は site 単位のキューの物理名を組み立てる。
//
// **直接呼ばない。site 修飾するかどうかの判定は PhysicalQueueName の 1 系統
// だけに通す**（判定が insert 側と subscribe 側に分かれると挿入先と購読先が
// 食い違う。issue #185）。
//
// 区切り文字は `_`。site が空文字列なら store.DefaultSite に解決する。これは
// キュー名の規約上の解決であり、ジョブ引数の site を正規化するものではない。
func qualifyQueueName(base, site string) string {
	if site == "" {
		site = store.DefaultSite
	}
	return base + "_" + site
}

// PhysicalQueueName は論理キュー名から、adapter が Insert / 購読する物理キュー名を
// 返す。site 単位のキューだけ site 修飾し、それ以外は論理名をそのまま返す。
//
// adapter の投入（PhysicalQueueFor 経由で adapter へ注入する）と、cmd/rokuban が
// 組む worker 仕様の購読側の物理名の展開の両方がこの関数を通ることで、site 単位の
// 判定を 2 箇所に分けて食い違う経路を作らない。adapter 自身は命名規則を持たない。
func PhysicalQueueName(logical, boundSite string) string {
	if !slices.Contains(siteBoundQueueNames, logical) {
		return logical
	}
	return qualifyQueueName(logical, boundSite)
}

// PhysicalStoreQueueName は置き場単位の仕事を `{queue}_{store}` に振り分ける。
func PhysicalStoreQueueName(logical, storeName string) string {
	return logical + "_" + storeName
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
		CMDetectQueue,
		CleanupQueue,
		StorageQueue,
		TransferQueue,
		TSScanQueue,
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

// RequiresCMDetectTools reports whether the selected queues may execute CM jobs.
func RequiresCMDetectTools(queues []string) bool {
	return len(queues) == 0 || slices.Contains(queues, CMDetectQueue)
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
