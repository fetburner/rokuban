// Command mirakcmock は kind 上の受け入れ判定ハーネス（deploy/k8s/e2e）が使う
// mirakc のモックである。実機の mirakc はチューナー資源を要求し、EPG が揃うまで
// 数十分かかるため、CI にもローカルの kind にも載せられない。
//
// **実装するのは、いまハーネスの判定が通る経路だけ**である。rokuban は
// これ以外にも `GET/DELETE /api/recording/records/{id}` や
// `/api/services/{id}/stream` を叩くが（internal/mirakc/client.go）、判定が
// そこを通っていないのでモックは 501 を返す。**足す契機は「rokuban が新しい
// API を使い始めたとき」ではなく「判定が通る経路が増えたとき」**（判定 3 が
// 実物の encode / ingest を回し始めたら踏む）。未実装のパスを 404 ではなく
// 501 にしてあるのは、「モックが実装していない」と「mirakc が持っていない」を
// ハーネスの出力で区別するため。
//
// **レスポンスは internal/mirakc の型をそのまま組み立てて返す。** 写すと
// モックと製品でタグがズレうるし、ズレたときの症状は「番組表が空」で
// モックを疑うまでがいちばん遠い。
//
// **ただし、型を共有した以上このパッケージのテストは wire 名を守らない**
// （モックもクライアントも同じタグを見るので、rename しても対称に通る）。
// 実 mirakc に対する wire 名の固定は internal/mirakc の
// TestProgramWireNames / TestServiceWireNames / TestTunerWireNames /
// TestScheduleWireNames が持つ。ここが守っているのは「製品のクライアントで
// 読めて、製品が見るフィールドが埋まっている」ことまで。
//
// ハーネスがこのモックに要求する固有の機能は 2 つ:
//   - **`/events` の同時接続数を数えて `/mock/stats` で公開する**。判定 4
//     （watcher を 2 レプリカにしても二重に動かない）はこの数値だけで機械判定
//     できる --- watcher の singleton 性が主張しているのは「mirakc に N 本の
//     SSE を張らない」ことそのものだから（internal/role/leader.go の
//     パッケージコメント）。
//   - **`POST /mock/reset` で録画予約を空に戻す**。判定は同じクラスタを何度も
//     使い回すので、前回の周回で届いた予約が残っていると「今回 1 件も送って
//     いないのに緑」になる。
//
// 生成する EPG は**要求のたびに現在時刻から作る**ので、同じ programId の
// startAt は時間とともに未来へずれ続け、**どの番組も実際には開始しない**。
// 「録画が始まる」「開始時刻が安定している」ことを前提にする判定は、この
// モックに対しては書けない。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fetburner/rokuban/internal/mirakc"
)

func main() {
	listen := flag.String("listen", ":40772", "listen address")
	networkID := flag.Int("network-id", 32736, "network id of the generated services")
	serviceCount := flag.Int("services", 2, "number of generated services")
	programsPerService := flag.Int("programs", 8, "number of generated programs per service")
	programDuration := flag.Duration("program-duration", 30*time.Minute, "duration of each generated program")
	namePrefix := flag.String("name-prefix", "mock", "prefix of the generated service/program names")
	flag.Parse()

	m := newMock(*networkID, *serviceCount, *programsPerService, *programDuration, *namePrefix)

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	slog.Info("mirakcmock listening",
		"addr", *listen, "networkID", *networkID, "services", *serviceCount)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           m.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// /events は無期限に開き続けるので WriteTimeout を設定しない。
	}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// mock は生成した EPG と、POST された録画予約を保持する。
//
// 番組表は**要求のたびに now() から生成する**（起動時に固定しない）。固定すると
// クラスタを立ててから判定が走るまでの時間ぶん番組が過去に流れ、ruler が
// 「もう始まっている番組」として予約を作らない/GC する経路に落ちて、判定 1 が
// 環境の速さ次第で緑にも赤にもなる。
type mock struct {
	networkID          int
	serviceCount       int
	programsPerService int
	programDuration    time.Duration
	namePrefix         string

	// now は時刻の唯一の出どころ。テストが差し替える（差し替え口が無いと
	// 「ハンドラが固定時刻を使っている」という、この型が防ごうとしている
	// 実装をテストから作れない）。
	now func() time.Time

	mu        sync.Mutex
	schedules map[int64]mirakc.Schedule

	eventsOpen  atomic.Int64
	eventsTotal atomic.Int64
}

func newMock(networkID, serviceCount, programsPerService int, programDuration time.Duration, namePrefix string) *mock {
	return &mock{
		networkID:          networkID,
		serviceCount:       serviceCount,
		programsPerService: programsPerService,
		programDuration:    programDuration,
		namePrefix:         namePrefix,
		now:                time.Now,
		schedules:          map[int64]mirakc.Schedule{},
	}
}

