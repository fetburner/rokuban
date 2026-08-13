package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
)

// errStopped は「ここでプロセスが落ちた」を模した書き込み停止。
var errStopped = errors.New("injected stop")

// stoppingFile は共有の残バイト数を使い切ったところで書き込みを止めるファイル。
// 実ファイルには停止するまでのバイト列がそのまま残る --- クラッシュ後の
// ディスク上の状態を再現するのが目的なので、後始末はしない。
type stoppingFile struct {
	f         *os.File
	remaining *int64
}

func (w *stoppingFile) Write(p []byte) (int, error) {
	if *w.remaining <= 0 {
		return 0, errStopped
	}
	if int64(len(p)) > *w.remaining {
		n, err := w.f.Write(p[:*w.remaining])
		*w.remaining = 0
		if err != nil {
			return n, err
		}
		return n, errStopped
	}
	n, err := w.f.Write(p)
	*w.remaining -= int64(n)
	return n, err
}

func (w *stoppingFile) Sync() error  { return w.f.Sync() }
func (w *stoppingFile) Close() error { return w.f.Close() }

// stopWritesAfter は catalog の書き込みを合計 n バイトで止めるよう差し替え、
// 元に戻す関数を返す。
func stopWritesAfter(n int64) (restore func()) {
	remaining := n
	orig := openCatalogFile
	openCatalogFile = func(path string) (catalogFile, error) {
		f, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		return &stoppingFile{f: f, remaining: &remaining}, nil
	}
	return func() { openCatalogFile = orig }
}

// generationByteSize は 1 世代を書き切るのに必要な総バイト数を測る。
func generationByteSize(t *testing.T, doc *Document) int64 {
	t.Helper()
	dir := t.TempDir()
	genDir, err := Write(dir, doc, DefaultKeep)
	if err != nil {
		t.Fatalf("probe Write: %v", err)
	}
	var total int64
	for _, name := range []string{DocumentFilename, ManifestFilename} {
		info, err := os.Stat(filepath.Join(genDir, name))
		if err != nil {
			t.Fatalf("probe stat %s: %v", name, err)
		}
		total += info.Size()
	}
	return total
}

// **失敗注入の本体**: catalog 出力を「任意の地点」で止めても、rescue が選ぶのは
// 常に完成した世代であること（書きかけを選ばない / 1 世代前へ落ちる）。
//
// 停止点は 0 バイトから世代を書き切るまでの全オフセットを総当たりする
// （ディレクトリだけできて本体 0 バイト / 本体の途中 / 本体だけ完成して manifest
// 無し / manifest の途中、が全部この掃引に入る）。
func TestSelectLatest_StopAtAnyOffsetNeverSelectsTornGeneration(t *testing.T) {
	oldAt := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	newAt := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	total := generationByteSize(t, testDoc(newAt, "new"))
	if total < 100 {
		t.Fatalf("probe size = %d, too small to be a real generation", total)
	}

	// 1 世代前（フォールバック先）。掃引の間ずっと据え置く。
	dir := t.TempDir()
	if _, err := Write(dir, testDoc(oldAt, "old"), DefaultKeep); err != nil {
		t.Fatalf("writing previous generation: %v", err)
	}
	newGenDir := filepath.Join(Dir(dir), "catalog-20260702T000000Z")

	for stop := int64(0); stop <= total; stop++ {
		// 前の停止点が残した書きかけを消してから次の停止点を試す。
		if err := os.RemoveAll(newGenDir); err != nil {
			t.Fatal(err)
		}

		restore := stopWritesAfter(stop)
		_, writeErr := Write(dir, testDoc(newAt, "new"), DefaultKeep)
		restore()

		// 期待値（両方向）: manifest の中身まで書き切った停止点だけが新しい世代
		// として選ばれる。total-1 は encoder が最後に足す改行 1 バイトが欠けた
		// だけで、manifest の中身は完成している。
		wantNew := stop >= total-1
		if stop == total && writeErr != nil {
			t.Fatalf("stop=%d (nothing was stopped): Write: %v", stop, writeErr)
		}
		if stop < total && writeErr == nil {
			t.Fatalf("stop=%d: Write succeeded although the write was stopped", stop)
		}

		sel, err := SelectLatest(dir)
		if err != nil {
			t.Fatalf("stop=%d: SelectLatest: %v", stop, err)
		}
		if sel.Legacy {
			t.Fatalf("stop=%d: selected an unverified legacy file", stop)
		}
		doc, err := Load(sel.DocumentPath)
		if err != nil {
			t.Fatalf("stop=%d: selected catalog does not load: %v", stop, err)
		}
		title := doc.Recordings[0].Title

		if wantNew {
			if title != "new" || sel.Generation != "catalog-20260702T000000Z" {
				t.Fatalf("stop=%d: selected %q (%s), want the new generation",
					stop, title, sel.Generation)
			}
			if _, err := VerifyGeneration(filepath.Join(Dir(dir), sel.Generation)); err != nil {
				t.Fatalf("stop=%d: selected a generation that does not verify: %v", stop, err)
			}
			continue
		}
		if title != "old" || sel.Generation != "catalog-20260701T000000Z" {
			t.Fatalf("stop=%d: selected %q (%s), want the previous generation",
				stop, title, sel.Generation)
		}
		// 飛ばした世代は黙って捨てず、理由付きで報告されること。
		if len(sel.Rejected) != 1 || sel.Rejected[0].Name != "catalog-20260702T000000Z" {
			t.Fatalf("stop=%d: rejected = %+v, want the torn generation with a reason",
				stop, sel.Rejected)
		}
	}
}

