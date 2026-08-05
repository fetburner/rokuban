package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Write が catalog/ 配下に JSON を書き、LatestPath がそれを見つけること。
func TestWriteAndLatest(t *testing.T) {
	dir := t.TempDir()
	doc := &Document{
		Version:    Version,
		ExportedAt: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
		Recordings: []Recording{{
			ID: 1, Source: "manual", Site: "default",
			NetworkID: 1, ServiceID: 1, EventID: 1,
			ServiceName: "NHKG", ChannelType: "GR", Channel: "27",
			Title: "test", ProgramStartAt: time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC),
			ProgramDurationMs: 1800000, Status: "finished",
			QualityEvents: json.RawMessage(`[]`),
			CreatedAt:     time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC),
			UpdatedAt:     time.Date(2026, 7, 30, 2, 0, 0, 0, time.UTC),
		}},
	}

	path, err := Write(dir, doc, 7)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	wantName := "catalog-20260730T120000Z.json"
	if filepath.Base(path) != wantName {
		t.Errorf("path base = %q, want %q", filepath.Base(path), wantName)
	}

	latest, err := LatestPath(dir)
	if err != nil {
		t.Fatalf("LatestPath: %v", err)
	}
	if latest != path {
		t.Errorf("LatestPath = %q, want %q", latest, path)
	}

	loaded, err := Load(path)
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

// Prune が最新 keep 件だけ残し、古い catalog JSON を消すこと（両方向）。
func TestPrune_KeepsNewest(t *testing.T) {
	dir := t.TempDir()
	catalogDir := Dir(dir)
	if err := os.MkdirAll(catalogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 辞書順で古い → 新しい 5 件。
	names := []string{
		"catalog-20260701T000000Z.json",
		"catalog-20260702T000000Z.json",
		"catalog-20260703T000000Z.json",
		"catalog-20260704T000000Z.json",
		"catalog-20260705T000000Z.json",
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(catalogDir, name), []byte(`{"version":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// catalog 以外のファイルと .tmp 残骸。
	if err := os.WriteFile(filepath.Join(catalogDir, "notes.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalogDir, "catalog-20260705T000000Z.json.tmp"), []byte("tmp"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Prune(catalogDir, 2); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	entries, err := os.ReadDir(catalogDir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}

	// 最新 2 件と notes.txt は残る。.tmp と古い 3 件は消える。
	wantKeep := []string{
		"catalog-20260704T000000Z.json",
		"catalog-20260705T000000Z.json",
		"notes.txt",
	}
	wantGone := []string{
		"catalog-20260701T000000Z.json",
		"catalog-20260702T000000Z.json",
		"catalog-20260703T000000Z.json",
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
	catalogDir := Dir(dir)
	if err := os.MkdirAll(catalogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		name := Filename(time.Date(2026, 7, i, 0, 0, 0, 0, time.UTC))
		if err := os.WriteFile(filepath.Join(catalogDir, name), []byte(`{"version":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Prune(catalogDir, 0); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	entries, err := os.ReadDir(catalogDir)
	if err != nil {
		t.Fatal(err)
	}
	var jsonCount int
	for _, e := range entries {
		if isCatalogJSON(e.Name()) {
			jsonCount++
		}
	}
	if jsonCount != DefaultKeep {
		t.Errorf("remaining catalog files = %d, want DefaultKeep=%d", jsonCount, DefaultKeep)
	}
}

// Write 経由で keep を超えたら古いものが消えること（export 経路の結合）。
func TestWrite_PrunesOldGenerations(t *testing.T) {
	dir := t.TempDir()
	for day := 1; day <= 5; day++ {
		doc := &Document{
			Version:    Version,
			ExportedAt: time.Date(2026, 7, day, 0, 0, 0, 0, time.UTC),
		}
		if _, err := Write(dir, doc, 3); err != nil {
			t.Fatalf("Write day=%d: %v", day, err)
		}
	}

	entries, err := os.ReadDir(Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	// 最新 3 日分。
	want := map[string]bool{
		"catalog-20260703T000000Z.json": true,
		"catalog-20260704T000000Z.json": true,
		"catalog-20260705T000000Z.json": true,
	}
	for _, e := range entries {
		if !want[e.Name()] {
			t.Errorf("unexpected remaining file %q", e.Name())
		}
	}
}
