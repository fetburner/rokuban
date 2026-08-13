package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/diskusage"
	"github.com/fetburner/rokuban/internal/metrics"
)

func runStorageSync(t *testing.T, w *StorageSyncWorker) error {
	t.Helper()
	job := &river.Job[StorageSyncArgs]{JobRow: &rivertype.JobRow{}, Args: StorageSyncArgs{}}
	return w.Work(context.Background(), job)
}

func allStorageSync(t *testing.T, pool *pgxpool.Pool) []sqlcgen.StorageSync {
	t.Helper()
	rows, err := sqlcgen.New(pool).ListStorageSync(context.Background())
	if err != nil {
		t.Fatalf("ListStorageSync: %v", err)
	}
	return rows
}

func storageSyncByRoot(t *testing.T, pool *pgxpool.Pool, root string) sqlcgen.StorageSync {
	t.Helper()
	for _, r := range allStorageSync(t, pool) {
		if r.Root == root {
			return r
		}
	}
	t.Fatalf("no storage_sync row for root %q", root)
	return sqlcgen.StorageSync{}
}

// roots() は ScratchDir が空文字列のときだけ scratch を対象から外す。
// worker.Work の統合テスト（TestStorageSyncWorker_ScratchOptional）は
// 「観測失敗時は既存行に触らない」設計のせいで、誤って毎回 scratch を対象に
// 入れてしまう変異（対象パスが "" のまま statfs が失敗するだけ）を見逃しうる ---
// 対象集合そのものをここで直接検査する。
func TestStorageSyncWorker_Roots(t *testing.T) {
	w := &StorageSyncWorker{MediaDir: "/media"}
	if got := w.roots(); len(got) != 1 || got[0].name != "media" {
		t.Errorf("roots() = %+v, want [{media /media}]", got)
	}

	w.ScratchDir = "/scratch"
	got := w.roots()
	if len(got) != 2 {
		t.Fatalf("roots() = %+v, want 2 entries", got)
	}
	if got[0].name != "media" || got[1].name != "scratch" || got[1].path != "/scratch" {
		t.Errorf("roots() = %+v, want [{media /media} {scratch /scratch}]", got)
	}

	// allStorageRootNames は「観測しうる root の全体集合」であり、ラベル掃除の
	// 走査範囲でもある。ここに名前を足して rootPath() の case を足し忘れると、
	// その root は永久に観測されないのに DB の CHECK は通る（掃除だけが効いて
	// 行が消え続ける）ので、全 config を与えたときの roots() が全体集合と
	// 一致することを検査する。
	if len(got) != len(allStorageRootNames) {
		t.Fatalf("roots() with every dir configured = %+v (%d entries), want one per allStorageRootNames (%v)",
			got, len(got), allStorageRootNames)
	}
	for i, name := range allStorageRootNames {
		if got[i].name != name {
			t.Errorf("roots()[%d].name = %q, want %q (rootPath() missing a case?)", i, got[i].name, name)
		}
	}
}

// media/scratch の両方を実ディレクトリに向けた 1 パスで、2 行が正しく投影されること。
func TestStorageSyncWorker_FullSync(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	w := &StorageSyncWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: scratchDir}

	if err := runStorageSync(t, w); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	rows := allStorageSync(t, pool)
	if len(rows) != 2 {
		t.Fatalf("storage_sync rows = %+v, want 2", rows)
	}

	media := storageSyncByRoot(t, pool, "media")
	if media.Path != mediaDir {
		t.Errorf("media.Path = %q, want %q", media.Path, mediaDir)
	}
	if media.TotalBytes <= 0 {
		t.Errorf("media.TotalBytes = %d, want > 0", media.TotalBytes)
	}
	if media.UsedBytes > media.TotalBytes {
		t.Errorf("media.UsedBytes = %d > TotalBytes = %d", media.UsedBytes, media.TotalBytes)
	}

	scratch := storageSyncByRoot(t, pool, "scratch")
	if scratch.Path != scratchDir {
		t.Errorf("scratch.Path = %q, want %q", scratch.Path, scratchDir)
	}
}

// ScratchDir が空文字列なら scratch root は観測しない（media だけの 1 行）。
func TestStorageSyncWorker_ScratchOptional(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	w := &StorageSyncWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: ""}

	if err := runStorageSync(t, w); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	rows := allStorageSync(t, pool)
	if len(rows) != 1 {
		t.Fatalf("storage_sync rows = %+v, want 1 (media only)", rows)
	}
	if rows[0].Root != "media" {
		t.Errorf("rows[0].Root = %q, want %q", rows[0].Root, "media")
	}
}

