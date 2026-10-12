package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/jobq"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/testutil"
)

func runWorkerViaJobQFake[T jobq.Args](ctx context.Context, worker jobq.Worker[T], args T) error {
	return jobqfaketest.RunWorkerOnce(ctx, worker, args)
}

// singleSiteClients は 1 site だけを束縛するテスト用の Deps.MirakcClients /
// *Worker.MirakcClients を組み立てる（issue #532 で単一の *mirakc.Client
// フィールドが map になったことに伴うテストヘルパー）。site を省略（""）した
// 呼び出しは db.DefaultSite を使う --- 大半のワーカーテストは複数 site を
// 気にしないので、単一サイトの map を毎回手で組み立てずに済ませる。
func singleSiteClients(site string, client *mirakc.Client) map[string]*mirakc.Client {
	if site == "" {
		site = db.DefaultSite
	}
	return map[string]*mirakc.Client{site: client}
}

type recordingEnqueuer struct {
	mu   sync.Mutex
	args []jobq.Args
}

func newRecordingEnqueuer() *recordingEnqueuer { return &recordingEnqueuer{} }

func (enqueuer *recordingEnqueuer) Enqueue(_ context.Context, args jobq.Args, _ ...jobq.EnqueueOpt) (jobq.Result, error) {
	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	enqueuer.args = append(enqueuer.args, args)
	return jobq.Result{}, nil
}

func (*recordingEnqueuer) EnqueueTx(context.Context, db.Tx, jobq.Args, ...jobq.EnqueueOpt) (jobq.Result, error) {
	return jobq.Result{}, errors.New("recordingEnqueuer does not support transactions")
}

func (enqueuer *recordingEnqueuer) Enqueued(kind string) []jobq.Args {
	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	var found []jobq.Args
	for _, args := range enqueuer.args {
		if args.Kind() == kind {
			found = append(found, args)
		}
	}
	return found
}

func (enqueuer *recordingEnqueuer) Reset() {
	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	enqueuer.args = nil
}

func enqueuedArgs[T jobq.Args](enqueuer *recordingEnqueuer, kind string) []T {
	var found []T
	for _, args := range enqueuer.Enqueued(kind) {
		if typed, ok := args.(T); ok {
			found = append(found, typed)
		}
	}
	return found
}

func countEnqueuedForRecording(t *testing.T, enqueuer *recordingEnqueuer, kind string, recordingID int64) int {
	t.Helper()
	count := 0
	for _, args := range enqueuer.Enqueued(kind) {
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("encoding %s args: %v", kind, err)
		}
		var keyed struct {
			RecordingID int64 `json:"recording_id"`
		}
		if err := json.Unmarshal(encoded, &keyed); err != nil {
			t.Fatalf("decoding %s args: %v", kind, err)
		}
		if keyed.RecordingID == recordingID {
			count++
		}
	}
	return count
}

// TestVerifySite は issue #532 の受け入れ基準 3 を固定する: verifySite は
// clients（束縛サイトの map）に無い jobSite を拒み、ある jobSite は通して
// そのクライアントを返す。
//
// このテストが検出すべき変異: verifySite が clients の中身を見ずに常に通す
// （map の判定 `clients[site]` を消して常に何らかのクライアントを返す等）。
// その場合 "unbound site is refused" が誤って通ってしまい、このテストが落ちる。
func TestVerifySite(t *testing.T) {
	tokyoClient := mirakc.NewClient("http://tokyo.example", nil)
	takamatsuClient := mirakc.NewClient("http://takamatsu.example", nil)
	clients := map[string]*mirakc.Client{
		"tokyo":     tokyoClient,
		"takamatsu": takamatsuClient,
	}

	t.Run("bound site is allowed and returns its own client", func(t *testing.T) {
		got, err := verifySite(clients, "tokyo", "ingest")
		if err != nil {
			t.Fatalf("verifySite: %v", err)
		}
		if got != tokyoClient {
			t.Errorf("verifySite returned %v, want the tokyo client", got)
		}
	})

	t.Run("a second bound site is also allowed (N-site binding)", func(t *testing.T) {
		got, err := verifySite(clients, "takamatsu", "ingest")
		if err != nil {
			t.Fatalf("verifySite: %v", err)
		}
		if got != takamatsuClient {
			t.Errorf("verifySite returned %v, want the takamatsu client", got)
		}
	})

	t.Run("unbound site is refused", func(t *testing.T) {
		if _, err := verifySite(clients, "osaka", "ingest"); err == nil {
			t.Fatal("verifySite() error = nil, want error for a site not in clients")
		}
	})

	// jobSite は正規化しない: 空文字列は無条件に拒む。これは "default" という
	// 名前の site がたまたま束縛されているデプロイでも同じでなければならない
	// --- 正規化すると、args.Site 自体が壊れている（本来 verifySite が拾うべき）
	// ジョブが「default site 宛のジョブ」として素通りしてしまう。
	t.Run("empty jobSite is refused even when a site named default is bound", func(t *testing.T) {
		defaultOnly := singleSiteClients("", tokyoClient)
		if _, err := verifySite(defaultOnly, "", "ingest"); err == nil {
			t.Fatal("verifySite() error = nil, want error for an empty (unnormalized) jobSite")
		}
	})
}

