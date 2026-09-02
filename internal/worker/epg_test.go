package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
)

const testSite = "default"

// epgFixture は mirakc の /api/services と /api/programs のレスポンスを差し替え可能に保持する。
type epgFixture struct {
	services []mirakc.Service
	programs []mirakc.Program
}

func newEpgServer(t *testing.T, fx *epgFixture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/services":
			_ = json.NewEncoder(w).Encode(fx.services)
		case "/api/programs":
			_ = json.NewEncoder(w).Encode(fx.programs)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func msPtr(t time.Time) *mirakc.Milliseconds {
	m := mirakc.Milliseconds(t)
	return &m
}

func int64Ptr(v int64) *int64 { return &v }

func testService(networkID, serviceID, remoteKey int, name, channel string) mirakc.Service {
	return mirakc.Service{
		ID:                 mirakc.ServiceID(networkID, serviceID),
		ServiceID:          serviceID,
		NetworkID:          networkID,
		Type:               1,
		LogoID:             remoteKey,
		RemoteControlKeyID: remoteKey,
		Name:               name,
		Channel:            mirakc.ServiceChannel{Type: "GR", Channel: channel},
		HasLogoData:        true,
	}
}

func testProgram(networkID, serviceID, eventID int, name string, start time.Time, dur time.Duration) mirakc.Program {
	desc := name + "の説明"
	return mirakc.Program{
		ID:          mirakc.ComposeProgramID(networkID, serviceID, eventID),
		EventID:     eventID,
		ServiceID:   serviceID,
		NetworkID:   networkID,
		StartAt:     msPtr(start),
		Duration:    int64Ptr(dur.Milliseconds()),
		IsFree:      true,
		Name:        &name,
		Description: &desc,
		Extended:    map[string]string{"出演者": "テスト太郎"},
		Genres:      []mirakc.Genre{{LV1: 7, LV2: 0, UN1: 15, UN2: 15}},
		Video: &mirakc.VideoInfo{
			Type: strPtr("mpeg2"), Resolution: strPtr("1080i"),
			StreamContent: 1, ComponentType: 179,
		},
		Audios: []mirakc.AudioInfo{{ComponentType: 3, IsMain: true, SamplingRate: 48000, Langs: []string{"jpn"}}},
	}
}

func runEpgSync(t *testing.T, w *EpgSyncWorker) {
	t.Helper()
	job := &river.Job[EpgSyncArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   EpgSyncArgs{Site: testSite},
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
}

// allPrograms は site の全番組を program_id 昇順で返す。
func allPrograms(t *testing.T, w *EpgSyncWorker) []sqlcgen.EpgProgram {
	t.Helper()
	rows, err := sqlcgen.New(w.Pool).ListEpgPrograms(context.Background(), sqlcgen.ListEpgProgramsParams{
		Site:        testSite,
		WindowStart: time.Now().Add(-100 * 24 * time.Hour),
		WindowEnd:   time.Now().Add(100 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("ListEpgPrograms: %v", err)
	}
	slices.SortFunc(rows, func(a, b sqlcgen.EpgProgram) int { return int(a.ProgramID - b.ProgramID) })
	return rows
}

func TestEpgSyncWorker_FullSync(t *testing.T) {
	pool := setupTestPool(t)
	now := time.Now().Truncate(time.Second)

	fx := &epgFixture{
		services: []mirakc.Service{
			testService(32736, 1024, 1, "ＮＨＫ総合", "27"),
			testService(32737, 1032, 4, "テレビ東京", "23"),
		},
		programs: []mirakc.Program{
			testProgram(32736, 1024, 1, "ニュース", now.Add(-10*time.Minute), 30*time.Minute),
			testProgram(32736, 1024, 2, "ドラマ", now.Add(20*time.Minute), time.Hour),
			testProgram(32737, 1032, 1, "アニメ", now.Add(2*time.Hour), 30*time.Minute),
		},
	}
	srv := newEpgServer(t, fx)

	w := &EpgSyncWorker{MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)), Pool: pool, RetentionGrace: 24 * time.Hour}
	runEpgSync(t, w)

	q := sqlcgen.New(pool)
	ctx := context.Background()

	services, err := q.ListEpgServices(ctx, testSite)
	if err != nil {
		t.Fatalf("ListEpgServices: %v", err)
	}
	if len(services) != 2 {
		t.Fatalf("services = %d, want 2", len(services))
	}
	// 既定ソートは channel_type, remote_control_key_id
	if services[0].Name != "ＮＨＫ総合" || services[1].Name != "テレビ東京" {
		t.Errorf("service order = %q, %q; want ＮＨＫ総合, テレビ東京", services[0].Name, services[1].Name)
	}
	if services[0].NetworkID != 32736 || services[0].ServiceID != 1024 ||
		services[0].ChannelType != "GR" || services[0].Channel != "27" || !services[0].HasLogoData {
		t.Errorf("service[0] = %+v", services[0])
	}

	programs := allPrograms(t, w)
	if len(programs) != 3 {
		t.Fatalf("programs = %d, want 3", len(programs))
	}

	news := programs[0]
	if news.Name != "ニュース" || news.Description != "ニュースの説明" {
		t.Errorf("name/description = %q / %q", news.Name, news.Description)
	}
	if news.NetworkID != 32736 || news.ServiceID != 1024 || news.EventID != 1 {
		t.Errorf("identity = %d/%d/%d", news.NetworkID, news.ServiceID, news.EventID)
	}
	if !news.StartAt.Equal(now.Add(-10 * time.Minute)) {
		t.Errorf("start_at = %v, want %v", news.StartAt, now.Add(-10*time.Minute))
	}
	if news.DurationMs != (30 * time.Minute).Milliseconds() {
		t.Errorf("duration_ms = %d", news.DurationMs)
	}
	// end_at は start_at + duration_ms をアプリが計算して書く（刈り取りの軸）
	if want := news.StartAt.Add(30 * time.Minute); !news.EndAt.Equal(want) {
		t.Errorf("end_at = %v, want %v", news.EndAt, want)
	}
	if !news.IsFree {
		t.Error("is_free = false, want true")
	}
	if !reflect.DeepEqual(news.GenreLv1, []int16{7}) {
		t.Errorf("genre_lv1 = %v, want [7]", news.GenreLv1)
	}

	// jsonb 詳細ペイロードが往復すること
	var extended map[string]string
	if err := json.Unmarshal(news.Extended, &extended); err != nil {
		t.Fatalf("unmarshalling extended: %v", err)
	}
	if extended["出演者"] != "テスト太郎" {
		t.Errorf("extended = %v", extended)
	}
	var video mirakc.VideoInfo
	if err := json.Unmarshal(news.Video, &video); err != nil {
		t.Fatalf("unmarshalling video: %v", err)
	}
	if video.Resolution == nil || *video.Resolution != "1080i" {
		t.Errorf("video = %+v", video)
	}
	var audios []mirakc.AudioInfo
	if err := json.Unmarshal(news.Audios, &audios); err != nil {
		t.Fatalf("unmarshalling audios: %v", err)
	}
	if len(audios) != 1 || audios[0].SamplingRate != 48000 {
		t.Errorf("audios = %+v", audios)
	}
	var genres []mirakc.Genre
	if err := json.Unmarshal(news.Genres, &genres); err != nil {
		t.Fatalf("unmarshalling genres: %v", err)
	}
	if len(genres) != 1 || genres[0].LV1 != 7 || genres[0].UN1 != 15 {
		t.Errorf("genres = %+v", genres)
	}
}

// 全量同期は冪等。2 回走っても行が増えず、内容が更新される。
func TestEpgSyncWorker_IdempotentAndUpdates(t *testing.T) {
	pool := setupTestPool(t)
	now := time.Now().Truncate(time.Second)

	fx := &epgFixture{
		services: []mirakc.Service{testService(32736, 1024, 1, "ＮＨＫ総合", "27")},
		programs: []mirakc.Program{
			testProgram(32736, 1024, 1, "ニュース", now.Add(time.Hour), 30*time.Minute),
		},
	}
	srv := newEpgServer(t, fx)
	w := &EpgSyncWorker{MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)), Pool: pool, RetentionGrace: 24 * time.Hour}

	runEpgSync(t, w)

	// 番組名と開始時刻が EPG 更新で変わったとする
	renamed := testProgram(32736, 1024, 1, "ニュース（拡大版）", now.Add(90*time.Minute), time.Hour)
	fx.programs = []mirakc.Program{renamed}
	fx.services[0].Name = "ＮＨＫ総合・改"

	runEpgSync(t, w)

	programs := allPrograms(t, w)
	if len(programs) != 1 {
		t.Fatalf("programs = %d, want 1 (upsert should not duplicate)", len(programs))
	}
	if programs[0].Name != "ニュース（拡大版）" {
		t.Errorf("name = %q, want 更新後の名前", programs[0].Name)
	}
	if !programs[0].StartAt.Equal(now.Add(90 * time.Minute)) {
		t.Errorf("start_at = %v, want %v", programs[0].StartAt, now.Add(90*time.Minute))
	}

	services, err := sqlcgen.New(pool).ListEpgServices(context.Background(), testSite)
	if err != nil {
		t.Fatalf("ListEpgServices: %v", err)
	}
	if len(services) != 1 || services[0].Name != "ＮＨＫ総合・改" {
		t.Errorf("services = %+v", services)
	}
}