// MediaDir が空文字列なら、DB に触る前にエラーを返す（config の validate:"required"
// を信頼しきらない安全弁。storage_sync に行が増えていないことも確認する）。
func TestStorageSyncWorker_EmptyMediaDirFails(t *testing.T) {
	pool := setupTestPool(t)
	w := &StorageSyncWorker{Pool: pool, MediaDir: ""}

	if err := runStorageSync(t, w); err == nil {
		t.Fatal("Work() with empty MediaDir succeeded, want error")
	}

	rows := allStorageSync(t, pool)
	if len(rows) != 0 {
		t.Errorf("storage_sync rows = %+v, want 0 (no partial write)", rows)
	}
}

// 観測ループを止めて再起動しても収束する（crash-only。issue #238 の受け入れ基準）。
// 2 パス目も同じ 2 行のまま（重複が増えない）で、observed_at が前進すること。
func TestStorageSyncWorker_RestartConverges(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()

	// 1 回目のパス（1 プロセスの寿命を模す）。
	w1 := &StorageSyncWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: scratchDir}
	if err := runStorageSync(t, w1); err != nil {
		t.Fatalf("first Work() error: %v", err)
	}
	first := storageSyncByRoot(t, pool, "media")

	time.Sleep(10 * time.Millisecond)

	// 「プロセスが落ちて再起動した」を新しい *StorageSyncWorker インスタンスで模す。
	w2 := &StorageSyncWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: scratchDir}
	if err := runStorageSync(t, w2); err != nil {
		t.Fatalf("second Work() error: %v", err)
	}

	rows := allStorageSync(t, pool)
	if len(rows) != 2 {
		t.Fatalf("storage_sync rows after restart = %+v, want 2 (upsert, not duplicate)", rows)
	}

	second := storageSyncByRoot(t, pool, "media")
	if !second.ObservedAt.After(first.ObservedAt) {
		t.Errorf("second ObservedAt (%v) did not advance past first (%v)", second.ObservedAt, first.ObservedAt)
	}
}

// config から scratch_dir が外れた（空文字列になった）ら、次のパスで古い scratch
// 行が消えること。DeleteStorageSyncExcept が「今回の対象集合」で差集合を消す
// 設計の受け入れ基準。
func TestStorageSyncWorker_ConfigChangeSweepsRemovedRoot(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()

	w := &StorageSyncWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: scratchDir}
	if err := runStorageSync(t, w); err != nil {
		t.Fatalf("first Work() error: %v", err)
	}
	if len(allStorageSync(t, pool)) != 2 {
		t.Fatalf("expected 2 rows before config change")
	}

	// scratch_dir を空にした運用者の再設定 + プロセス再起動を模す。
	w.ScratchDir = ""
	if err := runStorageSync(t, w); err != nil {
		t.Fatalf("second Work() error: %v", err)
	}

	rows := allStorageSync(t, pool)
	if len(rows) != 1 {
		t.Fatalf("storage_sync rows = %+v, want 1 (scratch swept)", rows)
	}
	if rows[0].Root != "media" {
		t.Errorf("remaining row root = %q, want %q", rows[0].Root, "media")
	}
}

// 1 root の statfs 失敗はパス全体を落とさない。失敗した root の既存行は
// そのまま残る（observed_at が更新されない = 鮮度で異常を伝える）。
func TestStorageSyncWorker_PartialStatFailureKeepsStaleRow(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()

	// 1 回目: 両方成功させて scratch 行を作る。
	w := &StorageSyncWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: scratchDir}
	if err := runStorageSync(t, w); err != nil {
		t.Fatalf("first Work() error: %v", err)
	}
	staleScratch := storageSyncByRoot(t, pool, "scratch")

	time.Sleep(10 * time.Millisecond)

	// 2 回目: scratch だけ statfs が失敗する状況を注入する。
	w.Stat = func(path string) (diskusage.Usage, error) {
		if path == scratchDir {
			return diskusage.Usage{}, fmt.Errorf("simulated statfs failure")
		}
		return diskusage.Stat(path)
	}
	if err := runStorageSync(t, w); err != nil {
		t.Fatalf("second Work() error: %v (partial failure must not fail the whole pass)", err)
	}

	rows := allStorageSync(t, pool)
	if len(rows) != 2 {
		t.Fatalf("storage_sync rows = %+v, want 2 (stale scratch row kept, not deleted)", rows)
	}

	gotScratch := storageSyncByRoot(t, pool, "scratch")
	if !gotScratch.ObservedAt.Equal(staleScratch.ObservedAt) {
		t.Errorf("scratch ObservedAt changed to %v, want unchanged %v (statfs failed this pass)",
			gotScratch.ObservedAt, staleScratch.ObservedAt)
	}

	media := storageSyncByRoot(t, pool, "media")
	if media.ObservedAt.Before(staleScratch.ObservedAt) {
		t.Errorf("media ObservedAt did not advance despite succeeding this pass")
	}
}

