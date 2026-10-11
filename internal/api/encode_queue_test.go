package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
)

// setEncodeAttemptCount は seedEncodeAttempt が作った行の attempt_count を上書きする。
func setEncodeAttemptCount(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile string, count int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE recording_encode_attempts SET attempt_count = $3 WHERE recording_id = $1 AND profile = $2`,
		recordingID, profile, count); err != nil {
		t.Fatalf("setting attempt_count: %v", err)
	}
}

// encodeQueueFixture はドメイン状態だけで待機列の境界を作る。返す値は queued / running に
// 寄与する録画のタイトル。
func encodeQueueFixture(t *testing.T, pool *pgxpool.Pool) (queuedTitles, runningTitles []string) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	n := int64(900)
	mk := func(title string, profiles ...string) int64 {
		n++
		id := seedRecording(t, pool, title, base.Add(time.Duration(n)*time.Minute), "finished", int32(n))
		seedIngested(t, pool, id, 1000, nil)
		setEncodeProfiles(t, pool, id, profiles)
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	a := mk("待機と実行", "h264", "h265")
	seedEncodeAttempt(t, pool, a, "h264", "running", nil)

	b := mk("完了済み", "h265")
	seedEncodedAsset(t, pool, b, "h265", 1000)

	c := mk("削除中の encoded", "h264")
	exec(`INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, state)
		 VALUES ($1, 'encoded', 'h264', 'test/inactive_observed.mp4', 1000, 'deleting')`, c)

	d := mk("失敗 count 0", "h264")
	seedEncodeAttempt(t, pool, d, "h264", "failed", nil)
	e := mk("失敗 count 24", "h264")
	seedEncodeAttempt(t, pool, e, "h264", "failed", nil)
	setEncodeAttemptCount(t, pool, e, "h264", int(jobs.EncodeAttemptLimit)-1)
	f := mk("失敗 count 25", "h264")
	seedEncodeAttempt(t, pool, f, "h264", "failed", nil)
	setEncodeAttemptCount(t, pool, f, "h264", int(jobs.EncodeAttemptLimit))

	g := mk("観測済みなのに running 行", "h264")
	seedEncodedAsset(t, pool, g, "h264", 1000)
	seedEncodeAttempt(t, pool, g, "h264", "running", nil)

	h := mk("ごみ箱の running", "h264")
	seedEncodeAttempt(t, pool, h, "h264", "running", nil)
	exec(`UPDATE recordings SET deleted_at = now() WHERE id = $1`, h)

	u := mk("設定に無い profile の running", "removed")
	seedEncodeAttempt(t, pool, u, "removed", "running", nil)

	io := mk("active 原本なしの running", "h264")
	seedEncodeAttempt(t, pool, io, "h264", "running", nil)
	exec(`UPDATE media_assets SET state = 'deleted' WHERE recording_id = $1 AND kind = 'original'`, io)

	mk("未確認 cut", "cut")
	co := mk("確認済み cut", "cut")
	exec(`INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, co)

	return []string{"待機と実行", "削除中の encoded", "失敗 count 0", "失敗 count 24", "確認済み cut"},
		[]string{"待機と実行"}
}

func newEncodeQueueServer(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(RouterConfig{
		Pool:               pool,
		EncodeProfileNames: []string{"h264", "h265", "cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGetEncodeQueueSummaryFromDomainState(t *testing.T) {
	pool := testutil.SetupDB(t)
	queuedTitles, runningTitles := encodeQueueFixture(t, pool)
	srv := newEncodeQueueServer(t, pool)

	var got struct {
		Queued  int64 `json:"queued"`
		Running int64 `json:"running"`
	}
	resp := getJSON(t, srv.URL+"/api/encode-queue", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/encode-queue status = %d, want 200", resp.StatusCode)
	}
	if got.Queued != 5 || got.Running != 1 {
		t.Errorf("encode queue summary = queued %d, running %d; want queued 5, running 1", got.Queued, got.Running)
	}
	if got.Queued != int64(len(queuedTitles)) || got.Running != int64(len(runningTitles)) {
		t.Fatalf("fixture titles (%d queued, %d running) disagree with expected counts", len(queuedTitles), len(runningTitles))
	}

	// ListMissingEncodeProfiles は上限到達の失敗行も返す。待機列は queued + running = それ以外。
	missing, err := sqlcgen.New(pool).ListMissingEncodeProfiles(context.Background(), sqlcgen.ListMissingEncodeProfilesParams{
		KnownProfiles: []string{"h264", "h265", "cut"},
		CutProfiles:   []string{"cut"},
		RowLimit:      100,
	})
	if err != nil {
		t.Fatalf("listing missing encode profiles: %v", err)
	}
	const exhausted = 1
	if got.Queued+got.Running != int64(len(missing)-exhausted) {
		t.Errorf("queued + running = %d, ListMissingEncodeProfiles has %d rows and %d exhausted; want %d",
			got.Queued+got.Running, len(missing), exhausted, len(missing)-exhausted)
	}
}

// cut プロファイルが 1 つも無い構成でも、cut 以外のプロファイルは待機列に数える。
func TestGetEncodeQueueCountsWithoutCutProfiles(t *testing.T) {
	pool := testutil.SetupDB(t)
	id := seedRecording(t, pool, "cut 無し構成", time.Now().Truncate(time.Second), "finished", 951)
	seedIngested(t, pool, id, 1000, nil)
	setEncodeProfiles(t, pool, id, []string{"h264"})
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, EncodeProfileNames: []string{"h264"}}))
	t.Cleanup(srv.Close)

	var got struct {
		Queued  int64 `json:"queued"`
		Running int64 `json:"running"`
	}
	getJSON(t, srv.URL+"/api/encode-queue", &got)
	if got.Queued != 1 || got.Running != 0 {
		t.Errorf("encode queue = queued %d, running %d; want queued 1, running 0", got.Queued, got.Running)
	}
}

func TestListRecordingsEncodeStateMatchesQueueCounts(t *testing.T) {
	pool := testutil.SetupDB(t)
	wantQueued, wantRunning := encodeQueueFixture(t, pool)
	srv := newEncodeQueueServer(t, pool)

	for state, want := range map[string][]string{"queued": wantQueued, "running": wantRunning} {
		got := getRecordingsTitles(t, srv.URL, url.Values{"encodeState": {state}})
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("encodeState=%s got %v, want %v", state, got, want)
		}
	}
}

func TestListRecordingsRejectsUnknownEncodeState(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	resp := getJSON(t, srv.URL+"/api/recordings?encodeState=finished", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("encodeState=finished status = %d, want 400", resp.StatusCode)
	}
}
