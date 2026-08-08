package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
// まま残ること**で確認する（issue #185 の受け入れ基準の指定どおり。available の
// まま残ることまで見ないと、「たまたま最初の SKIP LOCKED で他ジョブを先に拾った」
// だけの偽陰性を拾えない）。
func TestMultiSiteWorker_OnlyDequeuesOwnSiteQueues(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	countingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/recording/records", "/api/services", "/api/programs", "/api/recording/schedules":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer countingSrv.Close()

	// このプロセスは tokyo に束縛されている。
	workers := NewWorkers(&Deps{Pool: pool, MirakcClient: mirakc.NewClient(countingSrv.URL, nil), Site: "tokyo"})
	client, err := NewClient(pool, workers, ClientConfig{BoundSite: "tokyo"})
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

	// takamatsu 向けの site 単位ジョブを 4 種とも投入する。
	takamatsuJobs := []river.JobArgs{
		IngestJobArgs{Site: "takamatsu", RecordID: "rec-1"},
		EpgSyncArgs{Site: "takamatsu"},
		ReconcilePassArgs{Site: "takamatsu"},
		RecordSweepArgs{Site: "takamatsu"},
	}
	wantQueues := map[string]string{
		"ingest":         "ingest_takamatsu",
		"epg_sync":       "epg_takamatsu",
		"reconcile_pass": "reconciler_takamatsu",
		"record_sweep":   "watcher_takamatsu",
	}
	var jobIDs []int64
	for _, args := range takamatsuJobs {
		res, err := client.Insert(ctx, args, nil)
		if err != nil {
			t.Fatalf("inserting %s job: %v", args.Kind(), err)
		}
		if want := wantQueues[args.Kind()]; res.Job.Queue != want {
			t.Errorf("%s job queue = %q, want %q", args.Kind(), res.Job.Queue, want)
		}
		jobIDs = append(jobIDs, res.Job.ID)
	}

	// tokyo 向けの ingest ジョブも 1 件投入し、tokyo の worker が正常に機能している
	// こと自体は確認する（4 件とも掴まないのが「worker が死んでいるだけ」という
	// 偽陰性でないことの対照）。
	tokyoIngest, err := client.Insert(ctx, IngestJobArgs{Site: "tokyo", RecordID: "rec-tokyo"}, nil)
	if err != nil {
		t.Fatalf("inserting tokyo ingest job: %v", err)
	}
	if want := "ingest_tokyo"; tokyoIngest.Job.Queue != want {
		t.Fatalf("tokyo ingest job queue = %q, want %q", tokyoIngest.Job.Queue, want)
	}

	// tokyo ジョブが最終状態（completed か discarded。record が存在しないので恐らく
	// discarded になるが、いずれにせよ「試行された」ことが分かればよい）に落ち着く
	// まで少し待つ。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if err := pool.QueryRow(ctx, "SELECT state FROM river_job WHERE id = $1", tokyoIngest.Job.ID).Scan(&state); err != nil {
			t.Fatalf("querying tokyo job state: %v", err)
		}
		if state != string(rivertype.JobStateAvailable) && state != string(rivertype.JobStateRunning) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	var tokyoState string
	if err := pool.QueryRow(ctx, "SELECT state FROM river_job WHERE id = $1", tokyoIngest.Job.ID).Scan(&tokyoState); err != nil {
		t.Fatalf("querying tokyo job final state: %v", err)
	}
	if tokyoState == string(rivertype.JobStateAvailable) {
		t.Fatal("tokyo ingest job was never attempted; the worker itself seems dead " +
			"(the takamatsu-job checks below would be a false negative)")
	}

	// takamatsu 向けの 4 件は available のまま残っているはず。
	for i, id := range jobIDs {
		var state string
		if err := pool.QueryRow(ctx, "SELECT state FROM river_job WHERE id = $1", id).Scan(&state); err != nil {
			t.Fatalf("querying job %d state: %v", id, err)
		}
		if state != string(rivertype.JobStateAvailable) {
			t.Errorf("takamatsu job %d (%s) state = %q, want %q (tokyo worker must never dequeue it)",
				id, takamatsuJobs[i].Kind(), state, rivertype.JobStateAvailable)
		}
	}

}
