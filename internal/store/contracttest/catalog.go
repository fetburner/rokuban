package contracttest

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/store"
)

// CatalogFixture は catalog 契約の実装と、競合状態を作るためのテスト用操作をまとめる。
type CatalogFixture struct {
	Store store.Catalog
	Pass  store.RulerPass
	// RulerInputSites は Store に構成した全 site（重複なし）。契約テストは Restore の後、
	// 各 site の版がちょうど 1 進むことを確かめる。
	RulerInputSites []string
	// BumpRulerInputRevision は site の ruler 入力の版を 1 進める。版の初期値を作るために使う。
	BumpRulerInputRevision func(ctx context.Context, site string) error
	// InsertRecordingAndAsset は録画と原本アセットを別 commit で作る。
	InsertRecordingAndAsset func(ctx context.Context, eventID int32) error
	// Reset は catalog が保護するテーブルを空にする。
	Reset func(t *testing.T)
	// FailAtMediaAssetWrite installs a failure after the restore has written earlier tables.
	FailAtMediaAssetWrite func(t *testing.T)
	// FailAtRulerInputRevisionWrite fails after an earlier site's revision has advanced.
	FailAtRulerInputRevisionWrite func(t *testing.T)
	// BeginLabelRuleReevaluationLock は再評価と同じロックを保持し、解放関数を返す。
	BeginLabelRuleReevaluationLock func(t *testing.T) (release func())
	// WaitForRestoreLock は Restore が再評価ロックで待っており、その時点で保護対象表への
	// 書き込みロックをまだ持っていないことを確認する。
	WaitForRestoreLock func(t *testing.T)
}

// NewCatalogStore は catalog 契約の実装と状態作成用フィクスチャを作る。
type NewCatalogStore func(t *testing.T) CatalogFixture

// RunCatalog は実在する store 実装に catalog の契約を実行する。
func RunCatalog(t *testing.T, newStore NewCatalogStore) {
	t.Helper()
	t.Run("export uses one snapshot during concurrent ingest", func(t *testing.T) {
		runCatalogConcurrentExport(t, newStore)
	})
	t.Run("restore round trips an exported document", func(t *testing.T) {
		runCatalogRoundTrip(t, newStore)
	})
	t.Run("restore failure rolls back", func(t *testing.T) {
		runCatalogRestoreRollback(t, newStore)
	})
	t.Run("restore bumps revisions once", func(t *testing.T) {
		runCatalogRestoreBumpsRulerInputRevisions(t, newStore)
	})
	t.Run("restore rolls back on revision failure", func(t *testing.T) {
		runCatalogRestoreRulerInputRevisionRollback(t, newStore)
	})
	t.Run("restore marks missing assets deleted and keeps their statistics", func(t *testing.T) {
		runCatalogRestoreMissingAssets(t, newStore)
	})
	t.Run("restore waits before writing for label rule reevaluation", func(t *testing.T) {
		runCatalogRestoreSerialization(t, newStore)
	})
}

func runCatalogRestoreMissingAssets(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	ctx := context.Background()
	result, err := f.Store.Restore(ctx, roundTripCatalogDocument(), store.RestoreOptions{
		MissingAssetIDs: map[int64]struct{}{40: {}},
	})
	if err != nil {
		t.Fatalf("Restore with a missing asset: %v", err)
	}
	if result.MissingMediaFiles != 1 {
		t.Fatalf("MissingMediaFiles = %d, want 1", result.MissingMediaFiles)
	}
	exported, err := f.Store.Export(ctx)
	if err != nil {
		t.Fatalf("Export after restore: %v", err)
	}
	states := map[int64]catalogdoc.MediaAsset{}
	for _, asset := range exported.MediaAssets {
		states[asset.ID] = asset
	}
	if got := states[40]; got.State != "deleted" || got.DeletedAt == nil {
		t.Fatalf("missing asset 40 = state %q, deleted_at %v; want deleted with a deletion time", got.State, got.DeletedAt)
	}
	if got := states[41]; got.State != "active" || got.DeletedAt != nil {
		t.Fatalf("asset 41 not listed as missing = state %q, deleted_at %v; want active without a deletion time", got.State, got.DeletedAt)
	}
	if len(exported.DropStats) != 1 || len(exported.DropPositions) != 1 || len(exported.MediaAssetTSScans) != 1 {
		t.Fatalf("statistics of the deleted asset = drop_stats %d, drop_positions %d, ts_scans %d; want 1/1/1",
			len(exported.DropStats), len(exported.DropPositions), len(exported.MediaAssetTSScans))
	}
}

