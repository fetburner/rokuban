package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// recordingOpener は開かれたファイル名の順と、**manifest を開いた瞬間の本体の
// 中身**を記録する。掃引（上）が書き込み順序の反転を捕まえるのは最終オフセット
// 1 点だけなので、順序そのものを直接固定する。
type recordingOpener struct {
	order       []string
	docAtOpen   []byte
	docReadErr  error
	sawManifest bool
}

// **書き込み順序の固定**: manifest は必ず最後に開かれ、しかも**開かれた時点で
// 本体は最終形まで書き終わっている**こと（公開点が「完成宣言」として機能する
// 前提そのもの。docs/storage.md §8）。
func TestWrite_ManifestIsCreatedAfterTheDocumentIsComplete(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingOpener{}

	orig := openCatalogFile
	openCatalogFile = func(path string) (catalogFile, error) {
		name := filepath.Base(path)
		rec.order = append(rec.order, name)
		if name == ManifestFilename {
			rec.sawManifest = true
			rec.docAtOpen, rec.docReadErr = os.ReadFile(filepath.Join(filepath.Dir(path), DocumentFilename))
		}
		return os.Create(path)
	}
	defer func() { openCatalogFile = orig }()

	doc := testDoc(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), "ordered")
	genDir, err := Write(dir, doc, DefaultKeep)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	want := []string{DocumentFilename, ManifestFilename}
	if len(rec.order) != len(want) || rec.order[0] != want[0] || rec.order[1] != want[1] {
		t.Fatalf("open order = %v, want %v", rec.order, want)
	}
	if !rec.sawManifest {
		t.Fatal("manifest was never created")
	}
	if rec.docReadErr != nil {
		t.Fatalf("document was not readable when the manifest was created: %v", rec.docReadErr)
	}

	// manifest を開いた時点の本体が、最終的な本体と 1 バイト違わないこと
	// （= 本体を書き終えてから完成宣言している）。
	final, err := os.ReadFile(filepath.Join(genDir, DocumentFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(rec.docAtOpen) != string(final) {
		t.Fatalf("document at manifest creation was %d bytes, final is %d bytes "+
			"(the manifest must be written after the document is complete)",
			len(rec.docAtOpen), len(final))
	}
}

// 同じ秒に 10 本以上書いても、辞書順（= 新しい順の唯一の根拠）が壊れないこと。
func TestWrite_GenerationSuffixKeepsLexicalOrder(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	var names []string
	for i := 0; i < 11; i++ {
		genDir, err := Write(dir, testDoc(at, fmt.Sprintf("gen-%d", i)), 1000)
		if err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		names = append(names, filepath.Base(genDir))
	}

	// 書いた順 = 新しい順なので、名前は辞書順で単調増加していなければならない。
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("generation names are not lexically increasing: %q then %q (all: %v)",
				names[i-1], names[i], names)
		}
	}
	// 最後に書いたものが選ばれること（辞書順が壊れるとここで 2 本目が返る）。
	sel, err := SelectLatest(dir)
	if err != nil {
		t.Fatalf("SelectLatest: %v", err)
	}
	if sel.Generation != names[len(names)-1] {
		t.Errorf("selected %q, want the newest %q", sel.Generation, names[len(names)-1])
	}
}

// ListSnapshots が rescue と同じ優先順を返し、最初の完成エントリが
// SelectLatest の選択と一致すること（verify CLI が rescue の挙動を代弁できる根拠）。
func TestListSnapshots_FirstCompleteMatchesSelectLatest(t *testing.T) {
	dir := t.TempDir()
	writeCompleteGeneration(t, dir, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), "old")
	newest := writeCompleteGeneration(t, dir, time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC), "new")
	// 最新世代の manifest を壊す。
	if err := os.Remove(filepath.Join(newest, ManifestFilename)); err != nil {
		t.Fatal(err)
	}

	statuses, err := ListSnapshots(dir)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	var gotNames []string
	for _, st := range statuses {
		gotNames = append(gotNames, st.Name)
	}
	want := []string{"catalog-20260702T000000Z", "catalog-20260701T000000Z"}
	if len(gotNames) != len(want) {
		t.Fatalf("statuses = %v, want %v", gotNames, want)
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Fatalf("statuses = %v, want %v", gotNames, want)
		}
	}
	if statuses[0].Complete {
		t.Error("the newest generation lost its manifest; it must not be complete")
	}
	if !statuses[1].Complete || statuses[1].Manifest == nil {
		t.Errorf("the previous generation must be complete with a manifest: %+v", statuses[1])
	}

	sel, err := SelectLatest(dir)
	if err != nil {
		t.Fatalf("SelectLatest: %v", err)
	}
	if sel.Generation != statuses[1].Name {
		t.Errorf("SelectLatest picked %q, but the first complete status is %q",
			sel.Generation, statuses[1].Name)
	}
	// 飛ばした世代は黙って落とさず報告される。
	if len(sel.Rejected) != 1 || sel.Rejected[0].Name != "catalog-20260702T000000Z" {
		t.Errorf("rejected = %+v, want the incomplete newest generation", sel.Rejected)
	}
}

// manifest はあっても本体ファイルが無ければ完成世代にならないこと。
func TestVerifyGeneration_RejectsMissingDocumentFile(t *testing.T) {
	dir := t.TempDir()
	genDir := writeCompleteGeneration(t, dir, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), "gen")
	if err := os.Remove(filepath.Join(genDir, DocumentFilename)); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyGeneration(genDir); err == nil {
		t.Fatal("VerifyGeneration accepted a generation with a missing document file")
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
		{"document name mismatch", func(m *Manifest) { m.Document = "elsewhere.json" }},
		{"size mismatch", func(m *Manifest) { m.SizeBytes++ }},
		{"sha256 mismatch", func(m *Manifest) { m.SHA256 = strings.Repeat("0", 64) }},
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
