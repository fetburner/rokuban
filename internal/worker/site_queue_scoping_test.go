package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestMultiSiteWorker_OnlyDequeuesOwnSiteQueues は issue #185（M4-13）の受け入れ
// 基準そのものを固定する: 2 サイトのレジストリで tokyo に束縛された worker は
// ingest_tokyo / epg_tokyo / reconciler_tokyo / watcher_tokyo を購読し、
// takamatsu 向けのジョブ（ingest_takamatsu 等に乗る）を 1 件も掴まない。
//
// 「掴まないこと」はモックへのリクエスト 0 件だけでなく、**ジョブが available の
// まま残ること**で確認する（issue #185 の受け入れ基準の指定どおり）。
//
// # このテストが検出すべき変異（mutation）のリスト --- 実装前に列挙し、
// 実際に注入してすべて落ちることを確認した（issue #185 のレビュー対応）
//
//  1. siteBoundQueueNames から ingestQueue / epgQueue / reconcilerQueue /
//     recordSweepQueue のいずれかを外す --- その種別だけ、insert 側
//     （physicalQueueName 経由）と subscribe 側の両方が非修飾のキュー名に
//     戻り、tokyo の worker が takamatsu 向けの同種ジョブも掴んでしまう
//     （cross-site leak）。4 種それぞれについて「自分の site のジョブは
//     処理され、他 site のジョブは available のまま」の両方を見ないと、
//     1 種だけ壊れても他の 3 種の canary が「worker は生きている」を保証して
//     しまい見逃す（issue #185 のレビューで実際に record_sweep がこの形で
//     見逃されていた）。
//  2. qualifyQueueName の区切り文字や resolve 規則を変える（例: `_` → `-`、
//     または db.DefaultSite への解決を外す） --- Insert 直後の
//     `res.Job.Queue` のリテラル比較が直接落ちる。
//  3. physicalQueueName が boundSite を無視する（常に site 非依存のように
//     振る舞う、または常に qualifyQueueName を呼ぶ） --- 自分の site の
//     ジョブが期待したキュー名にならず、Insert 直後のリテラル比較か、
//     「自分の site のジョブは処理される」チェックが落ちる。
//  4. buildRiverConfig が cfg.BoundSites を渡さない/無視する --- 自分の
//     site のジョブが二度と処理されなくなり、「自分の site のジョブは
//     処理される」チェックが（誤検出防止の canary として）落ちる。
//
// 各変異を実際に注入して落ちることを確認済み（PR 本文に貼った失敗出力を参照）。
func TestMultiSiteWorker_OnlyDequeuesOwnSiteQueues(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/recording/records", "/api/services", "/api/programs", "/api/recording/schedules":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// このプロセスは tokyo に束縛されている。
	workers := NewWorkers(&Deps{Pool: pool, MirakcClients: singleSiteClients("tokyo", mirakc.NewClient(srv.URL, nil))})
	client, err := NewClient(pool, workers, ClientConfig{BoundSites: []string{"tokyo"}})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	clientCtx, clientCancel := context.WithCancel(ctx)
	defer clientCancel()
	if err := client.Start(clientCtx); err != nil {
		t.Fatalf("starting client: %v", err)
	}
	defer func() {
		clientCancel()
		<-client.Stopped()
	}()

	// 4 つの site 単位キューそれぞれについて、tokyo 向け（自分の site。処理
	// されるはず）と takamatsu 向け（他 site。available のまま残るはず）の
	// 両方を用意する。want*Queue はすべてリテラルで書く（実装の定数と比較すると
	// 何も主張しない。issue #185 のレビュー指摘）。
	type siteBoundCase struct {
		name           string
		tokyoArgs      river.JobArgs
		takamatsuArgs  river.JobArgs
		wantTokyoQueue string
		wantOtherQueue string
	}
	cases := []siteBoundCase{
		{
			name:           "ingest",
			tokyoArgs:      IngestJobArgs{Site: "tokyo", RecordID: "rec-tokyo"},
			takamatsuArgs:  IngestJobArgs{Site: "takamatsu", RecordID: "rec-takamatsu"},
			wantTokyoQueue: "ingest_tokyo",
			wantOtherQueue: "ingest_takamatsu",
		},
		{
			name:           "epg_sync",
			tokyoArgs:      EpgSyncArgs{Site: "tokyo"},
			takamatsuArgs:  EpgSyncArgs{Site: "takamatsu"},
			wantTokyoQueue: "epg_tokyo",
			wantOtherQueue: "epg_takamatsu",
		},
		{
			name:           "reconcile_pass",
			tokyoArgs:      ReconcilePassArgs{Site: "tokyo"},
			takamatsuArgs:  ReconcilePassArgs{Site: "takamatsu"},
			wantTokyoQueue: "reconciler_tokyo",
			wantOtherQueue: "reconciler_takamatsu",
		},
		{
			name:           "record_sweep",
			tokyoArgs:      RecordSweepArgs{Site: "tokyo"},
			takamatsuArgs:  RecordSweepArgs{Site: "takamatsu"},
			wantTokyoQueue: "watcher_tokyo",
			wantOtherQueue: "watcher_takamatsu",
		},
	}

	type inserted struct {
		name    string
		tokyoID int64
		otherID int64
	}
	var results []inserted

	for _, c := range cases {
		tokyoRes, err := client.Insert(ctx, c.tokyoArgs, nil)
		if err != nil {
			t.Fatalf("[%s] inserting tokyo job: %v", c.name, err)
		}
		if tokyoRes.Job.Queue != c.wantTokyoQueue {
			t.Fatalf("[%s] tokyo job queue = %q, want %q", c.name, tokyoRes.Job.Queue, c.wantTokyoQueue)
		}

		otherRes, err := client.Insert(ctx, c.takamatsuArgs, nil)
		if err != nil {
			t.Fatalf("[%s] inserting takamatsu job: %v", c.name, err)
		}
		if otherRes.Job.Queue != c.wantOtherQueue {
			t.Fatalf("[%s] takamatsu job queue = %q, want %q", c.name, otherRes.Job.Queue, c.wantOtherQueue)
		}

		results = append(results, inserted{
			name:    c.name,
			tokyoID: tokyoRes.Job.ID,
			otherID: otherRes.Job.ID,
		})
	}

	// 自分（tokyo）の 4 件すべてが「試行された」（available/running から
	// 動いた）ことを確認する。これが確認できないと、他サイトのジョブが
	// available のまま残っていても「worker が単に死んでいるだけ」の
	// 偽陰性と区別できない（各キュー種別ごとに canary を置く理由）。
	for _, r := range results {
		waitForNotAvailable(t, ctx, pool, r.name, r.tokyoID)
	}

	// 他サイト（takamatsu）の 4 件は、tokyo の worker が十分な時間動いた後でも
	// available のまま残っているはず（1 件も掴んでいない）。
	for _, r := range results {
		var state string
		if err := pool.QueryRow(ctx, "SELECT state FROM river_job WHERE id = $1", r.otherID).Scan(&state); err != nil {
			t.Fatalf("[%s] querying takamatsu job state: %v", r.name, err)
		}
		if state != string(rivertype.JobStateAvailable) {
			t.Errorf("[%s] takamatsu job state = %q, want %q (tokyo worker must never dequeue it)",
				r.name, state, rivertype.JobStateAvailable)
		}
		var attempt int
		if err := pool.QueryRow(ctx, "SELECT attempt FROM river_job WHERE id = $1", r.otherID).Scan(&attempt); err != nil {
			t.Fatalf("[%s] querying takamatsu job attempt: %v", r.name, err)
		}
		if attempt != 0 {
			t.Errorf("[%s] takamatsu job attempt = %d, want 0 (tokyo worker must never touch it)", r.name, attempt)
		}
	}
}

