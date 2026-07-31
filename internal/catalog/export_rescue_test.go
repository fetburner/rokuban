package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// Export → Write → 空 DB への Rescue で recordings / media_assets / drop_stats /
// rules が戻ること。再実行しても増殖しないこと。
func TestExportRescue_RoundTrip(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	// --- seed ---
	rule, err := q.CreateRule(ctx, sqlcgen.CreateRuleParams{
		Name:             "news",
		Description:      "seed",
		Enabled:          true,
		Priority:         10,
		KeepOriginal:     "always",
		EncodeProfiles:   []string{},
		FilenameTemplate: "",
		Metadata:         json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	if err := q.InsertRuleTextMatch(ctx, sqlcgen.InsertRuleTextMatchParams{
		RuleID: rule.ID, Seq: 0, Target: "name", Mode: "keyword",
		Value: "ニュース", CaseSensitive: false, Negate: false,
	}); err != nil {
		t.Fatalf("InsertRuleTextMatch: %v", err)
	}
	if err := q.InsertRuleChannelType(ctx, sqlcgen.InsertRuleChannelTypeParams{
		RuleID: rule.ID, ChannelType: "GR",
	}); err != nil {
		t.Fatalf("InsertRuleChannelType: %v", err)
	}

	recID, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
		RuleID:            &rule.ID,
		Source:            "rule",
		Site:              "default",
		NetworkID:         32736,
		ServiceID:         1024,
		EventID:           100,
		ServiceName:       "NHK総合",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "ニュース",
		IsFree:            true,
		ProgramStartAt:    time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		ProgramDurationMs: 1800000,
		Status:            "finished",
		StartedAt:         ptrTime(time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)),
		EndedAt:           ptrTime(time.Date(2026, 7, 29, 12, 30, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("CreateRecording: %v", err)
	}

	assetID, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recID,
		Kind:        "original",
		RelPath:     "20260729/120000_ニュース_1024.m2ts",
		SizeBytes:   1_000_000,
	})
	if err != nil {
		t.Fatalf("CreateMediaAsset: %v", err)
	}
	// M3-7 の tombstone / 即時 purge 意図も catalog で保護する。
	purgeAt := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		UPDATE recordings SET deleted_at = $2, purge_after = $2 WHERE id = $1
	`, recID, purgeAt); err != nil {
		t.Fatalf("mark recording purge: %v", err)
	}

	pidType := "video"
	if err := q.InsertDropStat(ctx, sqlcgen.InsertDropStatParams{
		MediaAssetID: assetID,
		Pid:          0x100,
		Packets:      10000,
		Drops:        3,
		Errors:       0,
		Scrambled:    0,
		PidType:      &pidType,
	}); err != nil {
		t.Fatalf("InsertDropStat: %v", err)
	}

	// program_snapshots + intent（FK のため snapshot が先）。
	if _, err := pool.Exec(ctx, `
		INSERT INTO program_snapshots (site, program_id, title, start_at, duration_ms)
		VALUES ('default', 999001, '手動', '2026-07-30T01:00:00Z', 3600000)
	`); err != nil {
		t.Fatalf("insert program_snapshots: %v", err)
	}
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: "default", ProgramID: 999001, Action: "record",
	}); err != nil {
		t.Fatalf("UpsertProgramIntent: %v", err)
	}

	// --- export ---
	doc, err := Export(ctx, pool, "")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(doc.Rules) != 1 {
		t.Fatalf("exported rules = %d, want 1", len(doc.Rules))
	}
	if len(doc.Rules[0].TextMatches) != 1 {
		t.Errorf("exported text matches = %d, want 1", len(doc.Rules[0].TextMatches))
	}
	if len(doc.Recordings) != 1 || doc.Recordings[0].ID != recID {
		t.Fatalf("exported recordings = %+v, want id=%d", doc.Recordings, recID)
	}
	if doc.Recordings[0].DeletedAt == nil || doc.Recordings[0].PurgeAfter == nil ||
		!doc.Recordings[0].PurgeAfter.Equal(purgeAt) {
		t.Fatalf("exported tombstone deletedAt=%v purgeAfter=%v", doc.Recordings[0].DeletedAt, doc.Recordings[0].PurgeAfter)
	}
	if len(doc.MediaAssets) != 1 || doc.MediaAssets[0].ID != assetID {
		t.Fatalf("exported media_assets = %+v", doc.MediaAssets)
	}
	if len(doc.DropStats) != 1 || doc.DropStats[0].Drops != 3 {
		t.Fatalf("exported drop_stats = %+v", doc.DropStats)
	}
	if len(doc.ProgramIntents) != 1 {
		t.Fatalf("exported program_intents = %d, want 1", len(doc.ProgramIntents))
	}

	mediaDir := t.TempDir()
	path, err := Write(mediaDir, doc, 7)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// --- wipe durable tables ---
	if _, err := pool.Exec(ctx, `
		TRUNCATE drop_stats, media_assets, recordings, program_intents,
		         program_overrides, program_snapshots, rule_text_matches,
		         rule_services, rule_channel_types, rule_genres, rule_times,
		         rule_sites, rules RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// --- rescue ---
	result, err := RescueFile(ctx, pool, path)
	if err != nil {
		t.Fatalf("RescueFile: %v", err)
	}
	if result.Rules != 1 || result.Recordings != 1 || result.MediaAssets != 1 || result.DropStats != 1 {
		t.Fatalf("rescue counts: rules=%d rec=%d assets=%d drops=%d",
			result.Rules, result.Recordings, result.MediaAssets, result.DropStats)
	}
	if result.ProgramIntents != 1 || result.ProgramSnapshots != 1 {
		t.Fatalf("rescue intents=%d snaps=%d", result.ProgramIntents, result.ProgramSnapshots)
	}

	// IDs preserved.
	var gotTitle string
	var gotRecID int64
	if err := pool.QueryRow(ctx, `SELECT id, title FROM recordings`).Scan(&gotRecID, &gotTitle); err != nil {
		t.Fatalf("query recordings: %v", err)
	}
	if gotRecID != recID || gotTitle != "ニュース" {
		t.Errorf("recording id/title = %d/%q, want %d/ニュース", gotRecID, gotTitle, recID)
	}
	var gotDeletedAt, gotPurgeAfter *time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at, purge_after FROM recordings WHERE id = $1`, recID).
		Scan(&gotDeletedAt, &gotPurgeAfter); err != nil {
		t.Fatalf("query recording tombstone: %v", err)
	}
	if gotDeletedAt == nil || gotPurgeAfter == nil || !gotPurgeAfter.Equal(purgeAt) {
		t.Errorf("rescued tombstone deletedAt=%v purgeAfter=%v, want purgeAt=%v", gotDeletedAt, gotPurgeAfter, purgeAt)
	}

	var gotAssetID, gotSize int64
	if err := pool.QueryRow(ctx, `SELECT id, size_bytes FROM media_assets`).Scan(&gotAssetID, &gotSize); err != nil {
		t.Fatalf("query media_assets: %v", err)
	}
	if gotAssetID != assetID || gotSize != 1_000_000 {
		t.Errorf("media_asset id/size = %d/%d, want %d/1000000", gotAssetID, gotSize, assetID)
	}

	var drops int64
	if err := pool.QueryRow(ctx, `SELECT drops FROM drop_stats WHERE media_asset_id = $1`, assetID).Scan(&drops); err != nil {
		t.Fatalf("query drop_stats: %v", err)
	}
	if drops != 3 {
		t.Errorf("drops = %d, want 3", drops)
	}

	var matchValue string
	if err := pool.QueryRow(ctx,
		`SELECT value FROM rule_text_matches WHERE rule_id = $1`, rule.ID,
	).Scan(&matchValue); err != nil {
		t.Fatalf("query rule_text_matches: %v", err)
	}
	if matchValue != "ニュース" {
		t.Errorf("text match = %q, want ニュース", matchValue)
	}

	// --- idempotent second pass ---
	result2, err := RescueFile(ctx, pool, path)
	if err != nil {
		t.Fatalf("RescueFile second: %v", err)
	}
	if result2.Recordings != 1 {
		t.Errorf("second rescue recordings = %d, want 1", result2.Recordings)
	}
	var recCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recordings`).Scan(&recCount); err != nil {
		t.Fatal(err)
	}
	if recCount != 1 {
		t.Errorf("recordings after second rescue = %d, want 1 (増殖しない)", recCount)
	}
	var assetCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_assets`).Scan(&assetCount); err != nil {
		t.Fatal(err)
	}
	if assetCount != 1 {
		t.Errorf("media_assets after second rescue = %d, want 1", assetCount)
	}
}

// site 絞り込みが site 列を持つ表に効くこと。
func TestExport_SiteFilter(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	for _, site := range []string{"default", "other"} {
		if _, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
			Source: "manual", Site: site,
			NetworkID: 1, ServiceID: 1, EventID: int32(len(site)),
			ServiceName: "s", ChannelType: "GR", Channel: "1",
			Title: site, IsFree: true,
			ProgramStartAt:    time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC),
			ProgramDurationMs: 60000, Status: "finished",
		}); err != nil {
			t.Fatalf("CreateRecording site=%s: %v", site, err)
		}
	}

	doc, err := Export(ctx, pool, "default")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(doc.Recordings) != 1 || doc.Recordings[0].Site != "default" {
		t.Fatalf("filtered recordings = %+v, want 1 default", doc.Recordings)
	}
	if doc.Site == nil || *doc.Site != "default" {
		t.Errorf("doc.Site = %v, want default", doc.Site)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestExport_ConcurrentIngestStaysConsistent は、Export の実行中に他のトランザク
// ションが録画を作り続けても、返る Document が自己無矛盾（すべての media_asset の
// recording_id が同じ Document の recordings に存在する）であることを確認する
// 回帰テスト（issue #106）。
//
// Export が単一トランザクション（REPEATABLE READ, READ ONLY）から読まないと、
// recordings を読んだ「後」に作られた録画の media_assets だけを後続クエリが拾って
// しまい、Document 内で「recordings に居ない録画を指す media_asset」が発生しうる。
// RescueFile（internal/catalog/rescue.go）はこの Document を rules → recordings →
// media_assets の順に 1 トランザクションで書くので、そのケースは
// media_assets.recording_id の FK 制約違反でまるごと復元不能になる。
//
// 手動確認（issue #106 の受け入れ基準）: internal/catalog/export.go の Export から
// トランザクションを外し、pool.BeginTx を通さず sqlcgen.New(pool) を直接使う旧実装
// に戻してこのテストを実行すると、このテストは（毎回ではないがタイミング次第で）
// 失敗し、実際に log から
// "media_asset ... references recording ... not present in exported recordings"
// が観測できることを確認済み。トランザクションを戻すと決定的に通る。
func TestExport_ConcurrentIngestStaysConsistent(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			recID, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
				Source: "manual", Site: "default",
				NetworkID: 1, ServiceID: 1, EventID: int32(10_000 + i),
				ServiceName: "s", ChannelType: "GR", Channel: "1",
				Title: fmt.Sprintf("race-%d", i), IsFree: true,
				ProgramStartAt:    time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC),
				ProgramDurationMs: 60000, Status: "finished",
			})
			if err != nil {
				// テスト終盤で pool が閉じられたときなど。ゴルーチンは黙って終わる。
				return
			}
			if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
				RecordingID: recID,
				Kind:        "original",
				RelPath:     fmt.Sprintf("race/%d.m2ts", i),
				SizeBytes:   1,
			}); err != nil {
				return
			}
		}
	}()

	var lastDoc *Document
	const iterations = 100
	for i := 0; i < iterations; i++ {
		doc, err := Export(ctx, pool, "")
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("Export: %v", err)
		}
		recIDs := make(map[int64]bool, len(doc.Recordings))
		for _, r := range doc.Recordings {
			recIDs[r.ID] = true
		}
		for _, a := range doc.MediaAssets {
			if !recIDs[a.RecordingID] {
				close(stop)
				wg.Wait()
				t.Fatalf("media_asset %d references recording %d not present in exported recordings (snapshot not consistent)", a.ID, a.RecordingID)
			}
		}
		lastDoc = doc
	}
	close(stop)
	wg.Wait()

	// フルパイプライン: 最後に取れた Document を rescue しても FK 違反が出ないこと。
	mediaDir := t.TempDir()
	path, err := Write(mediaDir, lastDoc, 7)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := RescueFile(ctx, pool, path); err != nil {
		t.Fatalf("RescueFile after concurrent export: %v", err)
	}
}
