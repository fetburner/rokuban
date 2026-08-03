package catalog

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
)

// TestRescue_RejectsPreIssue101SnapshotWithoutChannelIdentity は、
// program_snapshots のチャンネル・イベント識別 6 列が NOT NULL 化される
// （issue #101。00026）より前に export された catalog ダンプ（channel
// identity が nil）を rescue しようとするとエラーになることを確認する。
//
// catalog.ProgramSnapshot はポインタのまま残してある（document.go のコメント
// 参照）。00026 以降の DB は NULL を書けないので、この経路を通るのは
// ディスク上に残る古いダンプだけ。推測で 0 / 空文字を書くと誤った識別を
// 永続化するので、applyDocument はこの行に到達した時点でエラーを返し、
// program_intents / program_overrides の FK 違反という分かりにくい失敗に
// させない（rescue.go の判断コメント参照）。
func TestRescue_RejectsPreIssue101SnapshotWithoutChannelIdentity(t *testing.T) {
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
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = applyDocument(ctx, tx, doc)
	if err == nil {
		t.Fatal("expected an error rescuing a pre-#101 snapshot without channel identity, got nil")
	}
	if !strings.Contains(err.Error(), "issue #101") {
		t.Errorf("error should mention issue #101 so operators know why the dump was rejected, got: %v", err)
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
