package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
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

	// recording_encode_policy 衛星表（issue #159）。凍結済み（ingest が INSERT
	// した状態を模す。resolveAndSnapshotEncodePolicy 相当）。
	if _, err := pool.Exec(ctx, `
		INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles)
		VALUES ($1, 'until_encoded', ARRAY['h265'])
	`, recID); err != nil {
		t.Fatalf("seeding recording_encode_policy: %v", err)
	}

	// 未凍結の録画（原本 media_asset を持たない）。行が無いことがそのまま
	// 往復すること（不変条件 10「意味を持たない行を作らない」）を確認する対照群。
	unfrozenRecID, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
		Source: "manual", Site: "default",
		NetworkID: 32736, ServiceID: 1024, EventID: 101,
		ServiceName: "NHK総合", ChannelType: "GR", Channel: "27",
		Title: "未凍結", IsFree: true,
		ProgramStartAt:    time.Date(2026, 7, 29, 13, 0, 0, 0, time.UTC),
		ProgramDurationMs: 1800000, Status: "recording",
	})
	if err != nil {
		t.Fatalf("CreateRecording (unfrozen): %v", err)
	}
	// M3-7 の tombstone / 即時 purge 意図（issue #319 で boolean 化）も
	// catalog で保護する。
	deletedAt := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		UPDATE recordings SET deleted_at = $2, purge_requested = true WHERE id = $1
	`, recID, deletedAt); err != nil {
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
		INSERT INTO program_snapshots (
			site, program_id, title, start_at, duration_ms,
			network_id, service_id, channel_type, channel, event_id, service_name
		)
		VALUES ('default', 999001, '手動', '2026-07-30T01:00:00Z', 3600000, 32736, 1025, 'GR', '28', 200, 'NHK総合')
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
	if len(doc.Recordings) != 2 {
		t.Fatalf("exported recordings = %+v, want 2 (frozen + unfrozen)", doc.Recordings)
	}
	var frozen *Recording
	for i := range doc.Recordings {
		if doc.Recordings[i].ID == recID {
			frozen = &doc.Recordings[i]
		}
	}
	if frozen == nil {
		t.Fatalf("exported recordings = %+v, want id=%d present", doc.Recordings, recID)
	}
	if frozen.DeletedAt == nil || !frozen.DeletedAt.Equal(deletedAt) || !frozen.PurgeRequested {
		t.Fatalf("exported tombstone deletedAt=%v purgeRequested=%v", frozen.DeletedAt, frozen.PurgeRequested)
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
	// recording_encode_policy（issue #159）: 凍結済みの recID だけが載り、
	// 未凍結の unfrozenRecID は載らない（行の有無そのものが意味を持つ。
	// 不変条件 10）。
	if len(doc.RecordingEncodePolicies) != 1 {
		t.Fatalf("exported recording_encode_policies = %+v, want exactly 1 (unfrozen recording must not appear)",
			doc.RecordingEncodePolicies)
	}
	p := doc.RecordingEncodePolicies[0]
	if p.RecordingID != recID || p.KeepOriginal != "until_encoded" ||
		len(p.EncodeProfiles) != 1 || p.EncodeProfiles[0] != "h265" {
		t.Fatalf("exported recording_encode_policy = %+v, want recordingId=%d keepOriginal=until_encoded profiles=[h265]",
			p, recID)
	}

	mediaDir := t.TempDir()
	if _, err := Write(mediaDir, doc, 7); err != nil {
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
	// 実際の入口（RescueLatest）を通す: 世代の完成判定 → 選択 → 復元まで。
	result, err := RescueLatest(ctx, pool, mediaDir, "default")
	if err != nil {
		t.Fatalf("RescueLatest: %v", err)
	}
	if result.Generation == "" || result.LegacyCatalog {
		t.Fatalf("rescued from %+v, want a verified generation", result)
	}
	if result.Rules != 1 || result.Recordings != 2 || result.MediaAssets != 1 || result.DropStats != 1 {
		t.Fatalf("rescue counts: rules=%d rec=%d assets=%d drops=%d",
			result.Rules, result.Recordings, result.MediaAssets, result.DropStats)
	}
	if result.RecordingEncodePolicies != 1 {
		t.Fatalf("rescue recording_encode_policies = %d, want 1 (unfrozen recording must not gain a row)",
			result.RecordingEncodePolicies)
	}
	if result.ProgramIntents != 1 || result.ProgramSnapshots != 1 {
		t.Fatalf("rescue intents=%d snaps=%d", result.ProgramIntents, result.ProgramSnapshots)
	}

	// IDs preserved.
	var gotTitle string
	var gotRecID int64
	if err := pool.QueryRow(ctx, `SELECT id, title FROM recordings WHERE id = $1`, recID).
		Scan(&gotRecID, &gotTitle); err != nil {
		t.Fatalf("query recordings: %v", err)
	}
	if gotRecID != recID || gotTitle != "ニュース" {
		t.Errorf("recording id/title = %d/%q, want %d/ニュース", gotRecID, gotTitle, recID)
	}

	// recording_encode_policy が値そのまま復元され、未凍結の録画には行が
	// 作られていないこと。
	var gotKeepOriginal string
	var gotProfiles []string
	if err := pool.QueryRow(ctx,
		`SELECT keep_original, encode_profiles FROM recording_encode_policy WHERE recording_id = $1`, recID,
	).Scan(&gotKeepOriginal, &gotProfiles); err != nil {
		t.Fatalf("query recording_encode_policy: %v", err)
	}
	if gotKeepOriginal != "until_encoded" || len(gotProfiles) != 1 || gotProfiles[0] != "h265" {
		t.Errorf("rescued recording_encode_policy = %q/%v, want until_encoded/[h265]", gotKeepOriginal, gotProfiles)
	}
	var unfrozenPolicyCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recording_encode_policy WHERE recording_id = $1`, unfrozenRecID,
	).Scan(&unfrozenPolicyCount); err != nil {
		t.Fatalf("query recording_encode_policy for unfrozen: %v", err)
	}
	if unfrozenPolicyCount != 0 {
		t.Errorf("unfrozen recording gained a recording_encode_policy row (count=%d), want 0", unfrozenPolicyCount)
	}
	var gotDeletedAt *time.Time
	var gotPurgeRequested bool
	if err := pool.QueryRow(ctx, `SELECT deleted_at, purge_requested FROM recordings WHERE id = $1`, recID).
		Scan(&gotDeletedAt, &gotPurgeRequested); err != nil {
		t.Fatalf("query recording tombstone: %v", err)
	}
	if gotDeletedAt == nil || !gotDeletedAt.Equal(deletedAt) || !gotPurgeRequested {
		t.Errorf("rescued tombstone deletedAt=%v purgeRequested=%v, want deletedAt=%v/true", gotDeletedAt, gotPurgeRequested, deletedAt)
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
	result2, err := RescueLatest(ctx, pool, mediaDir, "default")
	if err != nil {
		t.Fatalf("RescueLatest second: %v", err)
	}
	if result2.Recordings != 2 {
		t.Errorf("second rescue recordings = %d, want 2", result2.Recordings)
	}
	if result2.RecordingEncodePolicies != 1 {
		t.Errorf("second rescue recording_encode_policies = %d, want 1 (増殖しない)", result2.RecordingEncodePolicies)
	}
	var recCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recordings`).Scan(&recCount); err != nil {
		t.Fatal(err)
	}
	if recCount != 2 {
		t.Errorf("recordings after second rescue = %d, want 2 (増殖しない)", recCount)
	}
	var policyCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_encode_policy`).Scan(&policyCount); err != nil {
		t.Fatal(err)
	}
	if policyCount != 1 {
		t.Errorf("recording_encode_policy after second rescue = %d, want 1 (増殖しない)", policyCount)
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
// に戻してこのテストを `-count=5` で実行すると 5/5 決定的に失敗し、実際に
// "media_asset ... references recording ... not present in exported recordings"
// が観測できることを確認済み。トランザクションを戻すと `-count=10` で決定的に通る。
func TestExport_ConcurrentIngestStaysConsistent(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	stop := make(chan struct{})
	var inserted atomic.Int64
	// 挿入が失敗した理由を捨てない。飲み込むと「挿入数が足りない」ときに
	// 「CI で飢餓しただけ」と「制約違反で i=0 から失敗している」を区別できない
	// （下の自己チェックが本来検出したい後者が、前者に埋もれる）。
	var insertErr atomic.Pointer[error]
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
				// テスト終盤で pool が閉じられたときなど。理由は記録して終わる。
				insertErr.CompareAndSwap(nil, &err)
				return
			}
			if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
				RecordingID: recID,
				Kind:        "original",
				RelPath:     fmt.Sprintf("race/%d.m2ts", i),
				SizeBytes:   1,
			}); err != nil {
				insertErr.CompareAndSwap(nil, &err)
				return
			}
			inserted.Add(1)
		}
	}()

	var lastDoc *Document
	const iterations = 100
	// 競合ゴルーチンが最低 minInserts 件を入れるまでは Export を回し続ける。
	// 固定回数だけ回して後から挿入数を検証する形だと、コア数の少ない CI では
	// ゴルーチンが飢餓して「レース窓を踏めなかった」で落ちる（実際に落ちた）。
	// 一貫性の検証は毎イテレーション行われるので、回す回数を伸ばしても
	// テストの意味は変わらない。deadline は「挿入が本当に失敗し続けている」
	// ケースを無限ループにしないための上限。
	const minInserts = 10
	deadline := time.Now().Add(60 * time.Second)
	for i := 0; i < iterations || inserted.Load() < minInserts; i++ {
		if time.Now().After(deadline) {
			break
		}
		// ゴルーチンが挿入エラーで死んでいるなら待っても増えない。すぐ抜けて
		// 下の自己チェックにその理由を報告させる（制約違反で挿入が失敗し続ける
		// ケースを 60 秒待たずに落とす）。
		if i >= iterations && insertErr.Load() != nil {
			break
		}
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

	// 競合ゴルーチンが実際に挿入できていなければ、この回帰は何も検証せず
	// 「無言で成功し続ける」空虚なテストになる（将来 NOT NULL / CHECK が増えて
	// i=0 から挿入が失敗し続けるケースを想定）。挿入数を検証してそれを防ぐ。
	if n := inserted.Load(); n < minInserts {
		reason := "理由不明（競合ゴルーチンはエラーを報告していない）"
		if p := insertErr.Load(); p != nil {
			reason = (*p).Error()
		}
		t.Fatalf("concurrent goroutine only inserted %d recordings, want >= %d "+
			"(race window was not exercised): %s", n, minInserts, reason)
	}

	// フルパイプライン: 最後に取れた Document を rescue しても FK 違反が出ないこと。
	mediaDir := t.TempDir()
	if _, err := Write(mediaDir, lastDoc, 7); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := RescueLatest(ctx, pool, mediaDir, "default"); err != nil {
		t.Fatalf("RescueLatest after concurrent export: %v", err)
	}
}