// 本体が完成しても manifest が無ければ完成世代にならないこと（公開点は manifest）。
func TestVerifyGeneration_RequiresManifest(t *testing.T) {
	dir := t.TempDir()
	genDir := writeCompleteGeneration(t, dir, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), "gen")
	if err := os.Remove(filepath.Join(genDir, ManifestFilename)); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyGeneration(genDir); err == nil {
		t.Fatal("VerifyGeneration accepted a generation without a manifest")
	}
	if _, err := SelectLatest(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SelectLatest = %v, want os.ErrNotExist", err)
	}
}

// **parse 成功では検出できない壊れ方**: JSON 文字列の中の 1 バイトが化けても
// parse は通る。sha256 だけがこれを捕まえる（docs/storage.md §8 の根拠）。
func TestVerifyGeneration_RejectsSilentByteFlip(t *testing.T) {
	dir := t.TempDir()
	genDir := writeCompleteGeneration(t, dir, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), "title")

	docPath := filepath.Join(genDir, DocumentFilename)
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	flipped := []byte(strings.Replace(string(raw), `"title"`, `"titlf"`, 1))
	if string(flipped) == string(raw) {
		t.Fatal("test setup: nothing was flipped")
	}
	if len(flipped) != len(raw) {
		t.Fatalf("test setup: size changed (%d -> %d); this must be a same-size flip", len(raw), len(flipped))
	}
	if err := os.WriteFile(docPath, flipped, 0o644); err != nil {
		t.Fatal(err)
	}

	// 前提の確認: 壊れた本体は JSON としては読めてしまう（サイズも同じ）。
	if _, err := Load(docPath); err != nil {
		t.Fatalf("test setup: flipped document should still parse, got %v", err)
	}
	if _, err := VerifyGeneration(genDir); err == nil {
		t.Fatal("VerifyGeneration accepted a silently corrupted document")
	}
}