// EPG から消えた番組・サービスは observed_at スイープで削除される。
func TestEpgSyncWorker_RemovesVanishedRows(t *testing.T) {
	pool := setupTestPool(t)
	now := time.Now().Truncate(time.Second)

	keep := testProgram(32736, 1024, 1, "残る番組", now.Add(time.Hour), 30*time.Minute)
	vanish := testProgram(32736, 1024, 2, "消える番組", now.Add(2*time.Hour), 30*time.Minute)

	fx := &epgFixture{
		services: []mirakc.Service{
			testService(32736, 1024, 1, "残るサービス", "27"),
			testService(32737, 1032, 4, "消えるサービス", "23"),
		},
		programs: []mirakc.Program{keep, vanish},
	}
	srv := newEpgServer(t, fx)
	w := &EpgSyncWorker{MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)), Pool: pool, RetentionGrace: 24 * time.Hour}

	runEpgSync(t, w)
	if got := len(allPrograms(t, w)); got != 2 {
		t.Fatalf("after first sync programs = %d, want 2", got)
	}

	// mirakc 側から消える（番組編成変更・サービス削除）
	fx.programs = []mirakc.Program{keep}
	fx.services = fx.services[:1]

	runEpgSync(t, w)

	programs := allPrograms(t, w)
	if len(programs) != 1 {
		t.Fatalf("programs = %d, want 1", len(programs))
	}
	if programs[0].ProgramID != keep.ID {
		t.Errorf("remaining program = %d, want %d", programs[0].ProgramID, keep.ID)
	}

	services, err := sqlcgen.New(pool).ListEpgServices(context.Background(), testSite)
	if err != nil {
		t.Fatalf("ListEpgServices: %v", err)
	}
	if len(services) != 1 || services[0].Name != "残るサービス" {
		t.Errorf("services = %+v", services)
	}
}

