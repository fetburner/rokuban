package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
	"github.com/riverqueue/river/rivertype"
)

const riverTestPageSize = 10_000

type riverKindArgs struct{ kind string }

func (args riverKindArgs) Kind() string { return args.kind }

// NewRiverClient はワーカーを起動せず River の公開クライアント API を使う
// テスト用クライアントを作る。
func NewRiverClient(tb testing.TB, pool *pgxpool.Pool) *river.Client[pgx.Tx] {
	tb.Helper()
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		tb.Fatalf("creating test River client: %v", err)
	}
	return client
}

// RequireRiverInserted は rivertest の公開 assertion で kind ごとにちょうど 1 件の
// 投入を確認する。
func RequireRiverInserted[TArgs river.JobArgs](ctx context.Context, tb testing.TB, pool *pgxpool.Pool, args TArgs, opts *rivertest.RequireInsertedOpts) *river.Job[TArgs] {
	tb.Helper()
	return rivertest.RequireInserted[*riverpgxv5.Driver](ctx, tb, riverpgxv5.New(pool), args, opts)
}

// RequireRiverInsertedTx は rivertest の公開 assertion でトランザクション内の投入を確認する。
func RequireRiverInsertedTx[TArgs river.JobArgs](ctx context.Context, tb testing.TB, tx pgx.Tx, args TArgs, opts *rivertest.RequireInsertedOpts) *river.Job[TArgs] {
	tb.Helper()
	return rivertest.RequireInsertedTx[*riverpgxv5.Driver](ctx, tb, tx, args, opts)
}

// RequireManyRiverInserted は rivertest の公開 assertion で複数ジョブの投入数と順序を確認する。
func RequireManyRiverInserted(tb testing.TB, ctx context.Context, pool *pgxpool.Pool, expected []rivertest.ExpectedJob) []*rivertype.JobRow {
	tb.Helper()
	return rivertest.RequireManyInserted[*riverpgxv5.Driver](ctx, tb, riverpgxv5.New(pool), expected)
}

// RequireManyRiverInsertedTx は rivertest の公開 assertion でトランザクション内の複数投入を確認する。
func RequireManyRiverInsertedTx(tb testing.TB, ctx context.Context, tx pgx.Tx, expected []rivertest.ExpectedJob) []*rivertype.JobRow {
	tb.Helper()
	return rivertest.RequireManyInsertedTx[*riverpgxv5.Driver](ctx, tb, tx, expected)
}

// RequireRiverKindInserted は rivertest の assertion で kind ごとにちょうど 1 件の投入を確認する。
func RequireRiverKindInserted(tb testing.TB, ctx context.Context, pool *pgxpool.Pool, kind string) *rivertype.JobRow {
	tb.Helper()
	return RequireRiverInserted(ctx, tb, pool, riverKindArgs{kind: kind}, nil).JobRow
}

// RequireRiverKindInsertedTx は rivertest の assertion でトランザクション内の kind ごとの投入を確認する。
func RequireRiverKindInsertedTx(tb testing.TB, ctx context.Context, tx pgx.Tx, kind string) *rivertype.JobRow {
	tb.Helper()
	return RequireRiverInsertedTx(ctx, tb, tx, riverKindArgs{kind: kind}, nil).JobRow
}

// RequireManyRiverKindsInserted は rivertest の assertion で kind の件数と投入順を確認する。
func RequireManyRiverKindsInserted(tb testing.TB, ctx context.Context, pool *pgxpool.Pool, kinds ...string) []*rivertype.JobRow {
	tb.Helper()
	expected := make([]rivertest.ExpectedJob, len(kinds))
	for i, kind := range kinds {
		expected[i] = rivertest.ExpectedJob{Args: riverKindArgs{kind: kind}}
	}
	return RequireManyRiverInserted(tb, ctx, pool, expected)
}

// RequireManyRiverKindsInsertedTx は rivertest の assertion でトランザクション内の複数 kind を確認する。
func RequireManyRiverKindsInsertedTx(tb testing.TB, ctx context.Context, tx pgx.Tx, kinds ...string) []*rivertype.JobRow {
	tb.Helper()
	expected := make([]rivertest.ExpectedJob, len(kinds))
	for i, kind := range kinds {
		expected[i] = rivertest.ExpectedJob{Args: riverKindArgs{kind: kind}}
	}
	return RequireManyRiverInsertedTx(tb, ctx, tx, expected)
}

