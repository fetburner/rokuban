package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mirakc"
)

func newTestMock() *mock {
	return newMock(32736, 2, 3, 30*time.Minute, "test")
}

func newTestServer(t *testing.T) (*mock, *mirakc.Client, *httptest.Server) {
	t.Helper()
	m := newTestMock()
	srv := httptest.NewServer(m.routes())
	t.Cleanup(srv.Close)
	return m, mirakc.NewClient(srv.URL, srv.Client()), srv
}

// TestClientDecodesEveryEndpoint は、**製品のクライアントで**モックの全
// エンドポイントを読み、製品が実際に見るフィールドが埋まっていることを見る。
//
// **モックの中で完結するテストにしないこと。** 自前の型でデコードし直す形は
// JSON タグを何に変えても対称に通るので何も主張しない（実測で確認済み）。
//
// ただし**このテストも wire 名は守らない**。モックは internal/mirakc の型を
// そのまま組み立てて返すので、タグを rename すればモックもクライアントも
// 一緒に動く。実 mirakc に対する wire 名の固定は internal/mirakc の
// TestProgramWireNames ほかが持つ。ここが主張するのは「製品のクライアントで
// 読めて、**製品が実際に見るフィールドが埋まっている**」ことまで
// （埋め忘れ ---「Channel.Type を空にした」「Name を nil にした」--- は
// ここで落ちる）。
func TestClientDecodesEveryEndpoint(t *testing.T) {
	m, client, _ := newTestServer(t)
	ctx := context.Background()

	if _, err := client.GetVersion(ctx); err != nil {
		t.Fatalf("GetVersion: %v", err)
	}

	services, err := client.ListServices(ctx)
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(services) != m.serviceCount {
		t.Fatalf("got %d services, want %d", len(services), m.serviceCount)
	}
	for _, s := range services {
		// epg_sync は channel_type が GR/BS/CS/SKY でないサービスを捨てる
		// （internal/worker/epg.go の validChannelTypes）。空だと全件 skip
		// されて EPG が入らない。
		if s.Channel.Type != "GR" {
			t.Fatalf("service %d: channel type %q, want GR", s.ServiceID, s.Channel.Type)
		}
		if s.Name == "" || s.NetworkID == 0 || s.ServiceID == 0 {
			t.Fatalf("service decoded with empty fields: %+v", s)
		}
	}

	programs, err := client.ListPrograms(ctx)
	if err != nil {
		t.Fatalf("ListPrograms: %v", err)
	}
	if len(programs) != m.serviceCount*m.programsPerService {
		t.Fatalf("got %d programs, want %d", len(programs), m.serviceCount*m.programsPerService)
	}
	for _, p := range programs {
		// projectable()（internal/worker/epg.go）が要求する 2 つ。
		if p.StartAt == nil {
			t.Fatalf("program %d decoded with nil startAt", p.ID)
		}
		if p.Name == nil || *p.Name == "" {
			t.Fatalf("program %d decoded with empty name", p.ID)
		}
		if p.Duration == nil || *p.Duration == 0 {
			t.Fatalf("program %d decoded with nil/zero duration", p.ID)
		}
		if !p.StartAt.Time().After(time.Now()) {
			t.Fatalf("program %d starts at %v, want the future", p.ID, p.StartAt.Time())
		}
	}

	tuners, err := client.ListTuners(ctx)
	if err != nil {
		t.Fatalf("ListTuners: %v", err)
	}
	if len(tuners) == 0 {
		t.Fatal("got no tuners")
	}
	for _, tn := range tuners {
		// 容量判定（internal/capacity）は Types を必ず見る。
		if len(tn.Types) == 0 || tn.Name == "" {
			t.Fatalf("tuner decoded with empty fields: %+v", tn)
		}
	}

	if _, err := client.ListRecords(ctx); err != nil {
		t.Fatalf("ListRecords: %v", err)
	}

	// 予約の作成 → 一覧 → 単体取得 → 削除。reconciler が回す一巡そのもの。
	target := programs[0]
	created, err := client.CreateSchedule(ctx, mirakc.ScheduleInput{
		ProgramID: target.ID,
		Options:   mirakc.Options{Priority: 3},
		Tags:      []string{"rokuban"},
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if created.Program.ID != target.ID || created.State == "" {
		t.Fatalf("created schedule decoded as %+v", created)
	}
	if created.Options.Priority != 3 || len(created.Tags) != 1 {
		t.Fatalf("created schedule lost options/tags: %+v", created)
	}

	listed, err := client.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(listed) != 1 || listed[0].Program.ID != target.ID {
		t.Fatalf("got %+v, want one schedule for program %d", listed, target.ID)
	}

	got, err := client.GetSchedule(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if got.Program.ID != target.ID {
		t.Fatalf("GetSchedule returned program %d, want %d", got.Program.ID, target.ID)
	}

	// DeleteSchedule は 200 を厳密に要求する（内部で checkStatus）。
	if err := client.DeleteSchedule(ctx, target.ID); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	if n := m.statsSnapshot().Schedules; n != 0 {
		t.Fatalf("got %d schedules after delete, want 0", n)
	}
}

// TestProgramsFollowTheClockThroughTheHandler は、番組表が**要求のたびに**
// 現在時刻から生成されることを HTTP 経由で見る。
//
// **ハンドラを通す**のが要点。純関数 m.programs(t) を 2 つの引数で呼ぶ形だと、
// 「ハンドラが起動時に固定した時刻を使う」という、まさに防ぎたい実装を
// 作っても緑のまま通る（実測）。固定されると、クラスタを立ててから判定が
// 走るまでの時間ぶん番組が過去へ流れる。
func TestProgramsFollowTheClockThroughTheHandler(t *testing.T) {
	m, client, _ := newTestServer(t)
	ctx := context.Background()

	fixed := time.Now()
	var mu sync.Mutex
	m.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return fixed
	}

	first, err := client.ListPrograms(ctx)
	if err != nil {
		t.Fatalf("ListPrograms: %v", err)
	}

	mu.Lock()
	fixed = fixed.Add(2 * time.Hour)
	advanced := fixed
	mu.Unlock()

	second, err := client.ListPrograms(ctx)
	if err != nil {
		t.Fatalf("ListPrograms: %v", err)
	}

	if len(first) != len(second) || len(first) == 0 {
		t.Fatalf("got %d then %d programs", len(first), len(second))
	}
	if first[0].ID != second[0].ID {
		t.Fatalf("program id changed with the clock: %d -> %d", first[0].ID, second[0].ID)
	}
	if !second[0].StartAt.Time().After(first[0].StartAt.Time()) {
		t.Fatalf("startAt did not advance with the clock: %v -> %v",
			first[0].StartAt.Time(), second[0].StartAt.Time())
	}
	if !second[0].StartAt.Time().After(advanced) {
		t.Fatalf("startAt %v is not after the advanced clock %v",
			second[0].StartAt.Time(), advanced)
	}
}

// TestCreateScheduleRejectsUnknownProgram は、存在しない programId への予約を
// 404 にすることを固定する。ここを 201 にすると、判定 1 の「予約が mirakc に
// 反映される」が**届き先が存在しない場合まで緑になる**。
func TestCreateScheduleRejectsUnknownProgram(t *testing.T) {
	m, client, _ := newTestServer(t)

	if _, err := client.CreateSchedule(context.Background(), mirakc.ScheduleInput{ProgramID: 1}); err == nil {
		t.Fatal("CreateSchedule for an unknown program succeeded, want an error")
	}
	if n := m.statsSnapshot().Schedules; n != 0 {
		t.Fatalf("got %d schedules, want 0", n)
	}
}

// TestResetClearsSchedulesButNotEventCounters は判定の周回間の掃除を固定する。
//
// 予約が残っていると、判定 1.7（予約が mirakc に届く）が**今回 1 件も送って
// いなくても緑**になる。逆に eventsTotal まで戻すと、判定 4 の positive
// control（誰かが繋いだこと）が接続し直しまで成立しなくなる。
func TestResetClearsSchedulesButNotEventCounters(t *testing.T) {
	m, client, srv := newTestServer(t)
	ctx := context.Background()

	programs, err := client.ListPrograms(ctx)
	if err != nil {
		t.Fatalf("ListPrograms: %v", err)
	}
	if _, err := client.CreateSchedule(ctx, mirakc.ScheduleInput{ProgramID: programs[0].ID}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	m.eventsTotal.Store(3)

	resp, err := srv.Client().Post(srv.URL+"/mock/reset", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /mock/reset: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /mock/reset: status %d", resp.StatusCode)
	}

	got := m.statsSnapshot()
	if got.Schedules != 0 {
		t.Fatalf("got %d schedules after reset, want 0", got.Schedules)
	}
	if got.EventsTotal != 3 {
		t.Fatalf("eventsTotal = %d after reset, want 3 (must not be cleared)", got.EventsTotal)
	}
}

// TestEventsOpenCountTracksConnections は判定 4 の土台を固定する。**この数値が
// 増えも減りもしないなら、判定 4 は watcher が何本 SSE を張っても緑になる。**
func TestEventsOpenCountTracksConnections(t *testing.T) {
	m, _, srv := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	const conns = 2
	for range conns {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
		if err != nil {
			cancel()
			t.Fatalf("new request: %v", err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			cancel()
			t.Fatalf("GET /events: %v", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// ヘッダは返っているので、本文を読み切るまで待たずにカウントは
			// 上がっている。切断まで読み続ける。
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}

	waitForEventsOpen(t, m, conns)
	if total := m.eventsTotal.Load(); total != conns {
		t.Fatalf("eventsTotal = %d, want %d", total, conns)
	}

	cancel()
	wg.Wait()
	waitForEventsOpen(t, m, 0)
}

func waitForEventsOpen(t *testing.T, m *mock, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := m.eventsOpen.Load(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("eventsOpen = %d, want %d", m.eventsOpen.Load(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStatsJSONKeys は `/mock/stats` の**キー名**を固定する。
//
// このエンドポイントの唯一の消費者は Go ではなくシェル（lib/kube.sh の
// mock_stat が `json.load(...)[field]` で引く）なので、構造体を直接読む
// テストでは何も守れない。キーを変えると判定 4 だけが壊れる（幸い
// mock_stat は空文字を返し 4.2 が「測定できない」で赤くなるので、黙って
// 緑にはならない）。
func TestStatsJSONKeys(t *testing.T) {
	m, _, srv := newTestServer(t)
	m.eventsOpen.Store(1)
	m.eventsTotal.Store(2)

	resp, err := srv.Client().Get(srv.URL + "/mock/stats")
	if err != nil {
		t.Fatalf("GET /mock/stats: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// **リテラルで書く**（構造体のタグから生成しない）。lib/kube.sh の
	// mock_stat と checks/04 がこの綴りで引いている。
	for key, want := range map[string]float64{"eventsOpen": 1, "eventsTotal": 2, "schedules": 0} {
		v, ok := got[key]
		if !ok {
			t.Fatalf("%q is missing from /mock/stats: %v", key, got)
		}
		if n, ok := v.(float64); !ok || n != want {
			t.Errorf("%s = %v, want %v", key, v, want)
		}
	}
}

// TestUnimplementedEndpointIsNotFoundAs501 は、モックが実装していないパスを
// 404 ではなく 501 で返すことを見る。404 だと rokuban 側のログで「mirakc に
// 無い」と区別が付かない。
func TestUnimplementedEndpointIsNotFoundAs501(t *testing.T) {
	_, _, srv := newTestServer(t)

	resp, err := srv.Client().Get(srv.URL + "/api/channels")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("got status %d, want 501", resp.StatusCode)
	}
}