// 唯一の必須 root（media）まで含めて全 root の statfs が失敗したら、ジョブ全体を
// 失敗させて River のリトライに委ねる（「observed_at が全く進んでいない」を
// ジョブの失敗としても表出させる）。
func TestStorageSyncWorker_AllRootsFailReturnsError(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	w := &StorageSyncWorker{
		Pool:     pool,
		MediaDir: mediaDir,
		Stat: func(string) (diskusage.Usage, error) {
			return diskusage.Usage{}, fmt.Errorf("simulated statfs failure")
		},
	}

	if err := runStorageSync(t, w); err == nil {
		t.Fatal("Work() with all roots failing succeeded, want error")
	}

	rows := allStorageSync(t, pool)
	if len(rows) != 0 {
		t.Errorf("storage_sync rows = %+v, want 0 (nothing observed)", rows)
	}
}

// fakeStat は root ごとに固定の diskusage.Usage を返す（実ディスクの数字に依存せず
// ゲージの値を厳密に検証するため）。
func fakeStat(usageByPath map[string]diskusage.Usage, failPaths map[string]bool) func(string) (diskusage.Usage, error) {
	return func(path string) (diskusage.Usage, error) {
		if failPaths[path] {
			return diskusage.Usage{}, fmt.Errorf("simulated statfs failure for %s", path)
		}
		u, ok := usageByPath[path]
		if !ok {
			return diskusage.Usage{}, fmt.Errorf("fakeStat: no usage configured for %s", path)
		}
		return u, nil
	}
}

// StorageSyncLastSuccess（ジョブ全体のゲージ）は**全 root を観測できたパスだけ**
// 進む。1 root でも失敗した部分成功では進めない --- PR #258 のレビューで指摘された
// 欠陥（media が恒久的に壊れていても scratch が成功し続ける限りこのゲージが
// 進み続け、機能の存在理由である容量枯渇の検知を見失う）の直接の受け入れ基準。
func TestStorageSyncWorker_JobLevelLastSuccessOnlyOnFullSuccess(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()

	before := promtestutil.ToFloat64(metrics.StorageSyncLastSuccess)

	// 部分成功（scratch だけ失敗）では進まない。
	w := &StorageSyncWorker{
		Pool: pool, MediaDir: mediaDir, ScratchDir: scratchDir,
		Stat: fakeStat(
			map[string]diskusage.Usage{mediaDir: {TotalBytes: 100, UsedBytes: 10, AvailableBytes: 90}},
			map[string]bool{scratchDir: true},
		),
	}
	if err := runStorageSync(t, w); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
	if got := promtestutil.ToFloat64(metrics.StorageSyncLastSuccess); got != before {
		t.Errorf("StorageSyncLastSuccess = %v after partial success, want unchanged %v", got, before)
	}

	// 全 root 成功で進む。
	w.Stat = fakeStat(map[string]diskusage.Usage{
		mediaDir:   {TotalBytes: 100, UsedBytes: 10, AvailableBytes: 90},
		scratchDir: {TotalBytes: 200, UsedBytes: 20, AvailableBytes: 180},
	}, nil)
	if err := runStorageSync(t, w); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
	if got := promtestutil.ToFloat64(metrics.StorageSyncLastSuccess); got <= before {
		t.Errorf("StorageSyncLastSuccess = %v after full success, want > %v", got, before)
	}
}

