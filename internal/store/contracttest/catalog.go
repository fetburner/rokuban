package contracttest

import (
	"context"
	"reflect"
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
	// InsertRecordingAndAsset は録画と原本アセットを別 commit で作る。
	InsertRecordingAndAsset func(ctx context.Context, eventID int32) error
	// Reset は catalog が保護するテーブルを空にする。
	Reset func(t *testing.T)
	// FailAtMediaAssetWrite installs a failure after the restore has written earlier tables.
	FailAtMediaAssetWrite func(t *testing.T)
	// CatalogCounts は rules / recordings / media_assets の行数を返す。
	CatalogCounts func(t *testing.T) (rules, recordings, mediaAssets int)
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
	t.Run("restore rolls back earlier writes after a later failure", func(t *testing.T) {
		runCatalogRestoreRollback(t, newStore)
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
	f.FailAtMediaAssetWrite(t)
	if _, err := f.Store.Restore(context.Background(), roundTripCatalogDocument(), store.RestoreOptions{}); err == nil {
		t.Fatal("Restore succeeded despite the injected media asset write failure")
	}
	rules, recordings, assets := f.CatalogCounts(t)
	if rules != 0 || recordings != 0 || assets != 0 {
		t.Fatalf("rows after failed restore = rules %d, recordings %d, assets %d; want 0/0/0", rules, recordings, assets)
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
	ruleID, spanLabel := int64(10), "CM"
	profile, pidType, elapsed := "h264", "video", int64(1500)
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
			ProgramStartAt: started, ProgramDurationMs: 120_000, Status: "finished", StartedAt: &started,
			EndedAt: &ended, QualityEvents: []byte(`[]`), CreatedAt: at, UpdatedAt: at,
		}},
		RecordingWatched: []catalogdoc.RecordingWatched{{RecordingID: 30, WatchedAt: at}},
		RecordingEncodePolicies: []catalogdoc.RecordingEncodePolicy{{
			RecordingID: 30, KeepOriginal: "always", EncodeProfiles: []string{"h264"}, CMDetect: true,
			CreatedAt: at, UpdatedAt: at,
		}},
		MediaAssets: []catalogdoc.MediaAsset{{
			ID: 40, RecordingID: 30, Kind: "original", RelPath: "sites/default/round-trip.m2ts",
			SizeBytes: 100, State: "active", CreatedAt: at, UpdatedAt: at,
		}, {
			ID: 41, RecordingID: 30, Kind: "encoded", Profile: &profile, RelPath: "sites/default/round-trip.mp4",
			SizeBytes: 80, State: "active", CreatedAt: at, UpdatedAt: at,
		}},
		DropStats:                  []catalogdoc.DropStat{{MediaAssetID: 40, Pid: 256, Packets: 100, Drops: 2, Errors: 1, Scrambled: 0, PidType: &pidType}},
		DropPositions:              []catalogdoc.DropPosition{{MediaAssetID: 40, ByteOffset: 188, Pid: 256, ElapsedMs: &elapsed}},
		MediaAssetTSScans:          []catalogdoc.MediaAssetTSScan{{MediaAssetID: 40, ScannedSizeBytes: 100}},
		RecordingChapterOwnerships: []catalogdoc.RecordingChapterOwnership{{RecordingID: 30, AdoptedAt: at}},
		RecordingChapterSpans:      []catalogdoc.RecordingChapterSpan{{RecordingID: 30, StartMs: 100, EndMs: 200, Label: &spanLabel, Cut: true}},
	}
}