// mirakc が一時的に空リストを返しても（再起動直後で EPG 未ロード等）
// プロジェクション全体を消さないこと。
func TestEpgSyncWorker_EmptyResponseKeepsProjection(t *testing.T) {
	pool := setupTestPool(t)
	now := time.Now().Truncate(time.Second)

	fx := &epgFixture{
		services: []mirakc.Service{testService(32736, 1024, 1, "ＮＨＫ総合", "27")},
		programs: []mirakc.Program{
			testProgram(32736, 1024, 1, "番組A", now.Add(time.Hour), time.Hour),
			testProgram(32736, 1024, 2, "番組B", now.Add(2*time.Hour), time.Hour),
		},
	}
	srv := newEpgServer(t, fx)
	w := &EpgSyncWorker{MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)), Pool: pool, RetentionGrace: 24 * time.Hour}

	runEpgSync(t, w)
	if got := len(allPrograms(t, w)); got != 2 {
		t.Fatalf("after first sync programs = %d, want 2", got)
	}

	fx.services = []mirakc.Service{}
	fx.programs = []mirakc.Program{}
	runEpgSync(t, w)

	if got := len(allPrograms(t, w)); got != 2 {
		t.Errorf("programs = %d, want 2 — 空レスポンスでプロジェクションが消えた", got)
	}
	services, err := sqlcgen.New(pool).ListEpgServices(context.Background(), testSite)
	if err != nil {
		t.Fatalf("ListEpgServices: %v", err)
	}
	if len(services) != 1 {
		t.Errorf("services = %d, want 1 — 空レスポンスでサービスが消えた", len(services))
	}

	// mirakc が復帰したら通常どおりスイープが働くこと
	fx.services = []mirakc.Service{testService(32736, 1024, 1, "ＮＨＫ総合", "27")}
	fx.programs = []mirakc.Program{testProgram(32736, 1024, 1, "番組A", now.Add(time.Hour), time.Hour)}
	runEpgSync(t, w)

	if got := len(allPrograms(t, w)); got != 1 {
		t.Errorf("after recovery programs = %d, want 1 (スイープが働いていない)", got)
	}
}