// root ごとのゲージ（StorageRootLastSuccess / TotalBytes / UsedBytes /
// AvailableBytes）は、その root の観測が実際に成功したときだけ更新される。
// 失敗した root は前回の値のまま凍結する（PR #258 のレビュー指摘の核心 ---
// このゲージの組がなければ「片方の root だけ壊れている」を Prometheus から
// 検知できない）。
func TestStorageSyncWorker_PerRootMetricsFreezeOnFailure(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()

	w := &StorageSyncWorker{
		Pool: pool, MediaDir: mediaDir, ScratchDir: scratchDir,
		Stat: fakeStat(map[string]diskusage.Usage{
			mediaDir:   {TotalBytes: 100, UsedBytes: 10, AvailableBytes: 90},
			scratchDir: {TotalBytes: 500, UsedBytes: 50, AvailableBytes: 450},
		}, nil),
	}
	if err := runStorageSync(t, w); err != nil {
		t.Fatalf("first Work() error: %v", err)
	}
	scratchLastSuccessBefore := promtestutil.ToFloat64(metrics.StorageRootLastSuccess.WithLabelValues("scratch"))
	if got := promtestutil.ToFloat64(metrics.StorageRootTotalBytes.WithLabelValues("scratch")); got != 500 {
		t.Fatalf("scratch TotalBytes = %v, want 500", got)
	}

	time.Sleep(10 * time.Millisecond)

	// 2 回目: scratch の statfs が失敗、media は新しい値で成功する。
	w.Stat = fakeStat(map[string]diskusage.Usage{
		mediaDir: {TotalBytes: 999, UsedBytes: 111, AvailableBytes: 888},
	}, map[string]bool{scratchDir: true})
	if err := runStorageSync(t, w); err != nil {
		t.Fatalf("second Work() error: %v", err)
	}

	if got := promtestutil.ToFloat64(metrics.StorageRootLastSuccess.WithLabelValues("scratch")); got != scratchLastSuccessBefore {
		t.Errorf("scratch StorageRootLastSuccess = %v, want unchanged %v (statfs failed this pass)", got, scratchLastSuccessBefore)
	}
	if got := promtestutil.ToFloat64(metrics.StorageRootTotalBytes.WithLabelValues("scratch")); got != 500 {
		t.Errorf("scratch TotalBytes = %v, want unchanged 500 (statfs failed this pass)", got)
	}

	if got := promtestutil.ToFloat64(metrics.StorageRootLastSuccess.WithLabelValues("media")); got <= scratchLastSuccessBefore {
		t.Errorf("media StorageRootLastSuccess = %v, want > %v (media succeeded this pass)", got, scratchLastSuccessBefore)
	}
	if got := promtestutil.ToFloat64(metrics.StorageRootTotalBytes.WithLabelValues("media")); got != 999 {
		t.Errorf("media TotalBytes = %v, want 999 (updated this pass)", got)
	}
	if got := promtestutil.ToFloat64(metrics.StorageRootUsedBytes.WithLabelValues("media")); got != 111 {
		t.Errorf("media UsedBytes = %v, want 111", got)
	}
	if got := promtestutil.ToFloat64(metrics.StorageRootAvailableBytes.WithLabelValues("media")); got != 888 {
		t.Errorf("media AvailableBytes = %v, want 888", got)
	}
}

// config から root が外れたら、その root の Prometheus ラベルも消す
// （DeleteLabelValues）。残すと二度と更新されない値が Prometheus に居座り続ける
// （PR #258 のレビュー指摘）。
func TestStorageSyncWorker_MetricsClearedWhenRootRemoved(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()

	w := &StorageSyncWorker{
		Pool: pool, MediaDir: mediaDir, ScratchDir: scratchDir,
		Stat: fakeStat(map[string]diskusage.Usage{
			mediaDir:   {TotalBytes: 100, UsedBytes: 10, AvailableBytes: 90},
			scratchDir: {TotalBytes: 500, UsedBytes: 50, AvailableBytes: 450},
		}, nil),
	}
	if err := runStorageSync(t, w); err != nil {
		t.Fatalf("first Work() error: %v", err)
	}
	if got := promtestutil.ToFloat64(metrics.StorageRootLastSuccess.WithLabelValues("scratch")); got == 0 {
		t.Fatalf("scratch StorageRootLastSuccess = %v, want set (nonzero) before removal", got)
	}

	// scratch_dir を空にした運用者の再設定 + プロセス再起動を模す。
	w.ScratchDir = ""
	w.Stat = fakeStat(map[string]diskusage.Usage{
		mediaDir: {TotalBytes: 100, UsedBytes: 10, AvailableBytes: 90},
	}, nil)
	if err := runStorageSync(t, w); err != nil {
		t.Fatalf("second Work() error: %v", err)
	}

	if got := promtestutil.ToFloat64(metrics.StorageRootLastSuccess.WithLabelValues("scratch")); got != 0 {
		t.Errorf("scratch StorageRootLastSuccess = %v after removal, want 0 (DeleteLabelValues should have cleared it)", got)
	}
	if got := promtestutil.ToFloat64(metrics.StorageRootTotalBytes.WithLabelValues("scratch")); got != 0 {
		t.Errorf("scratch TotalBytes = %v after removal, want 0", got)
	}
}

