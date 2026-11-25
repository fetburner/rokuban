package contracttest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/store"
)

// catalogCopyStore は契約テストの物理コピーの置き場。Store の storeName と揃える（各実装の NewCatalog で渡す）。
const catalogCopyStore = "test-store"

var catalogCopyTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// runCatalogRestoreLegacyFailureReported は、failureReported の無い旧形式の文書を Restore が
// 品質イベントの配列から列を埋めることを確かめる。
func runCatalogRestoreLegacyFailureReported(t *testing.T, newStore NewCatalogStore) {
	tests := []struct {
		name   string
		events string
		want   bool
	}{
		{name: "failed event", events: `[{"event":"recording.failed"}]`, want: true},
		{name: "other event", events: `[{"event":"recording.record-broken"}]`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newStore(t)
			ctx := context.Background()
			doc := roundTripCatalogDocument()
			doc.Recordings[0].FailureReported = nil
			doc.Recordings[0].QualityEvents = []byte(tt.events)
			if _, err := f.Store.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
				t.Fatalf("Restore of a legacy document: %v", err)
			}
			exported, err := f.Store.Export(ctx)
			if err != nil {
				t.Fatalf("Export after restore: %v", err)
			}
			got := exportedRecording(t, exported, 30)
			if got.FailureReported == nil || *got.FailureReported != tt.want {
				t.Fatalf("restored failureReported = %v, want %t", got.FailureReported, tt.want)
			}
		})
	}
}

// runCatalogRestoreKeepsCurrentGenerationOfLogicalRow は、古い世代の文書が論理資産の現行世代・
// サイズ・内容の検査値を戻さないことを確かめる。両方とも Export の論理資産の値で観測する。
func runCatalogRestoreKeepsCurrentGenerationOfLogicalRow(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	seedCopyRecordings(t, f, 1)
	current, older := int64(8), int64(7)
	shaCurrent, shaOlder := strings.Repeat("88", 32), strings.Repeat("77", 32)
	asset := func(generation int64, relPath string, size int64, sha string) catalogdoc.MediaAsset {
		return catalogdoc.MediaAsset{
			ID: 10, RecordingID: 1, Kind: "seek_tiles", RelPath: relPath, SizeBytes: size, State: "active",
			CreatedAt: catalogCopyTime, UpdatedAt: catalogCopyTime,
			DerivedGeneration: &generation, Generation: &generation, SHA256: &sha, Store: catalogCopyStore,
		}
	}
	restoreCopyAssets(t, f, asset(current, "tiles/current.g8.jpg", 100, shaCurrent))
	restoreCopyAssets(t, f, asset(older, "tiles/old.g7.jpg", 80, shaOlder))
	copies := exportCopiesOf(t, f, 10)
	if len(copies) != 2 {
		t.Fatalf("copies of asset 10 = %d, want 2 (current and older generation)", len(copies))
	}
	for _, c := range copies {
		if c.DerivedGeneration == nil || *c.DerivedGeneration != current || c.SizeBytes != 100 || c.SHA256 == nil || *c.SHA256 != shaCurrent {
			t.Fatalf("logical asset 10 after an older catalog = derived %v, size %d, sha %v; want generation %d, size 100, sha %s",
				c.DerivedGeneration, c.SizeBytes, c.SHA256, current, shaCurrent)
		}
	}
}