// mirakc の EPG 収集はチャンネル単位なので、あるチャンネルだけ番組を返さなかった場合は
// そのチャンネルの番組を消さず、番組を返したチャンネルは通常どおりスイープすること。
func TestEpgSyncWorker_SweepIsPerChannel(t *testing.T) {
	pool := setupTestPool(t)
	now := time.Now().Truncate(time.Second)

	// GR/27 = ＯＨＫ（network 32678、サービス 5168 / 5169）
	// GR/21 = ＲＳＫ（network 32676、サービス 5152）
	ohkKeep := testProgram(32678, 5168, 1, "ＯＨＫ 残る番組", now.Add(time.Hour), time.Hour)
	ohkGone := testProgram(32678, 5168, 2, "ＯＨＫ 消える番組", now.Add(2*time.Hour), time.Hour)
	rskA := testProgram(32676, 5152, 1, "ＲＳＫ 番組A", now.Add(time.Hour), time.Hour)
	rskB := testProgram(32676, 5152, 2, "ＲＳＫ 番組B", now.Add(2*time.Hour), time.Hour)

	fx := &epgFixture{
		services: []mirakc.Service{
			testService(32678, 5168, 8, "ＯＨＫ", "27"),
			testService(32678, 5169, 8, "ＯＨＫ サブ", "27"),
			testService(32676, 5152, 6, "ＲＳＫテレビ", "21"),
		},
		programs: []mirakc.Program{ohkKeep, ohkGone, rskA, rskB},
	}
	srv := newEpgServer(t, fx)
	w := &EpgSyncWorker{MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)), Pool: pool, RetentionGrace: 24 * time.Hour}

	runEpgSync(t, w)
	if got := len(allPrograms(t, w)); got != 4 {
		t.Fatalf("after first sync programs = %d, want 4", got)
	}

	// ＲＳＫ（GR/21）の収集が失敗して番組がまったく返らず、
	// ＯＨＫ（GR/27）では 1 番組が編成から消えた、という状況
	fx.programs = []mirakc.Program{ohkKeep}
	runEpgSync(t, w)

	programs := allPrograms(t, w)
	got := make([]int64, len(programs))
	for i, p := range programs {
		got[i] = p.ProgramID
	}
	// ＯＨＫ は観測できたので消える番組がスイープされ、
	// ＲＳＫ は観測できなかったので 2 件とも残る
	want := []int64{ohkKeep.ID, rskA.ID, rskB.ID}
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		names := make([]string, len(programs))
		for i, p := range programs {
			names[i] = p.Name
		}
		t.Errorf("programs = %v (%v), want %v", got, names, want)
	}
}

// サブサービスのマルチ編成が終わって番組が 0 件になった場合、
// 同一チャンネルの他サービスが観測できていれば古い行がスイープされること。
// （スイープをサービス単位にしてしまうとこの行が残ってしまう）
func TestEpgSyncWorker_SweepsSubServiceWhenChannelObserved(t *testing.T) {
	pool := setupTestPool(t)
	now := time.Now().Truncate(time.Second)

	main := testProgram(32678, 5168, 1, "親サービスの番組", now.Add(time.Hour), time.Hour)
	multi := testProgram(32678, 5169, 1, "マルチ編成の番組", now.Add(time.Hour), time.Hour)

	fx := &epgFixture{
		services: []mirakc.Service{
			testService(32678, 5168, 8, "ＯＨＫ", "27"),
			testService(32678, 5169, 8, "ＯＨＫ サブ", "27"),
		},
		programs: []mirakc.Program{main, multi},
	}
	srv := newEpgServer(t, fx)
	w := &EpgSyncWorker{MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)), Pool: pool, RetentionGrace: 24 * time.Hour}

	runEpgSync(t, w)
	if got := len(allPrograms(t, w)); got != 2 {
		t.Fatalf("after first sync programs = %d, want 2", got)
	}

	// マルチ編成が終わり、サブサービス側の番組が無くなった
	fx.programs = []mirakc.Program{main}
	runEpgSync(t, w)

	programs := allPrograms(t, w)
	if len(programs) != 1 || programs[0].ProgramID != main.ID {
		t.Errorf("programs = %+v, want マルチ編成の番組がスイープされて親だけ残る", programs)
	}
}