type stats struct {
	// EventsOpen は現在開いている /events の本数。判定 4 が読む唯一の値。
	EventsOpen int64 `json:"eventsOpen"`
	// EventsTotal は起動からの /events 接続の延べ本数。0 のまま増えないことと
	// 「1 本開いている」を区別するために出す（watcher が一度も繋いでいないのに
	// EventsOpen == 1 を期待して待ち続ける、を避ける）。**reset では戻さない**
	// --- 判定 4 は「誰かが繋いだ」の positive control にこれを使うので、
	// 戻すと繋ぎ直しが起きるまで観測が成立しなくなる。
	EventsTotal int64 `json:"eventsTotal"`
	// Schedules は POST で作られた録画予約の件数（判定 1 が読む）。
	Schedules int `json:"schedules"`
}

func (m *mock) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/version", m.getVersion)
	mux.HandleFunc("GET /api/services", m.listServices)
	mux.HandleFunc("GET /api/programs", m.listPrograms)
	mux.HandleFunc("GET /api/tuners", m.listTuners)
	mux.HandleFunc("GET /api/recording/records", m.listRecords)
	mux.HandleFunc("GET /api/recording/schedules", m.listSchedules)
	mux.HandleFunc("POST /api/recording/schedules", m.createSchedule)
	mux.HandleFunc("GET /api/recording/schedules/{programId}", m.getSchedule)
	mux.HandleFunc("DELETE /api/recording/schedules/{programId}", m.deleteSchedule)
	mux.HandleFunc("GET /events", m.events)
	mux.HandleFunc("GET /mock/stats", m.getStats)
	mux.HandleFunc("POST /mock/reset", m.postReset)
	// 上の登録に当たらない要求は 501。404 にすると rokuban 側から見て
	// 「mirakc に無い」と区別できない。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		slog.Warn("unimplemented endpoint", "method", r.Method, "path", r.URL.Path)
		http.Error(w, "mirakcmock does not implement this endpoint", http.StatusNotImplemented)
	})
	return mux
}

func (m *mock) getVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, mirakc.Version{Current: "3.0.0-mock", Latest: "3.0.0-mock"})
}

func (m *mock) listServices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, m.services())
}

func (m *mock) listPrograms(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, m.programs(m.now()))
}

func (m *mock) listTuners(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []mirakc.Tuner{
		{Index: 0, Name: "mock-gr0", Types: []string{"GR"}, IsAvailable: true},
		{Index: 1, Name: "mock-gr1", Types: []string{"GR"}, IsAvailable: true},
	})
}

func (m *mock) listRecords(w http.ResponseWriter, _ *http.Request) {
	// 録画実行そのものはモックしない（判定 1 が見るのは「予約が mirakc に
	// 届くこと」まで）。空配列を返す。
	writeJSON(w, http.StatusOK, []mirakc.Record{})
}

func (m *mock) listSchedules(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mirakc.Schedule, 0, len(m.schedules))
	for _, s := range m.schedules {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Program.ID < out[j].Program.ID })
	writeJSON(w, http.StatusOK, out)
}

func (m *mock) createSchedule(w http.ResponseWriter, r *http.Request) {
	var in mirakc.ScheduleInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	p, ok := m.programByID(in.ProgramID, m.now())
	if !ok {
		// 実機は存在しない programId を 404 で返す。ここを 201 にすると
		// 「予約が届いた」の判定が、届き先が存在しない場合まで緑になる。
		http.Error(w, "program not found", http.StatusNotFound)
		return
	}
	s := mirakc.Schedule{State: "scheduled", Program: p, Options: in.Options, Tags: in.Tags}
	m.mu.Lock()
	m.schedules[in.ProgramID] = s
	m.mu.Unlock()
	slog.Info("schedule created", "programId", in.ProgramID, "tags", in.Tags)
	writeJSON(w, http.StatusCreated, s)
}