// runCatalogRestoreKeepsLiveOwnerAgainstTombstone は、同じ identity の墓石を復元しても、そのパスの
// 生きたコピーを墓石に置き換えないことを確かめる。墓石は別の（合成の）パスに置かれ、再実行でも増えない。
func runCatalogRestoreKeepsLiveOwnerAgainstTombstone(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	seedCopyRecordings(t, f, 1)
	const path = "originals/live.ts"
	live := copyOriginal(10, 1, path, "active", nil)
	deletedAt := catalogCopyTime.Add(time.Minute)
	tombstone := copyOriginal(10, 1, path, "deleted", &deletedAt)
	restoreCopyAssets(t, f, live)
	restoreCopyAssets(t, f, tombstone)
	first := exportCopiesOf(t, f, 10)
	tombstonePath := onlyDeletedPath(t, first)
	if tombstonePath == path {
		t.Fatalf("tombstone took the live copy's path %q", path)
	}
	assertCopyStates(t, first, 1, 1)
	restoreCopyAssets(t, f, live)
	restoreCopyAssets(t, f, tombstone)
	again := exportCopiesOf(t, f, 10)
	assertCopyStates(t, again, 1, 1)
	if got := onlyDeletedPath(t, again); got != tombstonePath {
		t.Fatalf("tombstone path after rerun = %q, want %q", got, tombstonePath)
	}
	for _, c := range again {
		if c.State == "active" && c.RelPath != path {
			t.Fatalf("live copy path = %q, want %q", c.RelPath, path)
		}
	}
}

// runCatalogRestoreDoesNotResurrectCopyDeletedAfterStat は、ファイルを確かめた後に削除された
// コピーを、古い生きたスナップショットで復活させないことを確かめる。
func runCatalogRestoreDoesNotResurrectCopyDeletedAfterStat(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	seedCopyRecordings(t, f, 1)
	const path = "originals/deleted-after-stat.ts"
	deletedAt := catalogCopyTime.Add(-time.Second)
	restoreCopyAssets(t, f, copyOriginal(10, 1, path, "deleted", &deletedAt))
	restoreCopyAssets(t, f, copyOriginal(10, 1, path, "active", nil))
	assertCopyStates(t, exportCopiesOf(t, f, 10), 0, 1)
}

// runCatalogRestoreKeepsConflictingTombstoneAndContinuesGeneration は、同じパスの別の資産の墓石が
// 生きたコピーと衝突しても、復元が止まらず、墓石を合成パスに置くことを確かめる。
func runCatalogRestoreKeepsConflictingTombstoneAndContinuesGeneration(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	seedCopyRecordings(t, f, 1, 2, 3)
	const path = "originals/reused-by-new-copy.ts"
	restoreCopyAssets(t, f, copyOriginal(10, 1, path, "active", nil))
	deletedAt := catalogCopyTime.Add(-time.Minute)
	// 古い文書は、生きた所有者が変わる前のものである。墓石の INSERT は、先に生きた所有者がいても
	// この世代を止めてはならない。
	restoreCopyDoc(t, f, &catalogdoc.Document{Version: catalogdoc.Version, MediaAssets: []catalogdoc.MediaAsset{
		copyOriginal(11, 2, path, "deleted", &deletedAt),
		copyOriginal(12, 3, "originals/following.ts", "active", nil),
	}})
	assertCopyStates(t, exportCopiesOf(t, f, 10), 1, 0)
	if live := exportCopiesOf(t, f, 10); live[0].State != "active" || live[0].RelPath != path {
		t.Fatalf("current path owner = %s at %q, want active at %q", live[0].State, live[0].RelPath, path)
	}
	historical := exportCopiesOf(t, f, 11)
	tombstonePath := onlyDeletedPath(t, historical)
	if tombstonePath == path || len(tombstonePath) <= 1024 || strings.Contains(tombstonePath, "/") {
		t.Fatalf("restored tombstone path length = %d, has separator %t, want a single component longer than 1024 bytes",
			len(tombstonePath), strings.Contains(tombstonePath, "/"))
	}
	if following := exportCopiesOf(t, f, 12); len(following) != 1 {
		t.Fatalf("later asset copies = %d, want one after the tombstone conflict", len(following))
	}
}

// runCatalogRestoreRestoresTwoTombstonesOfOneIdentity は、同じ論理資産の墓石が 2 つの異なるパスにあるとき、
// 両方を復元することを確かめる。
func runCatalogRestoreRestoresTwoTombstonesOfOneIdentity(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	seedCopyRecordings(t, f, 1)
	const first, second = "originals/identity-first.ts", "originals/identity-second.ts"
	deletedAt := catalogCopyTime.Add(-time.Minute)
	restoreCopyDoc(t, f, &catalogdoc.Document{Version: catalogdoc.Version, MediaAssets: []catalogdoc.MediaAsset{
		copyOriginal(10, 1, first, "deleted", &deletedAt),
		copyOriginal(10, 1, second, "deleted", &deletedAt),
	}})
	paths := deletedPaths(t, exportCopiesOf(t, f, 10))
	if len(paths) != 2 || !paths[first] || !paths[second] {
		t.Fatalf("tombstone paths = %v, want both %q and %q", paths, first, second)
	}
}