func runCatalogRestoreRollback(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	ctx := context.Background()
	seedCatalogRulerInputRevisions(t, f, ctx)
	revisionsBefore := catalogRulerInputRevisions(t, f, ctx)
	f.FailAtMediaAssetWrite(t)
	if _, err := f.Store.Restore(ctx, roundTripCatalogDocument(), store.RestoreOptions{}); err == nil {
		t.Fatal("Restore succeeded despite the injected media asset write failure")
	}
	assertCatalogEmpty(t, f, ctx)
	if revisionsAfter := catalogRulerInputRevisions(t, f, ctx); !reflect.DeepEqual(revisionsAfter, revisionsBefore) {
		t.Fatalf("ruler input revisions after failed restore = %v, want unchanged %v", revisionsAfter, revisionsBefore)
	}
}

func runCatalogRestoreBumpsRulerInputRevisions(t *testing.T, newStore NewCatalogStore) {
	tests := []struct {
		name     string
		document func() *catalogdoc.Document
	}{
		{
			name: "no rules",
			document: func() *catalogdoc.Document {
				doc := roundTripCatalogDocument()
				doc.Rules = nil
				doc.Recordings[0].RuleID = nil
				return doc
			},
		},
		{
			name: "multiple rules",
			document: func() *catalogdoc.Document {
				doc := roundTripCatalogDocument()
				second := doc.Rules[0]
				second.ID = 11
				second.Name = "catalog round trip second rule"
				doc.Rules = append(doc.Rules, second)
				return doc
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newStore(t)
			ctx := context.Background()
			seedCatalogRulerInputRevisions(t, f, ctx)
			revisionsBefore := catalogRulerInputRevisions(t, f, ctx)
			if _, err := f.Store.Restore(ctx, tt.document(), store.RestoreOptions{}); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			revisionsAfter := catalogRulerInputRevisions(t, f, ctx)
			for _, site := range f.RulerInputSites {
				revisionAfter, ok := revisionsAfter[site]
				if !ok {
					t.Fatalf("ruler input revision for site %q was not read after restore", site)
				}
				if want := revisionsBefore[site] + 1; revisionAfter != want {
					t.Errorf("%s ruler input revision after restore = %d, want %d", site, revisionAfter, want)
				}
			}
		})
	}
}

func runCatalogRestoreRulerInputRevisionRollback(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	ctx := context.Background()
	seedCatalogRulerInputRevisions(t, f, ctx)
	revisionsBefore := catalogRulerInputRevisions(t, f, ctx)
	f.FailAtRulerInputRevisionWrite(t)
	if _, err := f.Store.Restore(ctx, roundTripCatalogDocument(), store.RestoreOptions{}); err == nil {
		t.Fatal("Restore succeeded despite the injected ruler input revision failure")
	}
	assertCatalogEmpty(t, f, ctx)
	if revisionsAfter := catalogRulerInputRevisions(t, f, ctx); !reflect.DeepEqual(revisionsAfter, revisionsBefore) {
		t.Fatalf("ruler input revisions after failed restore = %v, want unchanged %v", revisionsAfter, revisionsBefore)
	}
}

func seedCatalogRulerInputRevisions(t *testing.T, f CatalogFixture, ctx context.Context) {
	t.Helper()
	for range 2 {
		for _, site := range f.RulerInputSites {
			if err := f.BumpRulerInputRevision(ctx, site); err != nil {
				t.Fatalf("seeding ruler input revision for %q: %v", site, err)
			}
		}
	}
	for _, site := range f.RulerInputSites {
		revision, err := f.Pass.GetRulerInputRevision(ctx, site)
		if err != nil {
			t.Fatalf("reading seeded ruler input revision for %q: %v", site, err)
		}
		if revision != 2 {
			t.Fatalf("seeded ruler input revision for %q = %d, want 2", site, revision)
		}
	}
}