// 投影できる番組が 1 件もない場合もスイープを見送ること
// （startAt なしばかりが返る異常時に番組表を消さない）。
func TestEpgSyncWorker_NoProjectableProgramsKeepsProjection(t *testing.T) {
	pool := setupTestPool(t)
	now := time.Now().Truncate(time.Second)

	fx := &epgFixture{
		services: []mirakc.Service{testService(32736, 1024, 1, "ＮＨＫ総合", "27")},
		programs: []mirakc.Program{testProgram(32736, 1024, 1, "番組A", now.Add(time.Hour), time.Hour)},
	}
	srv := newEpgServer(t, fx)
	w := &EpgSyncWorker{MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)), Pool: pool, RetentionGrace: 24 * time.Hour}

	runEpgSync(t, w)

	broken := testProgram(32736, 1024, 2, "壊れた番組", now.Add(time.Hour), time.Hour)
	broken.StartAt = nil
	fx.programs = []mirakc.Program{broken}
	runEpgSync(t, w)

	if got := len(allPrograms(t, w)); got != 1 {
		t.Errorf("programs = %d, want 1 — 投影可能 0 件でプロジェクションが消えた", got)
	}
}

// ローリングウィンドウ: 放送終了から猶予を超えた番組は、mirakc が返し続けても刈り取る。
func TestEpgSyncWorker_PrunesAiredPrograms(t *testing.T) {
	pool := setupTestPool(t)
	now := time.Now().Truncate(time.Second)

	oldProgram := testProgram(32736, 1024, 1, "3 日前の番組", now.Add(-72*time.Hour), time.Hour)
	recent := testProgram(32736, 1024, 2, "1 時間前に終わった番組", now.Add(-2*time.Hour), time.Hour)
	future := testProgram(32736, 1024, 3, "未来の番組", now.Add(time.Hour), time.Hour)

	fx := &epgFixture{
		services: []mirakc.Service{testService(32736, 1024, 1, "ＮＨＫ総合", "27")},
		programs: []mirakc.Program{oldProgram, recent, future},
	}
	srv := newEpgServer(t, fx)

	w := &EpgSyncWorker{
		MirakcClients:  singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		Pool:           pool,
		RetentionGrace: 24 * time.Hour,
	}
	runEpgSync(t, w)

	programs := allPrograms(t, w)
	ids := make([]int64, len(programs))
	for i, p := range programs {
		ids[i] = p.ProgramID
	}
	// 猶予内（1 時間前終了）と未来の番組は残り、猶予超え（3 日前）は消える
	want := []int64{recent.ID, future.ID}
	slices.Sort(want)
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("remaining program ids = %v, want %v", ids, want)
	}
}

// 投影できない番組・サービスは捨てて同期は続行する。
func TestEpgSyncWorker_SkipsUnprojectableRows(t *testing.T) {
	pool := setupTestPool(t)
	now := time.Now().Truncate(time.Second)

	noStart := testProgram(32736, 1024, 9, "開始時刻なし", now.Add(time.Hour), time.Hour)
	noStart.StartAt = nil

	noDuration := testProgram(32736, 1024, 8, "長さ未定", now.Add(time.Hour), time.Hour)
	noDuration.Duration = nil

	// 詳細ペイロードが一切ない番組（jsonb が NULL になること）
	minimal := mirakc.Program{
		ID: 3273610240007, EventID: 7, ServiceID: 1024, NetworkID: 32736,
		StartAt: msPtr(now.Add(3 * time.Hour)), Duration: int64Ptr(600000),
		Name: strPtr("詳細なし"),
	}

	badService := testService(32738, 2048, 9, "未知の伝送路", "0")
	badService.Channel.Type = "UNKNOWN"

	fx := &epgFixture{
		services: []mirakc.Service{testService(32736, 1024, 1, "ＮＨＫ総合", "27"), badService},
		programs: []mirakc.Program{noStart, noDuration, minimal},
	}
	srv := newEpgServer(t, fx)
	w := &EpgSyncWorker{MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)), Pool: pool, RetentionGrace: 24 * time.Hour}

	runEpgSync(t, w)

	services, err := sqlcgen.New(pool).ListEpgServices(context.Background(), testSite)
	if err != nil {
		t.Fatalf("ListEpgServices: %v", err)
	}
	if len(services) != 1 || services[0].ServiceID != 1024 {
		t.Errorf("services = %+v, want only the GR service", services)
	}

	programs := allPrograms(t, w)
	if len(programs) != 2 {
		t.Fatalf("programs = %d, want 2 (startAt なしは捨てる)", len(programs))
	}
	for _, p := range programs {
		if p.ProgramID == noStart.ID {
			t.Error("program without startAt was projected")
		}
		if p.ProgramID == noDuration.ID {
			// 長さ未定は duration 0 で投影する（end_at == start_at）
			if p.DurationMs != 0 || !p.EndAt.Equal(p.StartAt) {
				t.Errorf("nil duration: duration_ms = %d, end_at = %v, start_at = %v",
					p.DurationMs, p.EndAt, p.StartAt)
			}
		}
		if p.ProgramID == minimal.ID {
			// description は NOT NULL DEFAULT ''、jsonb 詳細は NULL
			if p.Description != "" {
				t.Errorf("minimal program description = %q, want empty", p.Description)
			}
			if p.Extended != nil || p.Genres != nil || p.Video != nil || p.Audios != nil {
				t.Errorf("minimal program should have NULL jsonb details, got %+v", p)
			}
			if len(p.GenreLv1) != 0 {
				t.Errorf("genre_lv1 = %v, want empty", p.GenreLv1)
			}
		}
	}
}