// runCatalogRestoreTwoTombstonesOfOneIdentityGetDistinctSyntheticPaths は、同じ identity の墓石 2 つが
// どちらも生きた別資産のパスと衝突するとき、合成パスを 2 つの別々の単一成分として置くことを確かめる。
func runCatalogRestoreTwoTombstonesOfOneIdentityGetDistinctSyntheticPaths(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	seedCopyRecordings(t, f, 1, 2, 3)
	const first, second = "originals/owned-first.ts", "originals/owned-second.ts"
	restoreCopyDoc(t, f, &catalogdoc.Document{Version: catalogdoc.Version, MediaAssets: []catalogdoc.MediaAsset{
		copyOriginal(20, 2, first, "active", nil),
		copyOriginal(21, 3, second, "active", nil),
	}})
	deletedAt := catalogCopyTime.Add(-time.Minute)
	restoreCopyDoc(t, f, &catalogdoc.Document{Version: catalogdoc.Version, MediaAssets: []catalogdoc.MediaAsset{
		copyOriginal(10, 1, first, "deleted", &deletedAt),
		copyOriginal(10, 1, second, "deleted", &deletedAt),
	}})
	tombstones := exportCopiesOf(t, f, 10)
	paths := deletedPaths(t, tombstones)
	if len(paths) != 2 {
		t.Fatalf("tombstone paths = %v, want two distinct synthetic paths", paths)
	}
	for p := range paths {
		if p == first || p == second || len(p) <= 1024 || strings.Contains(p, "/") {
			t.Fatalf("tombstone path %q is not a reserved single-component synthetic path", p)
		}
	}
	for _, owner := range []struct {
		id   int64
		path string
	}{{20, first}, {21, second}} {
		copies := exportCopiesOf(t, f, owner.id)
		if len(copies) != 1 || copies[0].State != "active" || copies[0].RelPath != owner.path {
			t.Fatalf("live owner %d = %+v, want one active copy at %q", owner.id, copies, owner.path)
		}
	}
}

// runCatalogRestoreRerunKeepsOneSyntheticTombstone は、同じ文書を 2 回復元しても、合成の墓石が 1 つのままで、
// 生きた所有者が変わらないことを確かめる。
func runCatalogRestoreRerunKeepsOneSyntheticTombstone(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	seedCopyRecordings(t, f, 1, 2)
	const path = "originals/rerun-conflict.ts"
	deletedAt := catalogCopyTime.Add(-time.Minute)
	doc := &catalogdoc.Document{Version: catalogdoc.Version, MediaAssets: []catalogdoc.MediaAsset{
		copyOriginal(10, 1, path, "active", nil),
		copyOriginal(11, 2, path, "deleted", &deletedAt),
	}}
	restoreCopyDoc(t, f, doc)
	restoreCopyDoc(t, f, doc)
	if owner := exportCopiesOf(t, f, 10); len(owner) != 1 || owner[0].State != "active" {
		t.Fatalf("current path owner after two restores = %+v, want one active copy", owner)
	}
	if n := len(deletedPaths(t, exportCopiesOf(t, f, 11))); n != 1 {
		t.Fatalf("historical tombstones after two restores = %d, want exactly one", n)
	}
}

