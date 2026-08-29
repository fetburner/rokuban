package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testDoc は catalog 本体の最小サンプル。title で世代を見分ける。
func testDoc(exportedAt time.Time, title string) *Document {
	return &Document{
		Version:    Version,
		ExportedAt: exportedAt,
		Recordings: []Recording{{
			ID: 1, Source: "manual", Site: "default",
			NetworkID: 1, ServiceID: 1, EventID: 1,
			ServiceName: "NHKG", ChannelType: "GR", Channel: "27",
			Title: title, ProgramStartAt: time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC),
			ProgramDurationMs: 1800000, Status: "finished",
			QualityEvents: json.RawMessage(`[]`),
			CreatedAt:     time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC),
			UpdatedAt:     time.Date(2026, 7, 30, 2, 0, 0, 0, time.UTC),
		}},
	}
}

// Write が世代ディレクトリ（本体 + manifest）を書き、SelectLatest がそれを
// 完成世代として選ぶこと。
func TestWriteAndSelectLatest(t *testing.T) {
	dir := t.TempDir()
	doc := testDoc(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC), "test")

	genDir, err := Write(dir, doc, 7)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	wantName := "catalog-20260730T120000Z"
	if filepath.Base(genDir) != wantName {
		t.Errorf("generation dir = %q, want %q", filepath.Base(genDir), wantName)
	}
	for _, name := range []string{"catalog.json", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(genDir, name)); err != nil {
			t.Errorf("expected %s in generation dir: %v", name, err)
		}
	}

	sel, err := SelectLatest(dir)
	if err != nil {
		t.Fatalf("SelectLatest: %v", err)
	}
	if sel.Generation != wantName {
		t.Errorf("selected generation = %q, want %q", sel.Generation, wantName)
	}
	if sel.DocumentPath != filepath.Join(genDir, "catalog.json") {
		t.Errorf("document path = %q", sel.DocumentPath)
	}
	if sel.Manifest == nil || sel.Manifest.SchemaVersion != Version {
		t.Errorf("manifest = %+v", sel.Manifest)
	}
	if len(sel.Rejected) != 0 {
		t.Errorf("rejected = %+v, want none", sel.Rejected)
	}

	loaded, err := Load(sel.DocumentPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Version != Version {
		t.Errorf("version = %d, want %d", loaded.Version, Version)
	}
	if len(loaded.Recordings) != 1 || loaded.Recordings[0].Title != "test" {
		t.Errorf("recordings = %+v", loaded.Recordings)
	}
}

// manifest.json の on-disk 形をリテラルで固定する。Write と VerifyGeneration が
// 同じ Manifest 構造体を使い回すので、struct を読み書きするだけの往復テストは
// フィールド名や manifestVersion の値を綴り間違えても通ってしまう
// （CLAUDE.md「実装の定数と比較するテストは何も主張していない」）。issue #441 の
// レビューで指摘された穴を塞ぐため、JSON を map[string]any で読み直してキー名と
// manifestVersion をリテラルで検証する。
func TestWrite_ManifestOnDiskShape(t *testing.T) {
	dir := t.TempDir()
	genDir, err := Write(dir, testDoc(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC), "shape"), 7)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(genDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	// リテラル 2: files[] → 単一 sizeBytes/sha256 への形式変更で 1 から上げた版。
	if got, want := m["manifestVersion"], float64(2); got != want {
		t.Errorf("manifestVersion = %v, want %v", got, want)
	}
	if got, want := m["document"], "catalog.json"; got != want {
		t.Errorf("document = %v, want %q", got, want)
	}
	for _, key := range []string{"sizeBytes", "sha256", "generation", "schemaVersion", "exportedAt"} {
		if _, ok := m[key]; !ok {
			t.Errorf("manifest.json is missing key %q: %v", key, m)
		}
	}
	if _, present := m["files"]; present {
		t.Errorf("manifest.json still has the old files[] key: %v", m)
	}
}