// manifest の各項目が完成判定に効いていること（1 つずつ壊して両方向で見る）。
func TestVerifyGeneration_RejectsBrokenManifests(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(m *Manifest)
	}{
		{"manifestVersion missing", func(m *Manifest) { m.ManifestVersion = 0 }},
		{"manifestVersion from the future", func(m *Manifest) { m.ManifestVersion = ManifestVersion + 1 }},
		{"schemaVersion missing", func(m *Manifest) { m.SchemaVersion = 0 }},
		{"schemaVersion from the future", func(m *Manifest) { m.SchemaVersion = Version + 1 }},
		{"generation name mismatch", func(m *Manifest) { m.Generation = "catalog-19700101T000000Z" }},
		{"document not listed", func(m *Manifest) { m.Document = "elsewhere.json" }},
		{"no files", func(m *Manifest) { m.Files = nil }},
		{"file missing from disk", func(m *Manifest) {
			m.Files = append(m.Files, ManifestFile{Name: "extra.json", SizeBytes: 1, SHA256: "00"})
		}},
		{"size mismatch", func(m *Manifest) { m.Files[0].SizeBytes++ }},
		{"sha256 mismatch", func(m *Manifest) { m.Files[0].SHA256 = strings.Repeat("0", 64) }},
		{"path escape in file name", func(m *Manifest) { m.Files[0].Name = "../catalog.json" }},
		{"manifest lists itself", func(m *Manifest) {
			m.Files = append(m.Files, ManifestFile{Name: ManifestFilename, SizeBytes: 1, SHA256: "00"})
		}},
	}

	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			genDir := writeCompleteGeneration(t, dir, at, "gen")
			// 壊す前は通ること（壊した側だけ見て満足しない）。
			if _, err := VerifyGeneration(genDir); err != nil {
				t.Fatalf("baseline VerifyGeneration: %v", err)
			}

			manifestPath := filepath.Join(genDir, ManifestFilename)
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var m Manifest
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&m)
			broken, err := json.Marshal(&m)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, broken, 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := VerifyGeneration(genDir); err == nil {
				t.Fatal("VerifyGeneration accepted a broken manifest")
			}
			if _, err := SelectLatest(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("SelectLatest = %v, want os.ErrNotExist (the only generation is broken)", err)
			}
		})
	}
}

// 最新世代が壊れていたら 1 世代前の完成世代から復元できること。
func TestSelectLatest_FallsBackToPreviousComplete(t *testing.T) {
	dir := t.TempDir()
	writeCompleteGeneration(t, dir, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), "old")
	newest := writeCompleteGeneration(t, dir, time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC), "new")

	// 最新世代の本体を 1 バイト削る（サイズ不一致）。
	docPath := filepath.Join(newest, DocumentFilename)
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(docPath, raw[:len(raw)-1], 0o644); err != nil {
		t.Fatal(err)
	}

	sel, err := SelectLatest(dir)
	if err != nil {
		t.Fatalf("SelectLatest: %v", err)
	}
	if sel.Generation != "catalog-20260701T000000Z" {
		t.Fatalf("selected %q, want the previous generation", sel.Generation)
	}
	if len(sel.Rejected) != 1 || sel.Rejected[0].Name != "catalog-20260702T000000Z" {
		t.Fatalf("rejected = %+v, want the newest generation with a reason", sel.Rejected)
	}
	doc, err := Load(sel.DocumentPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if doc.Recordings[0].Title != "old" {
		t.Errorf("title = %q, want %q", doc.Recordings[0].Title, "old")
	}
}

// manifest を持たない旧形式のフラットファイルは、完成世代が 1 つも無いときだけ
// 最後の手段として使われること（完成世代があるならそちらが優先）。
func TestSelectLatest_LegacyFlatFileIsLastResort(t *testing.T) {
	dir := t.TempDir()
	catalogDir := Dir(dir)
	if err := os.MkdirAll(catalogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 世代ディレクトリより**新しい**時刻の旧形式ファイル。
	legacyPath := filepath.Join(catalogDir, "catalog-20260909T000000Z.json")
	legacyDoc, err := json.Marshal(testDoc(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC), "legacy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, legacyDoc, 0o644); err != nil {
		t.Fatal(err)
	}

	sel, err := SelectLatest(dir)
	if err != nil {
		t.Fatalf("SelectLatest: %v", err)
	}
	if !sel.Legacy || sel.DocumentPath != legacyPath {
		t.Fatalf("selection = %+v, want the legacy flat file", sel)
	}

	// 完成世代を足すと、旧形式より古くてもそちらが選ばれる（検証できる方を採る）。
	writeCompleteGeneration(t, dir, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), "verified")
	sel, err = SelectLatest(dir)
	if err != nil {
		t.Fatalf("SelectLatest after adding a generation: %v", err)
	}
	if sel.Legacy || sel.Generation != "catalog-20260701T000000Z" {
		t.Fatalf("selection = %+v, want the verified generation", sel)
	}
}