func catalogRulerInputRevisions(t *testing.T, f CatalogFixture, ctx context.Context) map[string]int64 {
	t.Helper()
	revisions := make(map[string]int64, len(f.RulerInputSites))
	for _, site := range f.RulerInputSites {
		revision, err := f.Pass.GetRulerInputRevision(ctx, site)
		if err != nil {
			t.Fatalf("GetRulerInputRevision(%q): %v", site, err)
		}
		revisions[site] = revision
	}
	return revisions
}

func assertCatalogEmpty(t *testing.T, f CatalogFixture, ctx context.Context) {
	t.Helper()
	doc, err := f.Store.Export(ctx)
	if err != nil {
		t.Fatalf("Export after failed restore: %v", err)
	}
	value := reflect.ValueOf(doc).Elem()
	type nonEmptyField struct {
		name  string
		count int
	}
	var nonEmpty []nonEmptyField
	for i := range value.NumField() {
		field := value.Field(i)
		if field.Kind() == reflect.Slice && field.Len() != 0 {
			nonEmpty = append(nonEmpty, nonEmptyField{name: value.Type().Field(i).Name, count: field.Len()})
		}
	}
	if len(nonEmpty) != 0 {
		t.Errorf("catalog rows after failed restore = %v; want all catalog tables empty", nonEmpty)
	}
}

func runCatalogConcurrentExport(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	ctx := context.Background()
	stop := make(chan struct{})
	var inserted atomic.Int64
	var insertErr atomic.Pointer[error]
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for eventID := int32(10_000); ; eventID++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := f.InsertRecordingAndAsset(ctx, eventID); err != nil {
				writeErr := err
				insertErr.CompareAndSwap(nil, &writeErr)
				return
			}
			inserted.Add(1)
		}
	}()

	const minInserts = 10
	deadline := time.Now().Add(60 * time.Second)
	for iteration := 0; iteration < 100 || inserted.Load() < minInserts; iteration++ {
		if time.Now().After(deadline) || (iteration >= 100 && insertErr.Load() != nil) {
			break
		}
		doc, err := f.Store.Export(ctx)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("Export: %v", err)
		}
		recordingIDs := make(map[int64]struct{}, len(doc.Recordings))
		for _, recording := range doc.Recordings {
			recordingIDs[recording.ID] = struct{}{}
		}
		for _, asset := range doc.MediaAssets {
			if _, ok := recordingIDs[asset.RecordingID]; !ok {
				close(stop)
				wg.Wait()
				t.Fatalf("media asset %d references recording %d not present in exported recordings", asset.ID, asset.RecordingID)
			}
		}
	}
	close(stop)
	wg.Wait()
	if count := inserted.Load(); count < minInserts {
		reason := "the concurrent writer did not report an error"
		if err := insertErr.Load(); err != nil {
			reason = (*err).Error()
		}
		t.Fatalf("concurrent writer inserted %d rows, want at least %d: %s", count, minInserts, reason)
	}
}

func runCatalogRoundTrip(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	ctx := context.Background()
	seed := roundTripCatalogDocument()
	if _, err := f.Store.Restore(ctx, seed, store.RestoreOptions{}); err != nil {
		t.Fatalf("seeding catalog through Restore: %v", err)
	}
	exported, err := f.Store.Export(ctx)
	if err != nil {
		t.Fatalf("Export before reset: %v", err)
	}
	assertCatalogExportReadbacks(t, exported, seed)
	f.Reset(t)
	if _, err := f.Store.Restore(ctx, exported, store.RestoreOptions{}); err != nil {
		t.Fatalf("Restore into empty database: %v", err)
	}
	reexported, err := f.Store.Export(ctx)
	if err != nil {
		t.Fatalf("Export after restore: %v", err)
	}
	exported.ExportedAt = time.Time{}
	reexported.ExportedAt = time.Time{}
	if !reflect.DeepEqual(reexported, exported) {
		t.Fatalf("document after restore differs from exported document:\n got: %#v\nwant: %#v", reexported, exported)
	}
}