// 同じ ExportedAt で 2 回書いても既存世代を上書きせず、別の世代になること
// （世代名を再利用しない。docs/storage.md §8）。
func TestWrite_DoesNotReuseGenerationName(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	first, err := Write(dir, testDoc(at, "first"), 7)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}
	second, err := Write(dir, testDoc(at, "second"), 7)
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if first == second {
		t.Fatalf("second write reused the generation dir %q", first)
	}

	// 1 本目の中身は書き換わっていない。
	firstDoc, err := Load(filepath.Join(first, DocumentFilename))
	if err != nil {
		t.Fatalf("Load first: %v", err)
	}
	if firstDoc.Recordings[0].Title != "first" {
		t.Errorf("first generation title = %q, want %q", firstDoc.Recordings[0].Title, "first")
	}
	// 2 本目が新しい側として選ばれる。
	sel, err := SelectLatest(dir)
	if err != nil {
		t.Fatalf("SelectLatest: %v", err)
	}
	if sel.Generation != filepath.Base(second) {
		t.Errorf("selected %q, want %q", sel.Generation, filepath.Base(second))
	}
}

// Prune が完成世代を新しい順に keep 件だけ残すこと（両方向）。
func TestPrune_KeepsNewestComplete(t *testing.T) {
	dir := t.TempDir()
	for day := 1; day <= 5; day++ {
		writeCompleteGeneration(t, dir, time.Date(2026, 7, day, 0, 0, 0, 0, time.UTC), "gen")
	}
	catalogDir := Dir(dir)
	// catalog- で始まらないファイルと、旧方式の残骸。
	if err := os.WriteFile(filepath.Join(catalogDir, "notes.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalogDir, "catalog-20260705T000000Z.json.tmp"), []byte("tmp"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Prune(catalogDir, 2); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	got := dirEntryNames(t, catalogDir)
	wantKeep := []string{
		"catalog-20260704T000000Z",
		"catalog-20260705T000000Z",
		"notes.txt",
	}
	wantGone := []string{
		"catalog-20260701T000000Z",
		"catalog-20260702T000000Z",
		"catalog-20260703T000000Z",
		"catalog-20260705T000000Z.json.tmp",
	}
	for _, name := range wantKeep {
		if !got[name] {
			t.Errorf("expected %q to remain, got %v", name, got)
		}
	}
	for _, name := range wantGone {
		if got[name] {
			t.Errorf("expected %q to be pruned, still present", name)
		}
	}
}

// keep=0 は DefaultKeep に落ちること。
func TestPrune_DefaultKeep(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 10; i++ {
		writeCompleteGeneration(t, dir, time.Date(2026, 7, i, 0, 0, 0, 0, time.UTC), "gen")
	}
	if err := Prune(Dir(dir), 0); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	entries, err := os.ReadDir(Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != DefaultKeep {
		t.Errorf("remaining generations = %d, want DefaultKeep=%d", len(entries), DefaultKeep)
	}
}

// Write 経由で keep を超えたら古い世代が消えること（export 経路の結合）。
func TestWrite_PrunesOldGenerations(t *testing.T) {
	dir := t.TempDir()
	for day := 1; day <= 5; day++ {
		if _, err := Write(dir, testDoc(time.Date(2026, 7, day, 0, 0, 0, 0, time.UTC), "gen"), 3); err != nil {
			t.Fatalf("Write day=%d: %v", day, err)
		}
	}

	got := dirEntryNames(t, Dir(dir))
	if len(got) != 3 {
		t.Fatalf("entries = %v, want 3", got)
	}
	for _, name := range []string{
		"catalog-20260703T000000Z",
		"catalog-20260704T000000Z",
		"catalog-20260705T000000Z",
	} {
		if !got[name] {
			t.Errorf("expected %q to remain, got %v", name, got)
		}
	}
}

// writeCompleteGeneration は完成世代を 1 つ書く（テストの下ごしらえ）。
// Prune の判定そのものを測りたいので、書く側では刈らない（keep を十分大きく取る）。
func writeCompleteGeneration(t *testing.T, mediaDir string, at time.Time, title string) string {
	t.Helper()
	genDir, err := Write(mediaDir, testDoc(at, title), 1000)
	if err != nil {
		t.Fatalf("Write(%s): %v", at, err)
	}
	return genDir
}

func dirEntryNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	return got
}