func (m *mock) getSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("programId"), 10, 64)
	if err != nil {
		http.Error(w, "bad programId", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	s, ok := m.schedules[id]
	m.mu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// deleteSchedule は 200 を返す。**204 にしないこと** ---
// internal/mirakc.Client.DeleteSchedule は checkStatus で 200 を厳密に要求する
// ので、変えると reconciler の GC と作り直し（DELETE → POST）が両方失敗し、
// 判定 1.7 が製品ではなくモックの都合で赤くなる。
func (m *mock) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("programId"), 10, 64)
	if err != nil {
		http.Error(w, "bad programId", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	_, ok := m.schedules[id]
	delete(m.schedules, id)
	m.mu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (m *mock) getStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, m.statsSnapshot())
}

// postReset は録画予約を空に戻す（ハーネスが 1 周ごとに呼ぶ）。
//
// **/events のカウンタは戻さない。** eventsOpen は現に開いている接続の数
// なので勝手に 0 にすると嘘になり、eventsTotal は判定 4 の positive control
// が使う（stats の doc コメント参照）。
func (m *mock) postReset(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	n := len(m.schedules)
	m.schedules = map[int64]mirakc.Schedule{}
	m.mu.Unlock()
	slog.Info("reset", "clearedSchedules", n)
	writeJSON(w, http.StatusOK, m.statsSnapshot())
}

// statsSnapshot は /mock/stats が返す値。ハンドラと同じものをテストから読むため
// に分けてある。
func (m *mock) statsSnapshot() stats {
	m.mu.Lock()
	n := len(m.schedules)
	m.mu.Unlock()
	return stats{
		EventsOpen:  m.eventsOpen.Load(),
		EventsTotal: m.eventsTotal.Load(),
		Schedules:   n,
	}
}

// events は mirakc の /events（SSE）。イベントは送らず、接続を開いたまま保つ。
//
// **判定 4 が数えるのはこのハンドラの同時実行数である。** 接続がいつ閉じたかを
// 取りこぼさないよう、増減は defer で対にし、r.Context().Done() で待つ
// （Flush だけで書き続ける形にすると、切断の検出が次の書き込みまで遅れる）。
func (m *mock) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	open := m.eventsOpen.Add(1)
	total := m.eventsTotal.Add(1)
	defer m.eventsOpen.Add(-1)
	slog.Info("events subscribed", "open", open, "total", total, "remote", r.RemoteAddr)

	// keepalive を送るのは、切断を検出するためではなく（それは Done で分かる）、
	// 経路上のプロキシに idle で切られないようにするため。
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			slog.Info("events unsubscribed", "open", m.eventsOpen.Load()-1)
			return
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ":keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// services は生成するサービス一覧。Mirakurun 互換の id（networkId * 100000 +
// serviceId）を使う。
//
// Channel.Type は EPG 射影が見る（internal/worker/epg.go の validChannelTypes）。
// 空にすると全サービスが skip され、番組が 1 件も入らない。
func (m *mock) services() []mirakc.Service {
	out := make([]mirakc.Service, 0, m.serviceCount)
	for i := range m.serviceCount {
		sid := 1024 + i
		out = append(out, mirakc.Service{
			ID:                 serviceID64(m.networkID, sid),
			ServiceID:          sid,
			NetworkID:          m.networkID,
			Type:               1,
			LogoID:             -1,
			RemoteControlKeyID: i + 1,
			Name:               fmt.Sprintf("%s service %d", m.namePrefix, i+1),
			Channel:            mirakc.ServiceChannel{Type: "GR", Channel: strconv.Itoa(13 + i)},
		})
	}
	return out
}

// programs は now を基準に生成する番組表。**先頭の番組は now より後に始まる**
// （mock の doc コメント参照）。
func (m *mock) programs(now time.Time) []mirakc.Program {
	base := now.Add(10 * time.Minute).Truncate(time.Minute)
	out := make([]mirakc.Program, 0, m.serviceCount*m.programsPerService)
	for i := range m.serviceCount {
		sid := 1024 + i
		for j := range m.programsPerService {
			start := mirakc.Milliseconds(base.Add(time.Duration(j) * m.programDuration))
			duration := m.programDuration.Milliseconds()
			eid := 1 + j
			name := fmt.Sprintf("%s program s%d e%d", m.namePrefix, i+1, eid)
			out = append(out, mirakc.Program{
				ID:        programID64(m.networkID, sid, eid),
				EventID:   eid,
				ServiceID: sid,
				NetworkID: m.networkID,
				StartAt:   &start,
				Duration:  &duration,
				IsFree:    true,
				Name:      &name,
			})
		}
	}
	return out
}

func (m *mock) programByID(id int64, now time.Time) (mirakc.Program, bool) {
	for _, p := range m.programs(now) {
		if p.ID == id {
			return p, true
		}
	}
	return mirakc.Program{}, false
}

func serviceID64(networkID, serviceID int) int64 {
	return int64(networkID)*100000 + int64(serviceID)
}

func programID64(networkID, serviceID, eventID int) int64 {
	return serviceID64(networkID, serviceID)*100000 + int64(eventID)
}

// writeJSON は status を書いてから本文を書く。**Content-Type は WriteHeader
// より前に設定する**（後だと無視されて text/plain で返り、curl で覗いたときに
// 本物の mirakc と違って見える）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encoding response", "err", err)
	}
}