// サブサービスの影の行（同じ eventId で name が null）を投影しないこと。
// マルチ編成の実番組は name を持つので残る（issue #17 の決定）。
func TestEpgSyncWorker_SkipsShadowSubServicePrograms(t *testing.T) {
	pool := setupTestPool(t)
	now := time.Now().Truncate(time.Second)

	// GR/27 に親 1024 とサブ 1025 / 1026。ＮＨＫ総合１/２ 相当の構成。
	main := testProgram(32736, 1024, 100, "ニュース", now.Add(time.Hour), time.Hour)

	// 同じ eventId・name が null の影の行が 2 サービス分返ってくる
	shadow1 := testProgram(32736, 1025, 100, "", now.Add(time.Hour), time.Hour)
	shadow1.Name = nil
	shadow2 := testProgram(32736, 1026, 100, "", now.Add(time.Hour), time.Hour)
	shadow2.Name = nil

	// サブサービスの独立編成（マルチ編成）。name を持つので残るべき。
	multi := testProgram(32736, 1025, 200, "第１０８回全国高校野球選手権香川大会 決勝", now.Add(3*time.Hour), 3*time.Hour)

	// name が空文字のケースも影と同じ扱い
	emptyName := testProgram(32736, 1026, 300, "", now.Add(7*time.Hour), time.Hour)

	fx := &epgFixture{
		services: []mirakc.Service{
			testService(32736, 1024, 1, "ＮＨＫ総合１", "27"),
			testService(32736, 1025, 1, "ＮＨＫ総合２", "27"),
			testService(32736, 1026, 1, "ＮＨＫ総合３", "27"),
		},
		programs: []mirakc.Program{main, shadow1, shadow2, multi, emptyName},
	}
	srv := newEpgServer(t, fx)
	w := &EpgSyncWorker{MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)), Pool: pool, RetentionGrace: 24 * time.Hour}

	runEpgSync(t, w)

	programs := allPrograms(t, w)
	got := make([]int64, len(programs))
	for i, p := range programs {
		got[i] = p.ProgramID
	}
	want := []int64{main.ID, multi.ID}
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		names := make([]string, len(programs))
		for i, p := range programs {
			names[i] = p.Name
		}
		t.Errorf("projected ids = %v (%v), want %v (親の実番組とマルチ編成のみ)", got, names, want)
	}

	// サービスは全件投影する（空列を隠すのは UI の関心事 = S3）
	services, err := sqlcgen.New(pool).ListEpgServices(context.Background(), testSite)
	if err != nil {
		t.Fatalf("ListEpgServices: %v", err)
	}
	if len(services) != 3 {
		t.Errorf("services = %d, want 3 — サービスは影のサブサービスも投影する", len(services))
	}
}

// epgBatchSize を超える件数がチャンク分割されて全件入ること。
func TestEpgSyncWorker_BatchesLargeSync(t *testing.T) {
	pool := setupTestPool(t)
	now := time.Now().Truncate(time.Second)

	const total = epgBatchSize*2 + 3
	programs := make([]mirakc.Program, 0, total)
	for i := range total {
		programs = append(programs, testProgram(
			32736, 1024, i+1, fmt.Sprintf("番組 %d", i),
			now.Add(time.Duration(i)*time.Minute), time.Minute,
		))
	}

	fx := &epgFixture{
		services: []mirakc.Service{testService(32736, 1024, 1, "ＮＨＫ総合", "27")},
		programs: programs,
	}
	srv := newEpgServer(t, fx)
	w := &EpgSyncWorker{MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)), Pool: pool, RetentionGrace: 24 * time.Hour}

	runEpgSync(t, w)

	count, err := sqlcgen.New(pool).CountEpgPrograms(context.Background(), testSite)
	if err != nil {
		t.Fatalf("CountEpgPrograms: %v", err)
	}
	if count != total {
		t.Errorf("count = %d, want %d", count, total)
	}
}