func assertCatalogExportReadbacks(t *testing.T, got, want *catalogdoc.Document) {
	t.Helper()
	gotPolicies := append([]catalogdoc.RecordingEncodePolicy(nil), got.RecordingEncodePolicies...)
	wantPolicies := append([]catalogdoc.RecordingEncodePolicy(nil), want.RecordingEncodePolicies...)
	for i := range gotPolicies {
		gotPolicies[i].CreatedAt = gotPolicies[i].CreatedAt.UTC()
		gotPolicies[i].UpdatedAt = gotPolicies[i].UpdatedAt.UTC()
	}
	for i := range wantPolicies {
		wantPolicies[i].CreatedAt = wantPolicies[i].CreatedAt.UTC()
		wantPolicies[i].UpdatedAt = wantPolicies[i].UpdatedAt.UTC()
	}
	gotPurgeRequests := append([]catalogdoc.RecordingPurgeRequest(nil), got.RecordingPurgeRequests...)
	wantPurgeRequests := append([]catalogdoc.RecordingPurgeRequest(nil), want.RecordingPurgeRequests...)
	for i := range gotPurgeRequests {
		gotPurgeRequests[i].RequestedAt = gotPurgeRequests[i].RequestedAt.UTC()
	}
	for i := range wantPurgeRequests {
		wantPurgeRequests[i].RequestedAt = wantPurgeRequests[i].RequestedAt.UTC()
	}
	gotAssets := append([]catalogdoc.MediaAsset(nil), got.MediaAssets...)
	wantAssets := append([]catalogdoc.MediaAsset(nil), want.MediaAssets...)
	for i := range gotAssets {
		gotAssets[i].CreatedAt = gotAssets[i].CreatedAt.UTC()
		gotAssets[i].UpdatedAt = gotAssets[i].UpdatedAt.UTC()
		gotAssets[i].DeletedAt = catalogUTC(gotAssets[i].DeletedAt)
	}
	for i := range wantAssets {
		wantAssets[i].CreatedAt = wantAssets[i].CreatedAt.UTC()
		wantAssets[i].UpdatedAt = wantAssets[i].UpdatedAt.UTC()
		wantAssets[i].DeletedAt = catalogUTC(wantAssets[i].DeletedAt)
	}
	for _, field := range []struct {
		name      string
		got, want any
	}{
		{"RecordingEncodePolicies", gotPolicies, wantPolicies},
		{"RecordingPurgeRequests", gotPurgeRequests, wantPurgeRequests},
		{"MediaAssets", gotAssets, wantAssets},
		{"DropStats", got.DropStats, want.DropStats},
		{"DropPositions", got.DropPositions, want.DropPositions},
		{"MediaAssetTSScans", got.MediaAssetTSScans, want.MediaAssetTSScans},
	} {
		reportCatalogDiff(t, "Catalog.Export."+field.name, reflect.ValueOf(field.got), reflect.ValueOf(field.want))
	}
	if len(got.Recordings) != len(want.Recordings) {
		t.Fatalf("Catalog.Export.Recordings count = %d, want %d", len(got.Recordings), len(want.Recordings))
	}
	for i := range got.Recordings {
		actual, expected := got.Recordings[i], want.Recordings[i]
		actual.CreatedAt, actual.UpdatedAt = actual.CreatedAt.UTC(), actual.UpdatedAt.UTC()
		actual.ProgramStartAt = actual.ProgramStartAt.UTC()
		actual.StartedAt, actual.EndedAt = catalogUTC(actual.StartedAt), catalogUTC(actual.EndedAt)
		actual.DeletedAt, actual.SupersededAt, actual.PurgedAt = catalogUTC(actual.DeletedAt), catalogUTC(actual.SupersededAt), catalogUTC(actual.PurgedAt)
		expected.CreatedAt, expected.UpdatedAt = expected.CreatedAt.UTC(), expected.UpdatedAt.UTC()
		expected.ProgramStartAt = expected.ProgramStartAt.UTC()
		expected.StartedAt, expected.EndedAt = catalogUTC(expected.StartedAt), catalogUTC(expected.EndedAt)
		expected.DeletedAt, expected.SupersededAt, expected.PurgedAt = catalogUTC(expected.DeletedAt), catalogUTC(expected.SupersededAt), catalogUTC(expected.PurgedAt)
		actual.Extended = canonicalCatalogJSON(t, actual.Extended)
		actual.Genres = canonicalCatalogJSON(t, actual.Genres)
		actual.QualityEvents = canonicalCatalogJSON(t, actual.QualityEvents)
		expected.Extended = canonicalCatalogJSON(t, expected.Extended)
		expected.Genres = canonicalCatalogJSON(t, expected.Genres)
		expected.QualityEvents = canonicalCatalogJSON(t, expected.QualityEvents)
		reportCatalogDiff(t, "Catalog.Export.Recordings["+strconv.Itoa(i)+"]", reflect.ValueOf(actual), reflect.ValueOf(expected))
	}
}

