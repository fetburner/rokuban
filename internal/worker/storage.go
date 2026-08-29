package worker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
)

// defaultStorageSyncInterval はストレージ観測の既定間隔。
//
// tuner_sync（10 分）より短くしているが、この値そのものに測定的根拠はない
// （未検証。record_sweep の 5 分に合わせただけの初期値）。実際のアラート整備
// （time() 差分での閾値検討）は本 PR のスコープ外 --- rokuban_storage_root_*
// メトリクスを export するようになった今は組めるようになったが、アラート自体は
// 未着手。専用の設定キーは設けていない --- tuner_sync / catalog_export /
// delete_reconcile / record_sweep の既定間隔と同じく、運用者が調整する理由が
// 今のところ無い（docs/runbook/setup.md の record_sweep の前例。issue #238）。
const defaultStorageSyncInterval = 5 * time.Minute

// storageSyncTimeout は 1 パス（root ごとの statfs + DB upsert 高々 2 回）の上限。
// tuner_sync と同じ理由で、River の既定（1 分）を明示する。
const storageSyncTimeout = time.Minute

// StorageSyncArgs はストレージ観測ジョブの引数。
//
// フィールドを持たない --- 観測対象（media_dir / scratch_dir）は worker
// プロセスの config から決まり、tuner_sync や epg_sync のように mirakc サイトへ
// 分岐する理由がない（アーカイブもスクラッチも単一。docs/storage/contract.md
// §5「rel_path の名前空間」の通りアーカイブは site 列を持たない）。
type StorageSyncArgs struct{}

// Kind は River ジョブの種別名を返す。
func (StorageSyncArgs) Kind() string { return "storage_sync" }

// InsertOpts は River ジョブの挿入オプションを返す。
//
// CatalogExportArgs / DeleteReconcileArgs と同じ理由（site 非依存、UniqueOpts で
// 重複実行を防ぐ）だが、キューは専用の storageQueue にする --- cleanupQueue は
// 「物理削除系ジョブ専用」と明記されている（allQueues のコメント参照）ため、
// 削除を一切行わない観測ジョブをそこに混ぜると、その名付けの前提を壊す。
func (StorageSyncArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: storageQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: uniqueByQueue,
			ByState: pendingJobStates,
		},
	}
}

// storageRoot は 1 つの観測対象（config キーと statfs するパスの対応）。
type storageRoot struct {
	// name は storage_sync.root の値（'media' | 'scratch'）。config キー名
	// （media_dir / scratch_dir）の接頭辞と 1:1 に保つ（マイグレーションの CHECK
	// 制約参照）。
	name string
	path string
}

// StorageSyncWorker は storage.media_dir / storage.scratch_dir の容量を
// storage_sync に全量投影する River ワーカー（issue #238 M7-5）。
//
// 存在理由は不変条件 1（api ロールはファイルシステムに依存しない）。ファイル
// システムを持つ worker がここで観測し、api はこの射影だけを読んで
// GET /api/storage を組み立てる。
//
// レベルトリガー: 毎パス「今 config が要求する root 全量」を対象に statfs
// し直す。過去の観測を積むログではなく、常に最新の観測だけを持つ（不変条件 9:
// 導出値と不可逆な事実を同じ列に載せない --- total/used/available bytes は
// 二度と取れない事実ではなく毎回作り直せる観測値なので、upsert で置き換える）。
// 観測ループを止めて再起動しても、次のパスが全量を書き直すので収束する
// （crash-only。TestStorageSyncWorker_RestartConverges で固定）。
type StorageSyncWorker struct {
	river.WorkerDefaults[StorageSyncArgs]
	Pool *pgxpool.Pool

	// MediaDir は storage.media_dir（必須。空なら Work がエラーを返す --- config
	// の validate:"required" を信頼しきらず、テストの部分構成でも安全に失敗する）。
	MediaDir string

	// ScratchDir は storage.scratch_dir。空文字列は「観測しない」を意味する ---
	// config 側は既定値 (/var/tmp/rokuban) を持つため、明示的に空にした場合だけ
	// ここに空文字列が届く。
	ScratchDir string

	// Stat は 1 root を観測する関数。nil なら statDisk を使う。
	// テストが実ディスクの数字に依存せず組み立てられるよう差し替え可能にしている
	// （internal/worker/diskusage.go のコメント参照）。
	Stat func(path string) (diskUsage, error)
}

// Timeout は River の既定（1 分）と同じ上限を明示する。
func (w *StorageSyncWorker) Timeout(*river.Job[StorageSyncArgs]) time.Duration {
	return storageSyncTimeout
}

func (w *StorageSyncWorker) statFunc() func(string) (diskUsage, error) {
	if w.Stat != nil {
		return w.Stat
	}
	return statDisk
}

