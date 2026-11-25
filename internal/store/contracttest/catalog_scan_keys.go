package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/store"
)

// runCatalogRestoreKeepsContentScan は、原本の TS 計測が復元で Export に出ること、サイズの違う計測で
// 置き換わること、計測の無い文書で消えることを確かめる。資産の内容を書き直すと計測は捨てられるので、
// 計測は資産の行の後に復元する。
func runCatalogRestoreKeepsContentScan(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	seedCopyRecordings(t, f, 1)
	asset := copyOriginal(10, 1, "originals/scan.ts", "active", nil)
	scans := func(size int64) []catalogdoc.MediaAssetTSScan {
		if size < 0 {
			return nil
		}
		return []catalogdoc.MediaAssetTSScan{{MediaAssetID: 10, ScannedSizeBytes: size}}
	}
	for _, tc := range []struct {
		name string
		size int64
		want []catalogdoc.MediaAssetTSScan
	}{
		{"restored scan is exported", 188, scans(188)},
		{"scan with another size replaces it", 187, scans(187)},
		{"a document without a scan removes it", -1, []catalogdoc.MediaAssetTSScan{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreCopyDoc(t, f, &catalogdoc.Document{
				Version: catalogdoc.Version, MediaAssets: []catalogdoc.MediaAsset{asset}, MediaAssetTSScans: scans(tc.size),
			})
			exported, err := f.Store.Export(context.Background())
			if err != nil {
				t.Fatalf("Export: %v", err)
			}
			if !reflect.DeepEqual(exported.MediaAssetTSScans, tc.want) {
				t.Fatalf("exported scans = %+v, want %+v", exported.MediaAssetTSScans, tc.want)
			}
		})
	}
}

// runCatalogRuleImportKeys は、取り込みキーの往復、旧形式の metadata からの移行、同じキーの重複で
// 文書全体が巻き戻ることを確かめる。
func runCatalogRuleImportKeys(t *testing.T, newStore NewCatalogStore) {
	for _, legacy := range []bool{false, true} {
		name := "current"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			runCatalogRuleImportKeysCase(t, newStore, legacy)
		})
	}
}

// ruleImportKeysDoc は、取り込みキーの検査用の文書を返す。legacy は旧形式（キーの配列が無く metadata だけ）。
func ruleImportKeysDoc(legacy bool) *catalogdoc.Document {
	doc := &catalogdoc.Document{Version: catalogdoc.Version}
	for _, id := range []int64{100, 101} {
		doc.Rules = append(doc.Rules, catalogdoc.Rule{
			ID: id, Name: "original", Enabled: true, Priority: 10, KeepOriginal: "always",
			EncodeProfiles: []string{}, Metadata: json.RawMessage(`{"epgstation":{"ruleId":42},"user":"kept"}`),
			CreatedAt: catalogCopyTime, UpdatedAt: catalogCopyTime,
		})
	}
	// 新形式では metadata の数値も不透明なまま。同定は配列だけで決まる。
	if legacy {
		doc.Rules[1].Metadata = json.RawMessage(`{"epgstation":{"ruleId":"42"}}`)
	} else {
		doc.RuleImportKeys = []catalogdoc.RuleImportKey{{RuleID: 100, Source: "epgstation", ExternalID: "42"}}
	}
	return doc
}

func runCatalogRuleImportKeysCase(t *testing.T, newStore NewCatalogStore, legacy bool) {
	f := newStore(t)
	ctx := context.Background()
	doc := ruleImportKeysDoc(legacy)
	if _, err := f.Store.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	exported, err := f.Store.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []catalogdoc.RuleImportKey{{RuleID: 100, Source: "epgstation", ExternalID: "42"}}
	if !reflect.DeepEqual(exported.RuleImportKeys, want) {
		t.Fatalf("keys = %+v, want %+v", exported.RuleImportKeys, want)
	}
	expectedMetadata := `{"epgstation":{"ruleId":42},"user":"kept"}`
	if legacy {
		expectedMetadata = `{"user":"kept"}`
	}
	assertCatalogImportMetadata(t, exported.Rules[0].Metadata, expectedMetadata)
	assertCatalogImportMetadata(t, exported.Rules[1].Metadata, string(doc.Rules[1].Metadata))
	ruleImportKeysRoundTrip(t, newStore, exported, want, expectedMetadata)

	// 先行するルール更新も含め、重複キーの文書全体が戻る。
	doc.Rules[0].Name = "must roll back"
	if legacy {
		doc.Rules[1].Metadata = json.RawMessage(`{"epgstation":{"ruleId":42}}`)
	} else {
		doc.RuleImportKeys = append(doc.RuleImportKeys, catalogdoc.RuleImportKey{RuleID: 101, Source: "epgstation", ExternalID: "42"})
	}
	if _, err := f.Store.Restore(ctx, doc, store.RestoreOptions{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate restore error = %v, want ErrConflict", err)
	}
	unchanged, err := f.Store.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unchanged.RuleImportKeys, want) || unchanged.Rules[0].Name != "original" {
		t.Fatalf("duplicate restore left partial changes: keys=%+v name=%q", unchanged.RuleImportKeys, unchanged.Rules[0].Name)
	}
}