// runCatalogRestoreAcceptsSyntheticTombstonePath は、以前の復元が置いた合成パスの墓石を、そのパスのまま受け入れる
// ことを確かめる。合成パスを作り直さない。
func runCatalogRestoreAcceptsSyntheticTombstonePath(t *testing.T, newStore NewCatalogStore) {
	// 合成パスの墓石を作る（別の資産が原パスを生きたまま持つ）。
	source := newStore(t)
	seedCopyRecordings(t, source, 1, 2)
	const original = "originals/already-moved.ts"
	deletedAt := catalogCopyTime.Add(-time.Minute)
	restoreCopyAssets(t, source, copyOriginal(11, 2, original, "active", nil))
	restoreCopyAssets(t, source, copyOriginal(10, 1, original, "deleted", &deletedAt))
	synthetic := onlyDeletedPath(t, exportCopiesOf(t, source, 10))

	// 合成パスの文書を、空の DB に復元する。
	f := newStore(t)
	seedCopyRecordings(t, f, 1)
	restoreCopyAssets(t, f, copyOriginal(10, 1, synthetic, "deleted", &deletedAt))
	copies := exportCopiesOf(t, f, 10)
	if len(copies) != 1 || copies[0].RelPath != synthetic {
		t.Fatalf("tombstone copies = %+v, want the synthetic path %q kept as is", copies, synthetic)
	}
}

// runCatalogRestoreAllowsPathReuseByAnotherAsset は、原パス P を生きた資産 B が使い、同じ P に資産 A の墓石を
// 復元するとき、B は P で生きたままで、A の墓石は合成パスに 1 つだけ置かれることを確かめる。
// 前提（A の墓石が合成パスにある）は、先に B を P に置いてから A を削除として復元して作る。
func runCatalogRestoreAllowsPathReuseByAnotherAsset(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	seedCopyRecordings(t, f, 1, 2)
	const path = "originals/reused-across-assets.ts"
	deletedAt := catalogCopyTime.Add(-time.Minute)
	restoreCopyAssets(t, f, copyOriginal(20, 2, path, "active", nil))
	restoreCopyAssets(t, f, copyOriginal(10, 1, path, "deleted", &deletedAt))
	restoreCopyDoc(t, f, &catalogdoc.Document{Version: catalogdoc.Version, MediaAssets: []catalogdoc.MediaAsset{
		copyOriginal(20, 2, path, "active", nil),
		copyOriginal(10, 1, path, "deleted", &deletedAt),
	}})
	owner := exportCopiesOf(t, f, 20)
	if len(owner) != 1 || owner[0].State != "active" || owner[0].RelPath != path {
		t.Fatalf("current path owner = %+v, want one active copy at %q", owner, path)
	}
	old := exportCopiesOf(t, f, 10)
	assertCopyStates(t, old, 0, 1)
	if onlyDeletedPath(t, old) == path {
		t.Fatalf("the old asset's tombstone took the path %q of the live owner", path)
	}
}

// runCatalogRestoreDoesNotResurrectMovedTombstoneAtReusedPath は、墓石が合成パスに移ったあと、同じ P を
// 生きた別資産が使っているとき、古い生きた文書で墓石の資産を復活させないことを確かめる。
// 前提（A の墓石が合成パスにあり、P を B が生きて持つ）は、Restore だけで作る。
func runCatalogRestoreDoesNotResurrectMovedTombstoneAtReusedPath(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	seedCopyRecordings(t, f, 1, 2)
	const path = "originals/reused-before-catalog-restore.ts"
	deletedAt := catalogCopyTime.Add(-time.Minute)
	restoreCopyAssets(t, f, copyOriginal(20, 2, path, "active", nil))
	restoreCopyAssets(t, f, copyOriginal(10, 1, path, "deleted", &deletedAt))
	// 古い文書の生きた行（A が P で生きていた時点）を復元しても、A は復活しない。
	restoreCopyAssets(t, f, copyOriginal(10, 1, path, "active", nil))
	assertCopyStates(t, exportCopiesOf(t, f, 10), 0, 1)
	owner := exportCopiesOf(t, f, 20)
	if len(owner) != 1 || owner[0].State != "active" || owner[0].RelPath != path {
		t.Fatalf("current path owner = %+v, want one active copy at %q", owner, path)
	}
}