// ListEpgPrograms の時間窓とサービス絞り込み（M1-7 の番組表グリッドが使う軸）。
func TestListEpgPrograms_WindowAndServiceFilter(t *testing.T) {
	pool := setupTestPool(t)
	now := time.Now().Truncate(time.Second)

	// 00:00-01:00, 01:00-02:00 を service 1024 に、00:30-01:30 を service 1032 に置く
	base := now.Truncate(time.Hour)
	fx := &epgFixture{
		services: []mirakc.Service{
			testService(32736, 1024, 1, "サービスA", "27"),
			testService(32737, 1032, 4, "サービスB", "23"),
		},
		programs: []mirakc.Program{
			testProgram(32736, 1024, 1, "A-1", base, time.Hour),
			testProgram(32736, 1024, 2, "A-2", base.Add(time.Hour), time.Hour),
			testProgram(32737, 1032, 1, "B-1", base.Add(30*time.Minute), time.Hour),
		},
	}
	srv := newEpgServer(t, fx)
	w := &EpgSyncWorker{
		MirakcClients:  singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		Pool:           pool,
		RetentionGrace: 365 * 24 * time.Hour,
	}
	runEpgSync(t, w)

	q := sqlcgen.New(pool)
	ctx := context.Background()

	// 窓に一部でも重なる番組が入る（境界は開区間: start < window_end かつ end > window_start）
	rows, err := q.ListEpgPrograms(ctx, sqlcgen.ListEpgProgramsParams{
		Site:        testSite,
		WindowStart: base.Add(45 * time.Minute),
		WindowEnd:   base.Add(75 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ListEpgPrograms: %v", err)
	}
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Name
	}
	slices.Sort(names)
	if !reflect.DeepEqual(names, []string{"A-1", "A-2", "B-1"}) {
		t.Errorf("overlapping window names = %v, want [A-1 A-2 B-1]", names)
	}

	// 窓にちょうど接するだけの番組は入らない（A-1 は end == window_start）
	rows, err = q.ListEpgPrograms(ctx, sqlcgen.ListEpgProgramsParams{
		Site:        testSite,
		WindowStart: base.Add(time.Hour),
		WindowEnd:   base.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("ListEpgPrograms: %v", err)
	}
	for _, r := range rows {
		if r.Name == "A-1" {
			t.Error("A-1 should not overlap [01:00, 02:00)")
		}
	}

	// サービス絞り込み
	svcID := int32(1024)
	rows, err = q.ListEpgPrograms(ctx, sqlcgen.ListEpgProgramsParams{
		Site:        testSite,
		WindowStart: base,
		WindowEnd:   base.Add(3 * time.Hour),
		ServiceID:   &svcID,
	})
	if err != nil {
		t.Fatalf("ListEpgPrograms: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("service-filtered rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.ServiceID != 1024 {
			t.Errorf("service filter leaked service_id = %d", r.ServiceID)
		}
	}
}

// TestEpgSyncWorker_SiteMismatch は、job.Args.Site がワーカー自身の site
// （w.Site）と一致しないジョブが mirakc に一切触れずに fail-fast することを
// 確認する（issue #139）。モックは 200 を返す（弱いテストにしないため。
// issue #139 のテスト規律）。
func TestEpgSyncWorker_SiteMismatch(t *testing.T) {
	pool := setupTestPool(t)

	var requests atomic.Int32
	fx := &epgFixture{
		services: []mirakc.Service{testService(32736, 1024, 1, "ＮＨＫ総合", "27")},
		programs: []mirakc.Program{testProgram(32736, 1024, 1, "番組A", time.Now().Add(time.Hour), time.Hour)},
	}
	countingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/services":
			_ = json.NewEncoder(w).Encode(fx.services)
		case "/api/programs":
			_ = json.NewEncoder(w).Encode(fx.programs)
		default:
			http.NotFound(w, r)
		}
	}))
	defer countingSrv.Close()

	// このワーカープロセスは site-a の mirakc を向いている。
	w := &EpgSyncWorker{MirakcClients: singleSiteClients("site-a", mirakc.NewClient(countingSrv.URL, nil)), Pool: pool}

	job := &river.Job[EpgSyncArgs]{JobRow: &rivertype.JobRow{}, Args: EpgSyncArgs{Site: "site-b"}}
	err := w.Work(context.Background(), job)
	if err == nil {
		t.Fatal("Work() error = nil, want error for site mismatch (site-a worker handling a site-b job)")
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("mirakc received %d requests, want 0 (guard must fail before touching mirakc): err=%v", got, err)
	}
}

