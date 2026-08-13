package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/diskusage"
	"github.com/fetburner/rokuban/internal/metrics"
)

// defaultStorageSyncInterval はストレージ観測の既定間隔。
//
// 残量は録画・削除・エンコードで連続的に変わるが、「残高」として UI に出す用途に
// 分単位の鮮度で足りる（tuner_sync の 10 分より短くしているのは、容量枯渇の
// 兆候をアラートに乗せるまでの遅延を抑えるため）。専用の設定キーは設けていない
// --- tuner_sync / catalog_export / delete_reconcile の既定間隔と同じく、
// 運用者が調整する理由が今のところ無い（issue #238）。
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

	// Stat は 1 root を観測する関数。nil なら diskusage.Stat を使う。
	// テストが実ディスクの数字に依存せず組み立てられるよう差し替え可能にしている
	// （internal/diskusage のコメント参照）。
	Stat func(path string) (diskusage.Usage, error)
}

// Timeout は River の既定（1 分）と同じ上限を明示する。
func (w *StorageSyncWorker) Timeout(*river.Job[StorageSyncArgs]) time.Duration {
	return storageSyncTimeout
}

func (w *StorageSyncWorker) statFunc() func(string) (diskusage.Usage, error) {
	if w.Stat != nil {
		return w.Stat
	}
	return diskusage.Stat
}

// roots は今回のパスで観測すべき root の一覧を返す。
func (w *StorageSyncWorker) roots() []storageRoot {
	roots := []storageRoot{{name: "media", path: w.MediaDir}}
	if w.ScratchDir != "" {
		roots = append(roots, storageRoot{name: "scratch", path: w.ScratchDir})
	}
	return roots
}

// Work はストレージ観測の全量同期を 1 パス実行する。
func (w *StorageSyncWorker) Work(ctx context.Context, _ *river.Job[StorageSyncArgs]) error {
	if w.MediaDir == "" {
		return fmt.Errorf("storage sync: media dir is empty")
	}

	roots := w.roots()
	desired := make([]string, len(roots))
	for i, r := range roots {
		desired[i] = r.name
	}

	q := sqlcgen.New(w.Pool)

	// config が要求しなくなった root（例: scratch_dir を空に変更した）だけを
	// 消す。今回 statfs に失敗した root はこの集合に含まれる（desired から
	// 外れない）ので、ここでは消えない --- 消すかどうかは config が決め、
	// 「今回観測できたか」では決めない（storage_sync.sql のコメント参照）。
	if err := q.DeleteStorageSyncExcept(ctx, desired); err != nil {
		return fmt.Errorf("storage sync: deleting stale roots: %w", err)
	}

	stat := w.statFunc()
	observed := 0
	for _, r := range roots {
		u, err := stat(r.path)
		if err != nil {
			// 1 root の statfs 失敗でパス全体を失敗させない --- media と scratch は
			// 別々のマウントであることが多く、片方の一時的な不調（アンマウント等）
			// でもう片方の観測まで止める理由がない。前回の観測行はそのまま残す
			// （observed_at が更新されないので、UI の鮮度表示が「観測が止まって
			// いる」ことを黒く塗らずに伝える。metrics.TunerSyncLastSuccess と同じ
			// 「沈黙は保証ではない」姿勢）。
			slog.Warn("storage sync: statfs failed, keeping previous observation",
				"root", r.name, "path", r.path, "err", err)
			continue
		}
		if err := q.UpsertStorageSync(ctx, sqlcgen.UpsertStorageSyncParams{
			Root:           r.name,
			Path:           r.path,
			TotalBytes:     u.TotalBytes,
			UsedBytes:      u.UsedBytes,
			AvailableBytes: u.AvailableBytes,
		}); err != nil {
			return fmt.Errorf("storage sync: upserting root %s: %w", r.name, err)
		}
		observed++
	}

	if observed == 0 {
		// 1 つも観測できなかった（media_dir すら statfs に失敗した）場合は
		// ジョブそのものを失敗させ River のリトライに委ねる --- 「observed_at が
		// 全く進んでいない」ことをジョブの失敗としても表出させるため
		// （EpgSyncWorker が空レスポンスでスイープを見送るのと同じ、警告を
		// 見逃さない側に倒す判断）。
		return fmt.Errorf("storage sync: all %d root(s) failed to observe", len(roots))
	}

	metrics.StorageSyncLastSuccess.SetToCurrentTime()
	slog.Info("storage sync complete", "roots", len(roots), "observed", observed)
	return nil
}