// reportCatalogDiff は got と want の最初の違いを、要素の添字とフィールド名つきで t.Errorf に出す。
// 件数が違うときは件数だけを出す。
func reportCatalogDiff(t *testing.T, name string, got, want reflect.Value) {
	t.Helper()
	switch got.Kind() {
	case reflect.Slice:
		if got.Len() != want.Len() {
			t.Errorf("%s count = %d, want %d", name, got.Len(), want.Len())
			return
		}
		for i := range got.Len() {
			reportCatalogDiff(t, name+"["+strconv.Itoa(i)+"]", got.Index(i), want.Index(i))
		}
	case reflect.Struct:
		for i := range got.NumField() {
			if !reflect.DeepEqual(got.Field(i).Interface(), want.Field(i).Interface()) {
				t.Errorf("%s.%s = %s, want %s", name, got.Type().Field(i).Name, catalogValueText(got.Field(i)), catalogValueText(want.Field(i)))
			}
		}
	default:
		if !reflect.DeepEqual(got.Interface(), want.Interface()) {
			t.Errorf("%s = %s, want %s", name, catalogValueText(got), catalogValueText(want))
		}
	}
}

// catalogValueText は失敗メッセージ用に、ポインタを辿り、バイト列を文字列で表す。
func catalogValueText(v reflect.Value) string {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "nil"
		}
		v = v.Elem()
	}
	if b, ok := v.Interface().([]byte); ok {
		return string(b)
	}
	if b, ok := v.Interface().(json.RawMessage); ok {
		return string(b)
	}
	return fmt.Sprintf("%v", v.Interface())
}

func catalogUTC(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func canonicalCatalogJSON(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decoding catalog JSON value %q: %v", raw, err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding catalog JSON value %q: %v", raw, err)
	}
	return canonical
}

func runCatalogRestoreSerialization(t *testing.T, newStore NewCatalogStore) {
	f := newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	doc := roundTripCatalogDocument()
	release := f.BeginLabelRuleReevaluationLock(t)
	released := false
	done := make(chan error, 1)
	finished := false
	defer func() {
		if !released {
			release()
		}
		cancel()
		if !finished {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("Restore goroutine did not stop after its context was canceled")
			}
		}
	}()
	go func() {
		_, err := f.Store.Restore(ctx, doc, store.RestoreOptions{})
		done <- err
	}()
	f.WaitForRestoreLock(t)
	select {
	case err := <-done:
		finished = true
		release()
		released = true
		t.Fatalf("Restore returned while re-evaluation lock was held: %v", err)
	default:
	}
	release()
	released = true
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatalf("Restore after lock release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Restore did not finish after the re-evaluation lock was released")
	}
}