// ListRiverJobs は River の公開 JobList API で条件に合うジョブを全件取得する。
func ListRiverJobs(ctx context.Context, client *river.Client[pgx.Tx], params *river.JobListParams) ([]*rivertype.JobRow, error) {
	if params == nil {
		params = river.NewJobListParams()
	}
	params = params.First(riverTestPageSize)

	var jobs []*rivertype.JobRow
	for {
		result, err := client.JobList(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("listing River jobs: %w", err)
		}
		jobs = append(jobs, result.Jobs...)
		if len(result.Jobs) < riverTestPageSize {
			return jobs, nil
		}
		if result.LastCursor == nil {
			return nil, fmt.Errorf("listing River jobs: full page has no cursor")
		}
		params = params.After(result.LastCursor)
	}
}

// MustListRiverJobs は ListRiverJobs のエラーをテスト失敗として扱う。
func MustListRiverJobs(tb testing.TB, ctx context.Context, client *river.Client[pgx.Tx], params *river.JobListParams) []*rivertype.JobRow {
	tb.Helper()
	jobs, err := ListRiverJobs(ctx, client, params)
	if err != nil {
		tb.Fatalf("listing River jobs: %v", err)
	}
	return jobs
}

// MustListRiverJobsOfKind は指定 kind のジョブを公開 JobList API で全件取得する。
func MustListRiverJobsOfKind(tb testing.TB, ctx context.Context, pool *pgxpool.Pool, kind string) []*rivertype.JobRow {
	tb.Helper()
	client := NewRiverClient(tb, pool)
	return MustListRiverJobs(tb, ctx, client, river.NewJobListParams().Kinds(kind))
}

// MustSingleRiverJobOfKind は指定 kind のジョブがちょうど 1 件あることを公開 API で確認する。
func MustSingleRiverJobOfKind(tb testing.TB, ctx context.Context, pool *pgxpool.Pool, kind string) *rivertype.JobRow {
	tb.Helper()
	rows := MustListRiverJobsOfKind(tb, ctx, pool, kind)
	if len(rows) != 1 {
		tb.Fatalf("River job count for %s = %d, want 1", kind, len(rows))
	}
	return rows[0]
}

// MustGetRiverJob は River の公開 JobGet API でジョブを取得し、エラーをテスト失敗にする。
func MustGetRiverJob(tb testing.TB, ctx context.Context, client *river.Client[pgx.Tx], id int64) *rivertype.JobRow {
	tb.Helper()
	job, err := client.JobGet(ctx, id)
	if err != nil {
		tb.Fatalf("getting River job %d: %v", id, err)
	}
	return job
}

// MustDecodeRiverJobArgs は公開 JobList / JobGet が返した引数を指定型へ復号する。
func MustDecodeRiverJobArgs[T any](tb testing.TB, row *rivertype.JobRow) T {
	tb.Helper()
	var args T
	if err := json.Unmarshal(row.EncodedArgs, &args); err != nil {
		tb.Fatalf("decoding River job %d args: %v", row.ID, err)
	}
	return args
}

// DeleteRiverJobs は River の公開 JobDeleteMany API で条件に合う非実行中
// ジョブを削除する。
func DeleteRiverJobs(ctx context.Context, client *river.Client[pgx.Tx], params *river.JobDeleteManyParams) ([]*rivertype.JobRow, error) {
	if params == nil {
		return nil, fmt.Errorf("deleting River jobs: params are required")
	}
	result, err := client.JobDeleteMany(ctx, params.First(riverTestPageSize))
	if err != nil {
		return nil, fmt.Errorf("deleting River jobs: %w", err)
	}
	return result.Jobs, nil
}

// MustDeleteRiverJobs は DeleteRiverJobs のエラーをテスト失敗として扱う。
func MustDeleteRiverJobs(tb testing.TB, ctx context.Context, client *river.Client[pgx.Tx], params *river.JobDeleteManyParams) []*rivertype.JobRow {
	tb.Helper()
	jobs, err := DeleteRiverJobs(ctx, client, params)
	if err != nil {
		tb.Fatalf("deleting River jobs: %v", err)
	}
	return jobs
}

// MustDeleteRiverJobsOfKind は指定 kind の非実行中ジョブを公開 JobDeleteMany API で削除する。
func MustDeleteRiverJobsOfKind(tb testing.TB, ctx context.Context, pool *pgxpool.Pool, kind string) []*rivertype.JobRow {
	tb.Helper()
	client := NewRiverClient(tb, pool)
	return MustDeleteRiverJobs(tb, ctx, client, river.NewJobDeleteManyParams().Kinds(kind))
}