// waitForNotAvailable は指定したジョブが available/running から動く
// （retryable / completed / discarded 等になる）まで待つ。動かなければ
// Fatal でその場で失敗させる --- 「他サイトのジョブが available のまま」だけを
// 見るテストは、自サイトのジョブも同じ理由で available のままという偽陰性を
// 拾えない。
func waitForNotAvailable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string, jobID int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var state string
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx, "SELECT state FROM river_job WHERE id = $1", jobID).Scan(&state); err != nil {
			t.Fatalf("[%s] querying job state: %v", name, err)
		}
		if state != string(rivertype.JobStateAvailable) && state != string(rivertype.JobStateRunning) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("[%s] tokyo job (id=%d) was never attempted (state stuck at %q); "+
		"the takamatsu-job checks in this test would be a false negative", name, jobID, state)
}

// TestIngestQueueRename_ByQueuePreventsStaleQueueFromBlockingNewInsert は
// issue #185 のレビューで見つかった問題を再現し、修正（ByQueue: uniqueByQueue）が
// 効いていることを固定する。
//
// # 何が起きていたか
//
// River の UniqueOpts は既定 ByQueue: false で、そのとき一意キーは
// kind + args だけで組み立てられ Queue を含まない
// （river@v0.40.0/internal/dbunique/db_unique.go の buildUniqueKeyString）。
// このリポジトリの site 単位ジョブはすべて UniqueOpts{ByArgs: true,
// ByState: pendingJobStates} だったため、キュー名を `ingest` → `ingest_tokyo`
// に変えても一意キーは変わらず、デプロイ後に旧キュー（`ingest`）へ残っていた
// 行が新キュー（`ingest_tokyo`）への Insert を `UniqueSkippedAsDuplicate` として
// **エラーを返さず黙って**弾いていた。
//
// # このテストが検出すべき変異
//
// `uniqueByQueue`（internal/worker/worker.go）を `false` に戻す変異。
// この変異を注入すると、このテストの最後のアサーション
// （`UniqueSkippedAsDuplicate == false` を期待する）が
// `true` に反転して落ちる。実際に注入して確認済み（PR 本文の失敗出力を参照）。
func TestIngestQueueRename_ByQueuePreventsStaleQueueFromBlockingNewInsert(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	client, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	args := IngestJobArgs{Site: "tokyo", RecordID: "rec-1"}

	// 1. 「デプロイ前」の旧キュー・旧一意性設定（ByQueue なし）を明示的に再現する
	//    ---  client.Insert の第 3 引数（明示 InsertOpts）は
	//    JobArgsWithInsertOpts（IngestJobArgs.InsertOpts()）を上書きするので、
	//    ここだけ意図的に旧形（`Queue: "ingest"`、ByQueue 無し）を強制する。
	oldStyleOpts := &river.InsertOpts{
		Queue: "ingest",
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
	oldRes, err := client.Insert(ctx, args, oldStyleOpts)
	if err != nil {
		t.Fatalf("inserting old-style job: %v", err)
	}
	if oldRes.UniqueSkippedAsDuplicate {
		t.Fatal("old-style insert should not itself be a duplicate (river_job was just cleared)")
	}
	if oldRes.Job.Queue != "ingest" {
		t.Fatalf("old-style job queue = %q, want %q", oldRes.Job.Queue, "ingest")
	}

	// 2. 「デプロイ後」に同じ record を record_sweep が再投入する状況を再現する
	//    --- 今の IngestJobArgs.InsertOpts()（Queue: physicalQueueName で
	//    "ingest_tokyo"、ByQueue: uniqueByQueue）で同じ args を Insert する。
	newRes, err := client.Insert(ctx, args, nil)
	if err != nil {
		t.Fatalf("inserting new-style job: %v", err)
	}

	// ByQueue: uniqueByQueue が効いていれば、旧キューの行があっても
	// UniqueSkippedAsDuplicate にはならず、新しい行が作られる（新旧の
	// unique_key がキュー名を含むかどうかで異なるハッシュになるため）。
	// **この分岐が最初に来ることが重要**: バグがあるとき River は
	// UniqueSkippedAsDuplicate=true を返すだけでなく、res.Job に旧行
	// （Queue="ingest" のまま）を返す。Queue の比較を先に書くと「新しい行が
	// 作られたのに Queue が違う」という誤った理解を招く失敗メッセージになる
	// （実際に踏んだ。旧行に合流したので Queue も旧行のものが返っていた）。
	if newRes.UniqueSkippedAsDuplicate {
		t.Fatal("new-style insert was skipped as a duplicate of the stale old-queue row --- " +
			"ByQueue: uniqueByQueue is not effective; the record would never be re-ingested " +
			"(uniqueByQueue の定義を確認する。internal/worker/worker.go の pendingJobStates 直後の doc コメント参照)")
	}
	if newRes.Job.ID == oldRes.Job.ID {
		t.Fatal("new-style insert reused the old job's row instead of creating a new one")
	}
	if newRes.Job.Queue != "ingest_tokyo" {
		t.Fatalf("new-style job queue = %q, want %q", newRes.Job.Queue, "ingest_tokyo")
	}
}
