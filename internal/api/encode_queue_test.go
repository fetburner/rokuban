package api

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	jobqtestutil "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

// setEncodeAttemptCount は seedEncodeAttempt が作った profile の行の attempt_count を上書きする。
func setEncodeAttemptCount(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile string, count int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE recording_encode_attempts SET attempt_count = $2 WHERE recording_id = $1 AND profile = $3`,
		recordingID, count, profile); err != nil {
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
		id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
			p.Title, p.ProgramStartAt, p.Status, p.EventID = title, base.Add(time.Duration(n)*time.Minute), "finished", int32(n)
		})
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
	dbtest.MediaAsset(t, pool, b, func(p *dbtest.MediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = new("h265")
		p.SizeBytes = 1000
	})

	c := mk("削除中の encoded", "h264")
	profile := "h264"
	cAsset := dbtest.MediaAsset(t, pool, c, func(p *dbtest.MediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = &profile
		p.RelPath = "test/inactive_observed.mp4"
		p.SizeBytes = 1000
	})
	exec(`UPDATE media_assets SET state = 'deleting' WHERE id = $1`, cAsset)

	d := mk("失敗 count 0", "h264")
	seedEncodeAttempt(t, pool, d, "h264", "failed", nil)
	e := mk("失敗 count 24", "h264")
	seedEncodeAttempt(t, pool, e, "h264", "failed", nil)
	setEncodeAttemptCount(t, pool, e, "h264", int(jobs.EncodeAttemptLimit)-1)
	f := mk("失敗 count 25", "h264")
	seedEncodeAttempt(t, pool, f, "h264", "failed", nil)
	setEncodeAttemptCount(t, pool, f, "h264", int(jobs.EncodeAttemptLimit))

	g := mk("観測済みなのに running 行", "h264")
	dbtest.MediaAsset(t, pool, g, func(p *dbtest.MediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = new("h264")
		p.SizeBytes = 1000
	})
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
	rebuild := mk("明示的な作り直し", "cut")
	exec(`INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, rebuild)
	dbtest.MediaAsset(t, pool, rebuild, func(p *dbtest.MediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = new("cut")
		p.SizeBytes = 1000
	})
	exec(`INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
		 VALUES ($1, 'cut', '{[0,1000)}'::int8multirange)`, rebuild)

	// 確認（所有）が無くても、押した時点の keep が確認なので作り直しは待機列に入る。
	unowned := mk("未確認の作り直し", "cut")
	dbtest.MediaAsset(t, pool, unowned, func(p *dbtest.MediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = new("cut")
		p.SizeBytes = 1000
	})
	exec(`INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
		 VALUES ($1, 'cut', '{[0,1000)}'::int8multirange)`, unowned)
	// 凍結区間が requested_keep と等しい要求は満たされていて、待機列に入らない。
	satisfied := mk("満たされた作り直し", "cut")
	dbtest.MediaAsset(t, pool, satisfied, func(p *dbtest.MediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = new("cut")
		p.SizeBytes = 1000
	})
	exec(`INSERT INTO media_asset_cuts (media_asset_id, keep_ranges)
		 SELECT id, '{[0,1000)}'::int8multirange FROM media_assets WHERE recording_id = $1 AND kind = 'encoded'`, satisfied)
	exec(`INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
		 VALUES ($1, 'cut', '{[0,1000)}'::int8multirange)`, satisfied)

	return []string{"待機と実行", "削除中の encoded", "失敗 count 0", "失敗 count 24", "確認済み cut", "明示的な作り直し", "未確認の作り直し"},
		[]string{"待機と実行"}
}