// epg_sync のパス完了はヒント経路の 1 つ: RulerPassArgs を投入して評価を早める
// （docs/recording.md §3.1「EPG 同期の完了」）。
func TestEpgSyncWorker_EnqueuesRulerPassHint(t *testing.T) {
	srv := newEpgServer(t, &epgFixture{})
	enqueuer := newRecordingEnqueuer()
	w := &EpgSyncWorker{
		Enqueuer:      enqueuer,
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		Pool:          testutil.SetupDB(t),
	}
	if err := runWorkerViaJobQFake(context.Background(), w, EpgSyncArgs{Site: testSite}); err != nil {
		t.Fatalf("EpgSyncWorker.Work: %v", err)
	}
	hints := enqueuedArgs[jobs.RulerPassArgs](enqueuer, "ruler_pass")
	if len(hints) != 1 || hints[0].Site != testSite {
		t.Fatalf("enqueued ruler_pass hints = %+v, want one hint for site %q", hints, testSite)
	}
}

// 区切り + 処理余白の上限は River の既定 JobTimeout より長く、rescue 待ちより短い。
func TestIngestWorker_TimeoutFitsBeforeRescue(t *testing.T) {
	args := jobs.IngestJobArgs{}
	got := args.Spec().Timeout(args)
	if got != 5*time.Minute {
		t.Errorf("Timeout() = %v, want 5m", got)
	}
	if got := ingestTransferSlice; got != 4*time.Minute {
		t.Errorf("ingest transfer slice = %v, want 4m", got)
	}
}

// EPG 同期は無制限にせず、10 分で打ち切る。
func TestEpgSyncWorker_HasGenerousTimeout(t *testing.T) {
	args := EpgSyncArgs{}
	got := args.Spec().Timeout(args)
	if got != 10*time.Minute {
		t.Errorf("Timeout() = %v, want 10m", got)
	}
}

// ruler は Args.Spec で既定より長い有限の締切を持つ。
func TestRulerPassWorker_HasGenerousTimeout(t *testing.T) {
	args := jobs.RulerPassArgs{}
	got := args.Spec().Timeout(args)
	if got != 5*time.Minute {
		t.Errorf("Timeout() = %v, want 5m", got)
	}
}

func TestLabelRuleReconcileWorker_TimeoutUsesSpec(t *testing.T) {
	args := jobs.LabelRuleReconcileArgs{}
	got := args.Spec().Timeout(args)
	if got != time.Minute {
		t.Errorf("Timeout() = %v, want Spec timeout 1m", got)
	}
}

func TestEncodeRebuildWorker_TimeoutUsesSpec(t *testing.T) {
	args := jobs.EncodeRebuildArgs{}
	got := args.Spec().Timeout(args)
	if got != time.Minute {
		t.Errorf("Timeout() = %v, want Spec timeout 1m", got)
	}
}

// ruler_pass ジョブが投入され、実際に実行されて予約が作られること
// （ロジック自体は internal/ruler でテスト済みなので、ここでは「ジョブとして
// 呼ばれると本当に動く」という配線だけを確認する）。
func TestRulerPassWorker_CreatesReservation(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	const site = "default"
	const networkID, serviceID int32 = 32736, 1024
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ($1, $2, $3, 1, 0, 1, 'テスト局', 'GR', '27', false)`, site, networkID, serviceID); err != nil {
		t.Fatalf("inserting epg_services fixture: %v", err)
	}

	const programID int64 = 1
	startAt := time.Now().Add(time.Hour)
	const durationMs int64 = 1800000
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_programs (site, program_id, network_id, service_id, event_id, start_at, duration_ms, end_at, is_free, name, description)
VALUES ($1, $2, $3, $4, 0, $5, $6, $7, true, 'テスト番組', '')`,
		site, programID, networkID, serviceID, startAt, durationMs, startAt.Add(time.Duration(durationMs)*time.Millisecond)); err != nil {
		t.Fatalf("inserting epg_programs fixture: %v", err)
	}

	var ruleID int64
	if err := pool.QueryRow(ctx, `INSERT INTO rules (name, priority) VALUES ('全部録る', 10) RETURNING id`).Scan(&ruleID); err != nil {
		t.Fatalf("inserting rule fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO rule_text_matches (rule_id, seq, target, mode, value, case_sensitive, negate)
VALUES ($1, 0, 'name', 'keyword', 'テスト', true, false)`, ruleID); err != nil {
		t.Fatalf("inserting rule_text_matches fixture: %v", err)
	}

	if err := runWorkerViaJobQFake(ctx, &RulerPassWorker{Pool: pool}, RulerPassArgs{Site: site}); err != nil {
		t.Fatalf("RulerPassWorker.Work: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reservations WHERE site = $1 AND program_id = $2`, site, programID,
	).Scan(&count); err != nil {
		t.Fatalf("querying reservations: %v", err)
	}
	if count != 1 {
		t.Fatalf("reservations count = %d, want 1 (ruler_pass ジョブがルール評価を実行して予約を作ったはず)", count)
	}

	var markerCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ruler_pass_snapshots WHERE site = $1`, site,
	).Scan(&markerCount); err != nil {
		t.Fatalf("querying ruler pass marker: %v", err)
	}
	if markerCount != 1 {
		t.Errorf("ruler pass marker rows = %d, want 1", markerCount)
	}
}

// ruler の処理が失敗したときは、ScaledJob の再試行を促すため marker を進めない。
func TestRulerPassWorker_DoesNotMarkFailedPass(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w := &RulerPassWorker{Pool: pool}
	job := RulerPassArgs{Site: "default"}
	if err := runWorkerViaJobQFake(ctx, w, job); err == nil {
		t.Fatal("Work() error = nil, want canceled ruler pass to fail")
	}

	var markerCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ruler_pass_snapshots WHERE site = $1`, "default",
	).Scan(&markerCount); err != nil {
		t.Fatalf("querying ruler pass marker: %v", err)
	}
	if markerCount != 0 {
		t.Errorf("ruler pass marker rows after failed pass = %d, want 0", markerCount)
	}
}