// ラベルの掃除は「DB 行がまだ見えている遷移」に依存しない（不変条件 5:
// レベルトリガー）。storage キューを引く別のレプリカが新しい config で先に走って
// 行を消していても、このプロセスは自分の次のパスで config から desired set を
// 再導出してラベルを掃除できること。
//
// エッジトリガー実装（既存行を SELECT して差集合だけ掃除する）だと、行を消したのが
// 他者だったパスでラベルが取り残され、二度と掃除されない ---
// rokuban_storage_root_last_success_timestamp_seconds{root="scratch"} が凍結した
// まま残り、その鮮度でアラートせよと書いている docs/operations/monitoring.md の
// 手順が Pod を再起動するまで消えない偽陽性を出し続ける。
func TestStorageSyncWorker_MetricsClearedWhenRowRemovedByAnotherReplica(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()

	w := &StorageSyncWorker{
		Pool: pool, MediaDir: mediaDir, ScratchDir: scratchDir,
		Stat: fakeStat(map[string]diskusage.Usage{
			mediaDir:   {TotalBytes: 100, UsedBytes: 10, AvailableBytes: 90},
			scratchDir: {TotalBytes: 777, UsedBytes: 77, AvailableBytes: 700},
		}, nil),
	}
	if err := runStorageSync(t, w); err != nil {
		t.Fatalf("first Work() error: %v", err)
	}
	if got := promtestutil.ToFloat64(metrics.StorageRootTotalBytes.WithLabelValues("scratch")); got != 777 {
		t.Fatalf("scratch TotalBytes = %v, want 777 before removal", got)
	}

	// storage キューを引く 2 台目の worker レプリカが、新しい config で先に 1 パス
	// 走って scratch 行を消した状況を模す（このプロセスから見ると、config 変更に
	// 気付いたときには DB 行がもう無い）。
	if _, err := pool.Exec(context.Background(), "DELETE FROM storage_sync WHERE root = 'scratch'"); err != nil {
		t.Fatalf("simulating another replica's sweep: %v", err)
	}

	// このプロセスも新しい config（scratch_dir 空）で 1 パス走る。
	w.ScratchDir = ""
	w.Stat = fakeStat(map[string]diskusage.Usage{
		mediaDir: {TotalBytes: 100, UsedBytes: 10, AvailableBytes: 90},
	}, nil)
	if err := runStorageSync(t, w); err != nil {
		t.Fatalf("second Work() error: %v", err)
	}

	if got := promtestutil.ToFloat64(metrics.StorageRootTotalBytes.WithLabelValues("scratch")); got != 0 {
		t.Errorf("scratch TotalBytes = %v, want 0 (stale label must be cleared even when another replica deleted the row first)", got)
	}
	if got := promtestutil.ToFloat64(metrics.StorageRootLastSuccess.WithLabelValues("scratch")); got != 0 {
		t.Errorf("scratch StorageRootLastSuccess = %v, want 0 (a frozen freshness gauge is a permanent false-positive alert)", got)
	}
	if got := promtestutil.ToFloat64(metrics.StorageRootUsedBytes.WithLabelValues("scratch")); got != 0 {
		t.Errorf("scratch UsedBytes = %v, want 0", got)
	}
	if got := promtestutil.ToFloat64(metrics.StorageRootAvailableBytes.WithLabelValues("scratch")); got != 0 {
		t.Errorf("scratch AvailableBytes = %v, want 0", got)
	}

	// media 側は掃除に巻き込まれない（desired set に残っている）。
	if got := promtestutil.ToFloat64(metrics.StorageRootTotalBytes.WithLabelValues("media")); got != 100 {
		t.Errorf("media TotalBytes = %v, want 100 (still desired, must not be swept)", got)
	}
}