func newEncodeQueueServer(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(RouterConfig{
		Ready:              pool.Ping,
		RecordingQuery:     pgstore.NewRecordings(pool),
		MediaReader:        pgstore.NewMedia(pool),
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
	if got.Queued != 7 || got.Running != 1 {
		t.Errorf("encode queue summary = queued %d, running %d; want queued 7, running 1", got.Queued, got.Running)
	}
	if got.Queued != int64(len(queuedTitles)) || got.Running != int64(len(runningTitles)) {
		t.Fatalf("fixture titles (%d queued, %d running) disagree with expected counts", len(queuedTitles), len(runningTitles))
	}

	// ListMissingEncodeProfiles は投入候補（不足と作り直し）を合わせて返す。
	missing, err := sqlcgen.New(pool).ListMissingEncodeProfiles(context.Background(), sqlcgen.ListMissingEncodeProfilesParams{
		KnownProfiles: []string{"h264", "h265", "cut"},
		CutProfiles:   []string{"cut"},
		AttemptLimit:  jobs.EncodeAttemptLimit,
		RowLimit:      100,
	})
	if err != nil {
		t.Fatalf("listing missing encode profiles: %v", err)
	}
	if got.Queued+got.Running != int64(len(missing)) {
		t.Errorf("queued + running = %d, ListMissingEncodeProfiles has %d rows; want equal counts",
			got.Queued+got.Running, len(missing))
	}
}

// cut プロファイルが 1 つも無い構成でも、cut 以外のプロファイルは待機列に数える。
func TestGetEncodeQueueCountsWithoutCutProfiles(t *testing.T) {
	pool := testutil.SetupDB(t)
	id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "cut 無し構成", time.Now().Truncate(time.Second), "finished", 951
	})
	seedIngested(t, pool, id, 1000, nil)
	setEncodeProfiles(t, pool, id, []string{"h264"})
	srv := httptest.NewServer(NewRouter(RouterConfig{Ready: pool.Ping,
		RecordingQuery: pgstore.NewRecordings(pool), MediaReader: pgstore.NewMedia(pool), EncodeProfileNames: []string{"h264"}}))
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

// TestEncodeReconcileAndQueueAgreeOnPendingSet は、encode を投入する経路
// （worker.EncodeReconcileWorker の全件パス。不足の補完と作り直し要求の両方）と、
// 待機列（GET /api/encode-queue と GET /api/recordings?encodeState=）が同じ録画の
// 集合に合意することを固定する。
//
// 2 つの経路は同じ述語（view unencoded_desired_profiles / encodable_recordings と
// 関数 encodable_profile）を参照するので、ずれうるのは各クエリへの直書きと、
// 設定から引数を組み立てる配線である。そのため両側を同じ config.EncodeConfig から
// 組み立て、待機列側はハンドラを実物の HTTP 経路で通す（RouterConfig への渡し方は
// cmd/rokuban/server.go と同じ ProfileNames / CutProfileNames）。各録画の desired は
// 1 つなので、録画の集合の一致は (recording_id, profile) の集合の一致と同じになる
// （queued + running の件数で確かめる）。
//
// フィクスチャは除外の軸を 1 つずつ持つ録画を並べ、期待する集合もリテラルで書く
// （両経路が同じ向きにずれても落とす）。残る非対称は全区間カットで keep が空の
// cut プロファイルだけである。reconcile は Go の keep の導出（chapters.Derive）で
// 空と分かったものを投入しない（投入しても observeEncodePlan が必ず失敗する）。待機列は
// SQL だけで数えるので、keep の導出を持てず数え続ける。導出を SQL に複製すると Go と
// SQL の 2 か所になるので、揃えずに残す。
func TestEncodeReconcileAndQueueAgreeOnPendingSet(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	base := time.Now().Truncate(time.Second)
	n := int64(1200)
	seed := func(title, profile string) int64 {
		t.Helper()
		n++
		id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
			p.Title, p.ProgramStartAt, p.Status, p.EventID = title, base.Add(time.Duration(n)*time.Minute), "finished", int32(n)
		})
		seedIngested(t, pool, id, 1000, nil)
		setEncodeProfiles(t, pool, id, []string{profile})
		return id
	}
	adopt := func(id int64, span string) {
		t.Helper()
		exec(`INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, id)
		exec(`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, $2::int8range, 'CM', true)`, id, span)
	}
	cmSpan := []chapters.Span{{StartMs: 600000, EndMs: 900000, Label: "CM", Cut: true}}
	seedRebuild := func(title string) int64 {
		t.Helper()
		id := seed(title, "cut")
		adopt(id, "[600000,900000)")
		dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) {
			p.Kind = "encoded"
			p.Profile = new("cut")
			p.SizeBytes = 1000
		})
		exec(`INSERT INTO media_asset_cuts (media_asset_id, keep_ranges)
			 SELECT id, '{[0,1800000)}'::int8multirange FROM media_assets
			 WHERE recording_id = $1 AND kind = 'encoded' AND profile = 'cut'`, id)
		keep, err := pgstore.KeepRangesParam(chapters.KeepRanges(chapters.Derive(true, cmSpan, nil, (30 * time.Minute).Milliseconds())))
		if err != nil {
			t.Fatal(err)
		}
		if err := sqlcgen.New(pool).UpsertEncodedAssetRebuildRequest(ctx, sqlcgen.UpsertEncodedAssetRebuildRequestParams{
			RecordingID: id, Profile: "cut", RequestedKeep: keep,
		}); err != nil {
			t.Fatalf("seeding rebuild request: %v", err)
		}
		return id
	}
	trash := func(id int64) { exec(`UPDATE recordings SET deleted_at = now() WHERE id = $1`, id) }
	dropOriginal := func(id int64) {
		exec(`UPDATE media_assets SET state = 'deleted' WHERE recording_id = $1 AND kind = 'original'`, id)
	}

	seed("不足", "h264")
	deleting := seed("削除中の版", "h264")
	profile := "h264"
	deletingAsset := dbtest.MediaAsset(t, pool, deleting, func(p *dbtest.MediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = &profile
		p.RelPath = "test/agree_deleting.mp4"
		p.SizeBytes = 1000
	})
	exec(`UPDATE media_assets SET state = 'deleting' WHERE id = $1`, deletingAsset)
	trash(seed("ごみ箱", "h264"))
	dropOriginal(seed("原本なし", "h264"))
	seed("設定に無い", "gone")
	seed("cut 未確認", "cut")
	adopt(seed("cut 確認済み", "cut"), "[600000,900000)")
	seedRebuild("作り直し要求")
	trash(seedRebuild("ごみ箱の作り直し要求"))
	dropOriginal(seedRebuild("原本なしの作り直し要求"))
	below := seed("試行上限の手前", "h264")
	seedEncodeAttempt(t, pool, below, "h264", "failed", nil)
	setEncodeAttemptCount(t, pool, below, "h264", int(jobs.EncodeAttemptLimit)-1)
	exhausted := seed("試行上限", "h264")
	seedEncodeAttempt(t, pool, exhausted, "h264", "failed", nil)
	setEncodeAttemptCount(t, pool, exhausted, "h264", int(jobs.EncodeAttemptLimit))
	exhaustedRebuild := seedRebuild("試行上限の作り直し")
	seedEncodeAttempt(t, pool, exhaustedRebuild, "cut", "failed", nil)
	setEncodeAttemptCount(t, pool, exhaustedRebuild, "cut", int(jobs.EncodeAttemptLimit))
	adopt(seed("全区間カット", "cut"), "[0,1800000)")

	profiles := config.EncodeConfig{Profiles: []config.EncodeProfile{
		{Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"},
		{Name: "cut", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"},
	}}

	enqueuer := newRecordingEnqueuer()
	if err := jobqtestutil.RunWorkerOnce(ctx, &worker.EncodeReconcileWorker{Enqueuer: enqueuer, Media: pgstore.NewMedia(pool), Profiles: profiles}, jobs.EncodeReconcileArgs{}); err != nil {
		t.Fatalf("EncodeReconcileWorker.Work: %v", err)
	}
	reconciled := map[string]bool{}
	for _, call := range enqueuer.CallsOfKind(jobs.EncodeJobArgs{}.Kind()) {
		args, ok := call.Args.(jobs.EncodeJobArgs)
		if !ok {
			t.Fatalf("encode args = %T, want jobs.EncodeJobArgs", call.Args)
		}
		var title string
		if err := pool.QueryRow(ctx, `SELECT title FROM recordings WHERE id = $1`, args.RecordingID).Scan(&title); err != nil {
			t.Fatal(err)
		}
		reconciled[title] = true
	}
	assertEncodeQueueMatchesReconcile(t, pool, profiles, reconciled)
}

func assertEncodeQueueMatchesReconcile(t *testing.T, pool *pgxpool.Pool, profiles config.EncodeConfig, reconciled map[string]bool) {
	t.Helper()
	srv := httptest.NewServer(NewRouter(RouterConfig{
		Ready:              pool.Ping,
		RecordingQuery:     pgstore.NewRecordings(pool),
		MediaReader:        pgstore.NewMedia(pool),
		EncodeProfileNames: profiles.ProfileNames(),
		CutProfileNames:    profiles.CutProfileNames(),
	}))
	t.Cleanup(srv.Close)
	queued := map[string]bool{}
	for _, state := range []string{"queued", "running"} {
		for _, title := range getRecordingsTitles(t, srv.URL, url.Values{"encodeState": {state}}) {
			queued[title] = true
		}
	}
	var summary struct {
		Queued  int64 `json:"queued"`
		Running int64 `json:"running"`
	}
	if resp := getJSON(t, srv.URL+"/api/encode-queue", &summary); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/encode-queue status = %d, want 200", resp.StatusCode)
	}
	if got := summary.Queued + summary.Running; got != int64(len(queued)) {
		t.Errorf("encode queue rows = %d, recordings in the queue = %d; want one pending profile per recording", got, len(queued))
	}

	if reconciled["試行上限"] || queued["試行上限"] {
		t.Errorf("exhausted attempts: reconciled %v, queued %v; want neither", reconciled["試行上限"], queued["試行上限"])
	}
	if reconciled["試行上限の作り直し"] || queued["試行上限の作り直し"] {
		t.Errorf("exhausted rebuild attempts: reconciled %v, queued %v; want neither", reconciled["試行上限の作り直し"], queued["試行上限の作り直し"])
	}
	if reconciled["全区間カット"] || !queued["全区間カット"] {
		t.Errorf("all-cut keep: reconciled %v, queued %v; want queued only", reconciled["全区間カット"], queued["全区間カット"])
	}
	delete(queued, "全区間カット")

	for title := range reconciled {
		if !queued[title] {
			t.Errorf("%q is enqueued by reconcile but missing from the encode queue", title)
		}
	}
	for title := range queued {
		if !reconciled[title] {
			t.Errorf("%q is in the encode queue but reconcile does not enqueue it", title)
		}
	}
	want := []string{"不足", "削除中の版", "cut 確認済み", "作り直し要求", "試行上限の手前"}
	slices.Sort(want)
	for name, got := range map[string]map[string]bool{"reconcile": reconciled, "queue": queued} {
		titles := slices.Sorted(maps.Keys(got))
		if !slices.Equal(titles, want) {
			t.Errorf("%s = %v, want %v", name, titles, want)
		}
	}
}
