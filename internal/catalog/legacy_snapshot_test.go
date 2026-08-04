package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
)

// TestRescue_SkipsPreIssue101SnapshotButRestoresDurableAssets は、
// program_snapshots のチャンネル・イベント識別 6 列が NOT NULL 化される
// （issue #101。00026）より前に export された catalog ダンプ（channel identity が
// nil）を rescue したとき、**その行だけスキップして永続資産の復旧は続行する**
// ことを確認する。
//
// catalog.ProgramSnapshot はポインタのまま残してある（document.go のコメント参照）。
// 00026 以降の DB は NULL を書けないので、この経路を通るのはディスク上に残る古い
// ダンプだけ。推測で 0 / 空文字を書くと誤った識別を永続化するのでスキップするが、
// **エラーで rescue 全体を止めてはいけない** --- rescue は 1 トランザクションで
// recordings / media_assets / rules（永続資産）も復元する。program_snapshots は
// 放送 + epg.retention_grace（既定 24h）で GC される導出テーブルなので、その 1 行の
// ために災害復旧そのものを止めるのは釣り合わない。
//
// FK 先を失う program_intents / program_overrides は連動してスキップし、件数を
// RescueResult に数える（黙って切り捨てない）。
func TestRescue_SkipsPreIssue101SnapshotButRestoresDurableAssets(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	doc := &Document{
		Version:    Version,
		ExportedAt: time.Now().UTC(),
		ProgramSnapshots: []ProgramSnapshot{
			{
				Site:       "default",
				ProgramID:  123456,
				Title:      "移行前ダンプ",
				StartAt:    time.Now().Add(time.Hour),
				DurationMs: 1800000,
				// NetworkID 等は意図的に設定しない
				// （00026 より前に作られた nil ダンプを模す）。
				UpdatedAt: time.Now(),
			},
		},
		// FK 先を失うので一緒にスキップされるはず。
		ProgramIntents: []ProgramIntent{
			{Site: "default", ProgramID: 123456, Action: "record",
				CreatedAt: time.Now(), UpdatedAt: time.Now()},
		},
		// 永続資産。これは必ず復元されなければならない。
		Recordings: []Recording{
			{
				ID: 4242, Source: "manual", Site: "default",
				NetworkID: 32736, ServiceID: 1024, EventID: 777,
				ServiceName: "NHK総合", ChannelType: "GR", Channel: "27",
				Title: "災害復旧で守るべき録画", IsFree: true,
				ProgramStartAt: time.Now().Add(-2 * time.Hour), ProgramDurationMs: 1800000,
				Status:    "finished",
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			},
		},
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	res, err := applyDocument(ctx, tx, doc)
	if err != nil {
		t.Fatalf("applyDocument: %v （識別子の無い 1 行で災害復旧を止めてはいけない）", err)
	}
	if res.SkippedProgramSnapshots != 1 {
		t.Errorf("SkippedProgramSnapshots = %d, want 1", res.SkippedProgramSnapshots)
	}
	if res.ProgramSnapshots != 0 {
		t.Errorf("ProgramSnapshots = %d, want 0（識別子が無いので復元しない）", res.ProgramSnapshots)
	}
	if res.SkippedProgramIntents != 1 {
		t.Errorf("SkippedProgramIntents = %d, want 1（FK 先を失うので連動して落ちる）", res.SkippedProgramIntents)
	}
	// 本命: 永続資産が復元されている。
	if res.Recordings != 1 {
		t.Fatalf("Recordings = %d, want 1（永続資産の復旧が止まっている）", res.Recordings)
	}
	var title string
	if err := tx.QueryRow(ctx, `SELECT title FROM recordings WHERE id = 4242`).Scan(&title); err != nil {
		t.Fatalf("querying restored recording: %v", err)
	}
	if title != "災害復旧で守るべき録画" {
		t.Errorf("restored title = %q", title)
	}
}

// TestRescue_AcceptsSnapshotWithChannelIdentity は上のテストの反対方向:
// channel identity が揃っている（00026 以降に export された、通常の）
// ProgramSnapshot は rescue できることを確認する。
func TestRescue_AcceptsSnapshotWithChannelIdentity(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	networkID, serviceID, eventID := int32(32736), int32(1024), int32(1)
	channelType, channel, serviceName := "GR", "27", "テスト局"
	doc := &Document{
		Version:    Version,
		ExportedAt: time.Now().UTC(),
		ProgramSnapshots: []ProgramSnapshot{
			{
				Site:        "default",
				ProgramID:   654321,
				Title:       "通常のダンプ",
				StartAt:     time.Now().Add(time.Hour),
				DurationMs:  1800000,
				NetworkID:   &networkID,
				ServiceID:   &serviceID,
				ChannelType: &channelType,
				Channel:     &channel,
				EventID:     &eventID,
				ServiceName: &serviceName,
				UpdatedAt:   time.Now(),
			},
		},
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := applyDocument(ctx, tx, doc)
	if err != nil {
		t.Fatalf("applyDocument with full channel identity should succeed: %v", err)
	}
	if result.ProgramSnapshots != 1 {
		t.Errorf("ProgramSnapshots rescued = %d, want 1", result.ProgramSnapshots)
	}
}

// TestRescue_RestoresLegacyEncodePolicyFromPreIssue159Dump は issue #159 より前に
// export された catalog ダンプ（recordings.keep_original / encode_profiles が
// まだ列だった頃。doc.RecordingEncodePolicies は空で、旧列の値は
// Recording.KeepOriginalLegacy / EncodeProfilesLegacy に残る。document.go
// 参照）を rescue すると、原本 media_asset を持つ（= ingest が完了していた）
// 録画については recording_encode_policy 行が復元されることを確認する。
//
// 対応する MediaAsset（kind="original"）が無い録画（旧列は既定値のまま残る、
// 未凍結だった録画）は行を復元しない --- migration 00030 backfill と同じ基準
// （原本 media_asset の有無。列の値そのものは判定に使わない。CLAUDE.md
// 不変条件 9）を、DB ではなくダンプ自身の doc.MediaAssets に対して適用する。
//
// 直す前の実装（Recording から KeepOriginalLegacy / EncodeProfilesLegacy を
// 削っただけ）はこの往復で凍結済みポリシーを黙って失っていた ---
// 削除エンジンが対象外になり、EnqueueMissingEncodes が no-op になり、事後追加
// API が既定値 'always' で上書きする（issue #159 レビューで指摘）。
func TestRescue_RestoresLegacyEncodePolicyFromPreIssue159Dump(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	frozenKeepOriginal := "until_encoded"
	base := time.Now().Add(-2 * time.Hour)

	doc := &Document{
		Version:    Version,
		ExportedAt: time.Now().UTC(),
		Recordings: []Recording{
			{
				// 原本ありの録画（旧 export では凍結済みだった）。
				ID: 9001, Source: "manual", Site: "default",
				NetworkID: 32736, ServiceID: 1024, EventID: 901,
				ServiceName: "NHK総合", ChannelType: "GR", Channel: "27",
				Title: "旧ダンプ・凍結済み", IsFree: true,
				ProgramStartAt: base, ProgramDurationMs: 1800000,
				Status:               "finished",
				KeepOriginalLegacy:   &frozenKeepOriginal,
				EncodeProfilesLegacy: []string{"h265"},
				CreatedAt:            base, UpdatedAt: base,
			},
			{
				// 原本なしの録画（旧 export では未凍結。旧列は既定値 'always' / []
				// のまま残っていた --- 凍結設計の正しい帰結でありバグではない）。
				ID: 9002, Source: "manual", Site: "default",
				NetworkID: 32736, ServiceID: 1024, EventID: 902,
				ServiceName: "NHK総合", ChannelType: "GR", Channel: "27",
				Title: "旧ダンプ・未凍結", IsFree: true,
				ProgramStartAt: base, ProgramDurationMs: 1800000,
				Status:               "recording",
				KeepOriginalLegacy:   ptrString("always"),
				EncodeProfilesLegacy: nil,
				CreatedAt:            base, UpdatedAt: base,
			},
		},
		MediaAssets: []MediaAsset{
			{
				ID: 9101, RecordingID: 9001, Kind: "original",
				RelPath: "20260101/090000_旧ダンプ・凍結済み_1024.m2ts", SizeBytes: 1_000_000,
				State: "active", CreatedAt: base, UpdatedAt: base,
			},
		},
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := applyDocument(ctx, tx, doc)
	if err != nil {
		t.Fatalf("applyDocument: %v", err)
	}
	if result.Recordings != 2 {
		t.Fatalf("Recordings = %d, want 2", result.Recordings)
	}
	if result.RestoredLegacyEncodePolicies != 1 {
		t.Errorf("RestoredLegacyEncodePolicies = %d, want 1 (only the recording with an original media_asset)", result.RestoredLegacyEncodePolicies)
	}

	var keepOriginal string
	var profiles []string
	err = tx.QueryRow(ctx,
		`SELECT keep_original, encode_profiles FROM recording_encode_policy WHERE recording_id = $1`, 9001,
	).Scan(&keepOriginal, &profiles)
	if err != nil {
		t.Fatalf("querying restored recording_encode_policy for 9001: %v", err)
	}
	if keepOriginal != "until_encoded" || len(profiles) != 1 || profiles[0] != "h265" {
		t.Errorf("recording_encode_policy for 9001 = (%q, %v), want (until_encoded, [h265])", keepOriginal, profiles)
	}

	var count int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM recording_encode_policy WHERE recording_id = $1`, 9002,
	).Scan(&count); err != nil {
		t.Fatalf("counting recording_encode_policy rows for 9002: %v", err)
	}
	if count != 0 {
		t.Errorf("recording_encode_policy rows for 9002 (no original media_asset) = %d, want 0 (未凍結のまま埋めない)", count)
	}
}

func ptrString(s string) *string { return &s }