// seedCopyRecordings は、資産の親になる録画を restore で入れる。
func seedCopyRecordings(t *testing.T, f CatalogFixture, ids ...int64) {
	t.Helper()
	doc := &catalogdoc.Document{Version: catalogdoc.Version}
	for _, id := range ids {
		doc.Recordings = append(doc.Recordings, catalogdoc.Recording{
			ID: id, Source: "manual", Site: "default", NetworkID: 1, ServiceID: 1, EventID: int32(id),
			ServiceName: "test", ChannelType: "GR", Channel: "27", Title: "copy contract", IsFree: true,
			ProgramStartAt: catalogCopyTime, ProgramDurationMs: 1000, Status: "recording",
			CreatedAt: catalogCopyTime, UpdatedAt: catalogCopyTime,
		})
	}
	restoreCopyDoc(t, f, doc)
}

// copyOriginal は録画 recordingID の原本（世代 0）の論理行と、その物理コピーの 1 行を表す。
func copyOriginal(id, recordingID int64, relPath, state string, deletedAt *time.Time) catalogdoc.MediaAsset {
	return catalogdoc.MediaAsset{
		ID: id, RecordingID: recordingID, Kind: "original", RelPath: relPath, SizeBytes: 100,
		State: state, DeletedAt: deletedAt, CreatedAt: catalogCopyTime, UpdatedAt: catalogCopyTime,
		Store: catalogCopyStore,
	}
}

// restoreCopyDoc は文書を復元する。
func restoreCopyDoc(t *testing.T, f CatalogFixture, document *catalogdoc.Document) {
	t.Helper()
	if _, err := f.Store.Restore(context.Background(), document, store.RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
}

// restoreCopyAssets は資産だけを含む文書を復元する。
func restoreCopyAssets(t *testing.T, f CatalogFixture, assets ...catalogdoc.MediaAsset) {
	t.Helper()
	restoreCopyDoc(t, f, &catalogdoc.Document{Version: catalogdoc.Version, MediaAssets: assets})
}

// exportCopiesOf は Export の中の資産 id の物理コピー（置き場が catalogCopyStore のもの）を返す。
func exportCopiesOf(t *testing.T, f CatalogFixture, id int64) []catalogdoc.MediaAsset {
	t.Helper()
	exported, err := f.Store.Export(context.Background())
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	var out []catalogdoc.MediaAsset
	for _, asset := range exported.MediaAssets {
		if asset.ID == id {
			out = append(out, asset)
		}
	}
	return out
}

// assertCopyStates は、コピーの active と deleted の件数を検査する。
func assertCopyStates(t *testing.T, copies []catalogdoc.MediaAsset, active, deleted int) {
	t.Helper()
	var gotActive, gotDeleted int
	for _, c := range copies {
		switch c.State {
		case "active":
			gotActive++
		case "deleted":
			gotDeleted++
		}
	}
	if gotActive != active || gotDeleted != deleted {
		t.Fatalf("copies = %d active, %d deleted; want %d active, %d deleted", gotActive, gotDeleted, active, deleted)
	}
}

// deletedPaths は墓石の RelPath の集合を返す。
func deletedPaths(t *testing.T, copies []catalogdoc.MediaAsset) map[string]bool {
	t.Helper()
	paths := map[string]bool{}
	for _, c := range copies {
		if c.State == "deleted" {
			paths[c.RelPath] = true
		}
	}
	return paths
}

// onlyDeletedPath は墓石が 1 つだけあることを確かめ、その RelPath を返す。
func onlyDeletedPath(t *testing.T, copies []catalogdoc.MediaAsset) string {
	t.Helper()
	paths := deletedPaths(t, copies)
	if len(paths) != 1 {
		t.Fatalf("tombstone paths = %v, want exactly one", paths)
	}
	for p := range paths {
		return p
	}
	return ""
}

// exportedRecording は文書の録画を ID で返す。無ければ検査を失敗させる。
func exportedRecording(t *testing.T, doc *catalogdoc.Document, id int64) catalogdoc.Recording {
	t.Helper()
	for _, r := range doc.Recordings {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("recording %d is missing from the export", id)
	return catalogdoc.Recording{}
}
