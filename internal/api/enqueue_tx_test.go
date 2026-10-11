package api

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	riveradaptertest "github.com/fetburner/rokuban/internal/jobq/riveradapter/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestInsertHelpers_FollowTransaction は、ドメイン書き込みと同一トランザクションで
// 投入するヘルパが tx の外へ投入していないことを確かめる。rollback で 0 件、commit
// 前に 0 件、commit 後に 1 件でなければ dual-write になる。
func TestInsertHelpers_FollowTransaction(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	riverClient, err := riveradapter.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	enqueuer := riveradapter.NewLegacyEnqueuer(riverClient)
	recordingID := seedRecording(t, pool, "cm", time.Now(), "finished", 1)

	cases := []struct {
		name   string
		kind   string
		insert func(ctx context.Context, tx pgx.Tx) error
	}{
		{"label_rule_reconcile", jobs.LabelRuleReconcileArgs{}.Kind(), func(ctx context.Context, tx pgx.Tx) error {
			return insertLabelRuleReconcile(ctx, tx, enqueuer)
		}},
		{"cm_detect_reconcile", jobs.CMDetectReconcileArgs{}.Kind(), func(ctx context.Context, tx pgx.Tx) error {
			return insertCMDetectReconcile(ctx, tx, enqueuer)
		}},
		{"cm_logo_candidate", jobs.CMLogoCandidateJobArgs{}.Kind(), func(ctx context.Context, tx pgx.Tx) error {
			return insertCMLogoCandidate(ctx, tx, enqueuer, jobs.CMLogoCandidateJobArgs{RecordingID: recordingID})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count := func() int {
				return len(riveradaptertest.MustListRiverJobsOfKind(t, ctx, pool, tc.kind))
			}
			run := func(commit bool) {
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatalf("begin: %v", err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				if err := tc.insert(ctx, tx); err != nil {
					t.Fatalf("insert: %v", err)
				}
				if got := count(); got != 0 {
					t.Fatalf("%s jobs before end of tx = %d, want 0", tc.kind, got)
				}
				if !commit {
					if err := tx.Rollback(ctx); err != nil {
						t.Fatalf("rollback: %v", err)
					}
					return
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatalf("commit: %v", err)
				}
			}
			run(false)
			if got := count(); got != 0 {
				t.Fatalf("%s jobs after rollback = %d, want 0", tc.kind, got)
			}
			run(true)
			if got := count(); got != 1 {
				t.Fatalf("%s jobs after commit = %d, want 1", tc.kind, got)
			}
		})
	}
}