// Work はストレージ観測の全量同期を 1 パス実行する。
func (w *StorageSyncWorker) Work(ctx context.Context, _ *river.Job[StorageSyncArgs]) error {
	if w.MediaDir == "" {
		return fmt.Errorf("storage sync: media dir is empty")
	}

	// allRoots は Rokuban が観測しうる root の全体集合（config キーと statfs
	// するパスの対応）。storage_sync.root 列の CHECK (root IN ('media',
	// 'scratch')) と 1:1 で、Go 側ではここが唯一の出所（観測対象も、外れた
	// root のラベル掃除の走査範囲も、両方これから導出する。増減するときは
	// マイグレーションの CHECK も合わせて直す）。
	allRoots := []storageRoot{
		{name: "media", path: w.MediaDir},
		{name: "scratch", path: w.ScratchDir},
	}

	// desired は今回観測する root 名の一覧。allRoots は高々 2 要素なので、
	// この後の掃除ループは desiredSet を別途持たず slices.Contains(desired, ...)
	// で足りる。
	roots := make([]storageRoot, 0, len(allRoots))
	desired := make([]string, 0, len(allRoots))
	for _, r := range allRoots {
		if r.path == "" {
			// 空文字列は「この root は観測しない」を意味する（scratch_dir を
			// 明示的に空にした場合。media_dir が空のケースは上の早期リターンで
			// 弾いている）。
			continue
		}
		roots = append(roots, r)
		desired = append(desired, r.name)
	}

	q := sqlcgen.New(w.Pool)

	// config が要求しなくなった root だけを消す。今回 statfs に失敗した root は
	// この集合に含まれる（desired から外れない）ので、ここでは消えない ---
	// 消すかどうかは config が決め、「今回観測できたか」では決めない
	// （storage_sync.sql のコメント参照）。
	if err := q.DeleteStorageSyncExcept(ctx, desired); err != nil {
		return fmt.Errorf("storage sync: deleting stale roots: %w", err)
	}

	// Prometheus 側も同じ desired set で毎パス揃える。DB 行と違って
	// DeleteLabelValues は「消えていること」に対して冪等なので、既に消えている
	// ラベルを毎パス消しても何も起きない --- 逆に「行がまだ見えていた 1 パス」に
	// 掃除を賭けると、storage キューを引く別レプリカが先に行を消したパスで
	// ラベルが取り残され、二度と掃除されない（不変条件 5: レベルトリガー。
	// TestStorageSyncWorker_MetricsClearedWhenRowRemovedByAnotherReplica で固定）。
	// 取り残されたラベルは「凍結した鮮度ゲージ」= Pod を再起動するまで消えない
	// 偽陽性アラートになる（docs/operations/monitoring.md §沈黙は保証ではない が
	// この鮮度を判断材料に挙げている）。
	for _, r := range allRoots {
		if slices.Contains(desired, r.name) {
			continue
		}
		metrics.StorageRootLastSuccess.DeleteLabelValues(r.name)
		metrics.StorageRootTotalBytes.DeleteLabelValues(r.name)
		metrics.StorageRootUsedBytes.DeleteLabelValues(r.name)
		metrics.StorageRootAvailableBytes.DeleteLabelValues(r.name)
	}

	stat := w.statFunc()
	observed := 0
	for _, r := range roots {
		u, err := stat(r.path)
		if err != nil {
			// 1 root の statfs 失敗でパス全体を失敗させない --- media と scratch は
			// 別々のマウントであることが多く、片方の一時的な不調（アンマウント等）
			// でもう片方の観測まで止める理由がない。前回の観測行・
			// StorageRootLastSuccess / バイト数ゲージはそのまま残す（更新しない）。
			// observed_at とこれらのゲージの鮮度だけが「観測が止まっている」
			// ことを伝える手がかりになる（M7-6 で UI がこの鮮度を出せるように
			// なる想定。metrics.TunerSyncLastSuccess と同じ「沈黙は保証ではない」
			// 姿勢）。
			slog.Warn("storage sync: statfs failed, keeping previous observation",
				"root", r.name, "path", r.path, "err", err)
			continue
		}
		if err := q.UpsertStorageSync(ctx, sqlcgen.UpsertStorageSyncParams{
			Root:           r.name,
			Path:           r.path,
			TotalBytes:     u.totalBytes,
			UsedBytes:      u.usedBytes,
			AvailableBytes: u.availableBytes,
		}); err != nil {
			return fmt.Errorf("storage sync: upserting root %s: %w", r.name, err)
		}
		observed++

		// root ごとのゲージは、この root の観測が実際に成功した場合にだけ進める
		// （StorageSyncLastSuccess の対になる、root 単位の鮮度シグナル。
		// internal/metrics.StorageRootLastSuccess のコメント参照）。
		metrics.StorageRootLastSuccess.WithLabelValues(r.name).SetToCurrentTime()
		metrics.StorageRootTotalBytes.WithLabelValues(r.name).Set(float64(u.totalBytes))
		metrics.StorageRootUsedBytes.WithLabelValues(r.name).Set(float64(u.usedBytes))
		metrics.StorageRootAvailableBytes.WithLabelValues(r.name).Set(float64(u.availableBytes))
	}

	if observed == 0 {
		// 1 つも観測できなかった（media_dir すら statfs に失敗した）場合は
		// ジョブそのものを失敗させ River のリトライに委ねる --- 「observed_at が
		// 全く進んでいない」ことをジョブの失敗としても表出させるため
		// （EpgSyncWorker が空レスポンスでスイープを見送るのと同じ、警告を
		// 見逃さない側に倒す判断）。
		return fmt.Errorf("storage sync: all %d root(s) failed to observe", len(roots))
	}

	// StorageSyncLastSuccess は**全 root**を観測できたパスだけで進める。
	// 1 root でも失敗した部分成功では進めない --- 進めてしまうと、片方の root が
	// 恒久的に壊れていても他方が成功し続ける限り「最後に成功した時刻」が
	// 現在時刻付近を保ち続け、まさにこの機能の存在理由（容量枯渇の検知）を
	// 見失う（PR #258 のレビュー指摘）。root 単位の欠落は
	// StorageRootLastSuccess で検知する。
	if observed == len(roots) {
		metrics.StorageSyncLastSuccess.SetToCurrentTime()
	}
	slog.Info("storage sync complete", "roots", len(roots), "observed", observed)
	return nil
}