// TestEpgSyncWorker_SiteMatch は、args.Site が一致するジョブは従来どおり処理
// されることを確認する（TestEpgSyncWorker_SiteMismatch と対になる両方向の確認）。
func TestEpgSyncWorker_SiteMatch(t *testing.T) {
	pool := setupTestPool(t)

	fx := &epgFixture{
		services: []mirakc.Service{testService(32736, 1024, 1, "ＮＨＫ総合", "27")},
		programs: []mirakc.Program{testProgram(32736, 1024, 1, "番組A", time.Now().Add(time.Hour), time.Hour)},
	}
	srv := newEpgServer(t, fx)

	w := &EpgSyncWorker{MirakcClients: singleSiteClients("site-a", mirakc.NewClient(srv.URL, nil)), Pool: pool}

	job := &river.Job[EpgSyncArgs]{JobRow: &rivertype.JobRow{}, Args: EpgSyncArgs{Site: "site-a"}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error = %v, want nil for matching site", err)
	}

	services, err := sqlcgen.New(pool).ListEpgServices(context.Background(), "site-a")
	if err != nil {
		t.Fatalf("ListEpgServices: %v", err)
	}
	if len(services) != 1 {
		t.Errorf("services projected for site-a = %d, want 1", len(services))
	}
}

func TestGenreLv1(t *testing.T) {
	tests := []struct {
		name   string
		genres []mirakc.Genre
		want   []int16
	}{
		{"nil", nil, []int16{}},
		{"empty", []mirakc.Genre{}, []int16{}},
		{"single", []mirakc.Genre{{LV1: 7}}, []int16{7}},
		{
			"lv1 が重複するジャンルは 1 つに畳む",
			[]mirakc.Genre{{LV1: 7, LV2: 1}, {LV1: 7, LV2: 3}, {LV1: 2}},
			[]int16{7, 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := genreLv1(tt.genres); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("genreLv1() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChunks(t *testing.T) {
	tests := []struct {
		name string
		in   []int
		size int
		want [][]int
	}{
		{"empty", []int{}, 3, nil},
		{"割り切れる", []int{1, 2, 3, 4}, 2, [][]int{{1, 2}, {3, 4}}},
		{"余りあり", []int{1, 2, 3, 4, 5}, 2, [][]int{{1, 2}, {3, 4}, {5}}},
		{"size より短い", []int{1}, 10, [][]int{{1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got [][]int
			for c := range chunks(tt.in, tt.size) {
				got = append(got, c)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("chunks() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChunksEarlyBreak(t *testing.T) {
	var seen int
	for range chunks([]int{1, 2, 3, 4, 5, 6}, 2) {
		seen++
		break
	}
	if seen != 1 {
		t.Errorf("iterations = %d, want 1 (break should stop the loop)", seen)
	}
}

func TestMarshalOrNil(t *testing.T) {
	// 「詳細なし」は型を問わず SQL NULL にする
	nilCases := []struct {
		name string
		in   any
	}{
		{"nil interface", nil},
		{"empty map", map[string]string{}},
		{"nil map", map[string]string(nil)},
		{"empty genres", []mirakc.Genre{}},
		{"nil audios", []mirakc.AudioInfo(nil)},
		{"nil video", (*mirakc.VideoInfo)(nil)},
	}
	for _, tt := range nilCases {
		t.Run(tt.name, func(t *testing.T) {
			if got := marshalOrNil(tt.in); got != nil {
				t.Errorf("marshalOrNil() = %s, want nil", got)
			}
		})
	}

	if got := marshalOrNil(map[string]string{"a": "b"}); string(got) != `{"a":"b"}` {
		t.Errorf("map = %s", got)
	}
	if got := marshalOrNil([]mirakc.Genre{{LV1: 7}}); string(got) == "" || string(got) == "null" {
		t.Errorf("genres = %s, want payload", got)
	}
	if got := marshalOrNil(&mirakc.VideoInfo{StreamContent: 1}); string(got) == "" || string(got) == "null" {
		t.Errorf("video = %s, want payload", got)
	}
}
