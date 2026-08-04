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
