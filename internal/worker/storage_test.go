package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/diskusage"
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