// 世代ディレクトリ導入前に書かれた（manifest を持たない）catalog からも復元
// できること。ただし「検証できていない」ことが結果に出る。
func TestRescueLatest_LegacyFlatFileStillRestores(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	catalogDir := Dir(mediaDir)
	if err := os.MkdirAll(catalogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := testDoc(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), "legacy title")
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalogDir, "catalog-20260701T000000Z.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := RescueLatest(context.Background(), pool, mediaDir, "default")
	if err != nil {
		t.Fatalf("RescueLatest: %v", err)
	}
	if !result.LegacyCatalog {
		t.Error("LegacyCatalog should be true (the caller must be able to report that completeness was not verified)")
	}
	if result.Generation != "" {
		t.Errorf("Generation = %q, want empty for a legacy flat file", result.Generation)
	}
	if result.Recordings != 1 {
		t.Errorf("rescued recordings = %d, want 1", result.Recordings)
	}
}

// 掃除の方針（docs/storage.md §8「不完全世代の保持と掃除」）:
// 最新側の不完全世代は進行中かもしれないので残し、より新しい完成世代が
// できたら消す。
func TestPrune_IncompleteGenerationLifecycle(t *testing.T) {
	dir := t.TempDir()
	catalogDir := Dir(dir)
	writeCompleteGeneration(t, dir, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), "old")

	// 進行中に見える世代（manifest 未着）。
	inflight := filepath.Join(catalogDir, "catalog-20260702T000000Z")
	if err := os.MkdirAll(inflight, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inflight, DocumentFilename), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Prune(catalogDir, DefaultKeep); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(inflight); err != nil {
		t.Fatalf("in-flight generation was removed: %v", err)
	}

	// より新しい完成世代ができたら掃除される。
	writeCompleteGeneration(t, dir, time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC), "new")
	if err := Prune(catalogDir, DefaultKeep); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(inflight); !os.IsNotExist(err) {
		t.Fatalf("stale incomplete generation survived: %v", err)
	}
	// 完成世代は両方とも keep の内側なので残る。
	got := dirEntryNames(t, catalogDir)
	for _, name := range []string{"catalog-20260701T000000Z", "catalog-20260703T000000Z"} {
		if !got[name] {
			t.Errorf("expected %q to remain, got %v", name, got)
		}
	}
}

// keep の勘定は完成世代だけで行い、不完全世代は枠を食わないこと。
func TestPrune_IncompleteDoesNotConsumeKeepSlots(t *testing.T) {
	dir := t.TempDir()
	catalogDir := Dir(dir)
	writeCompleteGeneration(t, dir, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), "a")
	writeCompleteGeneration(t, dir, time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC), "b")
	// 2 つの完成世代の間に挟まる不完全世代。
	broken := filepath.Join(catalogDir, "catalog-20260715T000000Z")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCompleteGeneration(t, dir, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), "c")

	if err := Prune(catalogDir, 2); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	got := dirEntryNames(t, catalogDir)
	if got["catalog-20260715T000000Z"] {
		t.Errorf("incomplete generation should have been removed: %v", got)
	}
	// keep=2 の枠は完成世代だけで埋まる（不完全世代が枠を食っていたら
	// catalog-20260702T000000Z が消える）。
	for _, name := range []string{"catalog-20260801T000000Z", "catalog-20260702T000000Z"} {
		if !got[name] {
			t.Errorf("expected %q to remain, got %v", name, got)
		}
	}
	if got["catalog-20260701T000000Z"] {
		t.Errorf("oldest complete generation should have been pruned: %v", got)
	}
}