func roundTripCatalogDocument() *catalogdoc.Document {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	started, ended := at.Add(time.Minute), at.Add(2*time.Minute)
	deletedAt, purgeRequestedAt, supersededAt, purgedAt := at.Add(3*time.Minute), at.Add(4*time.Minute), at.Add(5*time.Minute), at.Add(6*time.Minute)
	ruleID, spanLabel := int64(10), "CM"
	description := "round trip description"
	profile, deletedProfile, pidType, elapsed := "h264", "h265", "video", int64(1500)
	zeroGeneration := int64(0)
	return &catalogdoc.Document{
		Version: catalogdoc.Version, ExportedAt: at,
		Rules: []catalogdoc.Rule{{
			ID: 10, Name: "catalog round trip", Description: "contract", Enabled: true, Priority: 3,
			KeepOriginal: "always", EncodeProfiles: []string{"h264"}, FilenameTemplate: "",
			Metadata: []byte(`{}`), CreatedAt: at, UpdatedAt: at,
			TextMatches: []catalogdoc.RuleTextMatch{{Seq: 0, Target: "name", Mode: "keyword", Value: "round trip"}},
		}},
		LabelRules: []catalogdoc.LabelRule{{
			ID: 20, Key: "series", Value: "round trip", Keyword: "round trip", Priority: 5,
			CreatedAt: at, UpdatedAt: at,
		}},
		ProgramSnapshots: []catalogdoc.ProgramSnapshot{{
			Site: "default", ProgramID: 700, Title: "round trip", StartAt: started, DurationMs: 60_000,
			NetworkID: 32736, ServiceID: 1024, ChannelType: "GR", Channel: "27", EventID: 700,
			ServiceName: "test", UpdatedAt: at,
		}},
		ProgramIntents:   []catalogdoc.ProgramIntent{{Site: "default", ProgramID: 700, Action: "record", CreatedAt: at, UpdatedAt: at}},
		ProgramOverrides: []catalogdoc.ProgramOverride{{Site: "default", ProgramID: 700, Overrides: []byte(`{"title":"override"}`), CreatedAt: at, UpdatedAt: at}},
		Recordings: []catalogdoc.Recording{{
			ID: 30, RuleID: &ruleID, Source: "manual", Site: "default", NetworkID: 32736, ServiceID: 1024,
			EventID: 701, ServiceName: "test", ChannelType: "GR", Channel: "27", Title: "round trip",
			Description: &description, Extended: []byte(`{"episode":1}`), Genres: []byte(`[1,2]`), IsFree: true,
			ProgramStartAt: started, ProgramDurationMs: 120_000, Status: "finished", StartedAt: &started,
			EndedAt: &ended, QualityEvents: []byte(`[{"at":"2026-01-02T03:09:05Z","event":"broken","reason":{"type":"io-error"}}]`),
			DeletedAt: &deletedAt, SupersededAt: &supersededAt, PurgedAt: &purgedAt, CreatedAt: at, UpdatedAt: at,
		}},
		RecordingPurgeRequests: []catalogdoc.RecordingPurgeRequest{{RecordingID: 30, RequestedAt: purgeRequestedAt}},
		RecordingWatched:       []catalogdoc.RecordingWatched{{RecordingID: 30, WatchedAt: at}},
		RecordingEncodePolicies: []catalogdoc.RecordingEncodePolicy{{
			RecordingID: 30, KeepOriginal: "always", EncodeProfiles: []string{"h264"}, CMDetect: true,
			CreatedAt: at, UpdatedAt: at,
		}},
		MediaAssets: []catalogdoc.MediaAsset{{
			ID: 40, RecordingID: 30, Kind: "original", RelPath: "sites/default/round-trip.m2ts",
			SizeBytes: 100, State: "active", CreatedAt: at, UpdatedAt: at,
		}, {
			ID: 41, RecordingID: 30, Kind: "encoded", Profile: &profile, RelPath: "sites/default/round-trip.mp4",
			SizeBytes: 80, State: "active", CreatedAt: at, UpdatedAt: at, DerivedGeneration: &zeroGeneration,
		}, {
			ID: 42, RecordingID: 30, Kind: "encoded", Profile: &deletedProfile, RelPath: "sites/default/deleted.mp4",
			SizeBytes: 70, State: "deleted", DeletedAt: &deletedAt, CreatedAt: at, UpdatedAt: at, DerivedGeneration: &zeroGeneration,
		}},
		DropStats:                  []catalogdoc.DropStat{{MediaAssetID: 40, Pid: 256, Packets: 100, Drops: 2, Errors: 1, Scrambled: 0, PidType: &pidType}},
		DropPositions:              []catalogdoc.DropPosition{{MediaAssetID: 40, ByteOffset: 188, Pid: 256, ElapsedMs: &elapsed}},
		MediaAssetTSScans:          []catalogdoc.MediaAssetTSScan{{MediaAssetID: 40, ScannedSizeBytes: 100}},
		RecordingChapterOwnerships: []catalogdoc.RecordingChapterOwnership{{RecordingID: 30, AdoptedAt: at}},
		RecordingChapterSpans:      []catalogdoc.RecordingChapterSpan{{RecordingID: 30, StartMs: 100, EndMs: 200, Label: &spanLabel, Cut: true}},
	}
}
