package api

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	jobqtestutil "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestInsertHelpers_UseCallerTransaction は、ドメイン書き込みと同じ tx を
// EnqueueTx に渡すことを確認する。コミット連動の配送は adapter の契約テストで固定する。
func TestInsertHelpers_UseCallerTransaction(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	enqueuer := jobqtestutil.NewRecordingEnqueuer()

	cases := []struct {
		name   string
		kind   string
		insert func(ctx context.Context, tx pgx.Tx) error
	}{
		{"label_rule_reconcile", jobs.LabelRuleReconcileArgs{}.Kind(), func(ctx context.Context, tx pgx.Tx) error {
			return insertLabelRuleReconcile(ctx, tx, enqueuer)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enqueuer.Reset()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := tc.insert(ctx, tx); err != nil {
				t.Fatalf("insert: %v", err)
			}
			calls := enqueuer.CallsOfKind(tc.kind)
			if len(calls) != 1 {
				t.Fatalf("%s enqueue calls = %d, want 1", tc.kind, len(calls))
			}
			if !calls[0].Transactional || calls[0].Tx != tx {
				t.Errorf("%s enqueue call = %#v, want EnqueueTx with caller transaction", tc.kind, calls[0])
			}
		})
	}
}