// ruleImportKeysRoundTrip は、export の文書を JSON の往復を含めて別の DB に 2 回復元し、キーと metadata が
// 変わらないことを確かめる（再適用でも行数を増やさない）。
func ruleImportKeysRoundTrip(t *testing.T, newStore NewCatalogStore, exported *catalogdoc.Document, want []catalogdoc.RuleImportKey, expectedMetadata string) {
	t.Helper()
	raw, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip catalogdoc.Document
	if err := json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatal(err)
	}
	destination := newStore(t)
	for range 2 {
		if _, err := destination.Store.Restore(context.Background(), &roundTrip, store.RestoreOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	after, err := destination.Store.Export(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.RuleImportKeys, want) {
		t.Fatalf("round trip keys = %+v", after.RuleImportKeys)
	}
	assertCatalogImportMetadata(t, after.Rules[0].Metadata, expectedMetadata)
}

// runCatalogEmptyRuleImportKeys は、取り込みキーの無い文書を復元すると、metadata の不透明な値が
// 取り込みキーに変わらず、Export は空の配列を返すことを確かめる。
func runCatalogEmptyRuleImportKeys(t *testing.T, newStore NewCatalogStore) {
	ctx := context.Background()
	doc := &catalogdoc.Document{
		Version:        catalogdoc.Version,
		RuleImportKeys: []catalogdoc.RuleImportKey{},
		Rules: []catalogdoc.Rule{{
			ID: 100, Name: "opaque", KeepOriginal: "always", Metadata: json.RawMessage(`{"epgstation":{"ruleId":42}}`),
			CreatedAt: catalogCopyTime, UpdatedAt: catalogCopyTime,
		}},
	}
	source := newStore(t)
	if _, err := source.Store.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	exported, err := source.Store.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if exported.RuleImportKeys == nil || len(exported.RuleImportKeys) != 0 {
		t.Fatalf("exported keys = %#v, want non-nil empty array", exported.RuleImportKeys)
	}
	raw, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	var restored catalogdoc.Document
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	destination := newStore(t)
	if _, err := destination.Store.Restore(ctx, &restored, store.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	after, err := destination.Store.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.RuleImportKeys) != 0 {
		t.Fatalf("opaque metadata became import key: %+v", after.RuleImportKeys)
	}
	assertCatalogImportMetadata(t, after.Rules[0].Metadata, string(doc.Rules[0].Metadata))
}

// runCatalogRestoreRejectsChapterSpanWithoutOwnership は、所有の行の無い区間を Restore が拒否し、何も残さないことを確かめる。
// 区間は所有の行を参照する（Postgres の外部キー ON DELETE CASCADE と同じ）。
func runCatalogRestoreRejectsChapterSpanWithoutOwnership(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	seedCopyRecordings(t, f, 1)
	label := "CM"
	doc := &catalogdoc.Document{
		Version:               catalogdoc.Version,
		RecordingChapterSpans: []catalogdoc.RecordingChapterSpan{{RecordingID: 1, StartMs: 100, EndMs: 200, Label: &label, Cut: true}},
	}
	if _, err := f.Store.Restore(context.Background(), doc, store.RestoreOptions{}); err == nil {
		t.Fatal("Restore accepted a chapter span without its ownership row")
	}
	exported, err := f.Store.Export(context.Background())
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(exported.RecordingChapterSpans) != 0 {
		t.Fatalf("chapter spans after a rejected restore = %d, want 0", len(exported.RecordingChapterSpans))
	}
}

// runCatalogRestoreDropsOverlappingChapterSpans は、同じ録画で先に入った区間と重なる後続の区間を、
// 排他制約に当たるものとして黙って飛ばすことを確かめる（同一区間も重なりに含む）。重なっていない区間は残る。
// 再実行しても増えない。
func runCatalogRestoreDropsOverlappingChapterSpans(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	seedCopyRecordings(t, f, 1)
	span := func(start, end int64, label string) catalogdoc.RecordingChapterSpan {
		l := label
		return catalogdoc.RecordingChapterSpan{RecordingID: 1, StartMs: start, EndMs: end, Label: &l}
	}
	doc := &catalogdoc.Document{
		Version:                    catalogdoc.Version,
		RecordingChapterOwnerships: []catalogdoc.RecordingChapterOwnership{{RecordingID: 1, AdoptedAt: catalogCopyTime}},
		RecordingChapterSpans: []catalogdoc.RecordingChapterSpan{
			span(100, 200, "first"),
			span(150, 250, "overlaps the first"),
			span(200, 300, "touches the first end"),
		},
	}
	restoreCopyDoc(t, f, doc)
	restoreCopyDoc(t, f, doc)
	exported, err := f.Store.Export(context.Background())
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	var labels []string
	for _, s := range exported.RecordingChapterSpans {
		labels = append(labels, *s.Label)
	}
	if !reflect.DeepEqual(labels, []string{"first", "touches the first end"}) && !reflect.DeepEqual(labels, []string{"touches the first end", "first"}) {
		t.Fatalf("exported chapter spans = %v, want the first and the touching span only", labels)
	}
}

// assertCatalogImportMetadata は metadata の JSON の値が一致することを確かめる（キーの順序は問わない）。
func assertCatalogImportMetadata(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("metadata = %s, want %s", got, want)
	}
}
