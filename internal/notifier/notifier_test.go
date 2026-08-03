package notifier

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// defaultSite は単一サイト構成でのサイト名。定義は db.DefaultSite（唯一の出所）。
const defaultSite = db.DefaultSite

// sseTimeout は SSE イベントを待つ上限。
// 期限なしで読むと、通知を取りこぼしたときにパッケージ全体のタイムアウト
// （既定 10 分）までハングし、CI が「どのテストが原因か分からない失敗」になる。
const sseTimeout = 15 * time.Second

// readSSEEvent は次の SSE イベントの event 名を、期限付きで返す。
func readSSEEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	type result struct {
		name string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				ch <- result{err: err}
				return
			}
			if name, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "event: "); ok {
				ch <- result{name: name}
				return
			}
		}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("reading SSE stream: %v", got.err)
		}
		return got.name
	case <-time.After(sseTimeout):
		t.Fatalf("timed out after %s waiting for an SSE event", sseTimeout)
		return ""
	}
}

// openSSE は /api/events に接続し、retry 行を読み終えた状態の Reader を返す。
func openSSE(t *testing.T, url string) *bufio.Reader {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url+"/api/events", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	// nginx のバッファリング抑止（docs/api.md のリバースプロキシ要件）
	if ab := resp.Header.Get("X-Accel-Buffering"); ab != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", ab)
	}
	return bufio.NewReader(resp.Body)
}

// waitForClients は hub にクライアントが n 個繋がるまで待つ。
// waitListening は hub の LISTEN 確立を待つ。
//
// waitForClients だけでは足りない。あれは SSE クライアントが hub に登録された
// ことしか保証せず、hub の Postgres LISTEN は別 goroutine で非同期に張られる。
// LISTEN 前に発行された NOTIFY はどこにも届かないので、通知を期待する前に
// 確立を待つ必要がある。
func waitListening(t *testing.T, hub *EventHub) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), sseTimeout)
	defer cancel()
	if err := hub.waitListening(ctx); err != nil {
		t.Fatalf("waiting for hub to LISTEN: %v", err)
	}
}

func waitForClients(t *testing.T, hub *EventHub, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hub.clientCount() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("clients = %d, want %d", hub.clientCount(), n)
}

// runHub は hub の LISTEN ループを t の生存期間だけ回す。
func runHub(t *testing.T, hub *EventHub, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = hub.Run(ctx, pool)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// startNotifier は notifier 単体（EventHub だけを Mount したルーター）を起動する。
// notifier ロールが単独プロセスで動くケースに対応するテスト用ヘルパー。
func startNotifier(t *testing.T, pool *pgxpool.Pool) (*EventHub, *httptest.Server) {
	t.Helper()
	hub := NewEventHub()
	runHub(t, hub, pool)

	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{Mounter: hub}))
	t.Cleanup(srv.Close)
	return hub, srv
}

// seedRecording は notifier のテストに必要な最小限の recordings 行を作る。
func seedRecording(t *testing.T, pool *pgxpool.Pool, title string, start time.Time, status string, eventID int32) int64 {
	t.Helper()
	id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              defaultSite,
		NetworkID:         32678,
		ServiceID:         5168,
		EventID:           eventID,
		ServiceName:       "ＯＨＫ",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             title,
		ProgramStartAt:    start,
		ProgramDurationMs: (30 * time.Minute).Milliseconds(),
		Status:            status,
	})
	if err != nil {
		t.Fatalf("seeding recording: %v", err)
	}
	return id
}

// seedIngested は録画に原本 media_asset を付ける（recordings トピックの発火源）。
func seedIngested(t *testing.T, pool *pgxpool.Pool, recordingID, size int64, stats map[int32][4]int64) {
	t.Helper()
	ctx := context.Background()
	q := sqlcgen.New(pool)
	assetID, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "test/asset.m2ts",
		SizeBytes:   size,
	})
	if err != nil {
		t.Fatalf("seeding media_asset: %v", err)
	}
	for pid, s := range stats {
		if err := q.InsertDropStat(ctx, sqlcgen.InsertDropStatParams{
			MediaAssetID: assetID,
			Pid:          pid,
			Packets:      s[0],
			Drops:        s[1],
			Errors:       s[2],
			Scrambled:    s[3],
		}); err != nil {
			t.Fatalf("seeding drop_stat: %v", err)
		}
	}
}

// DB の変更がトリガー → NOTIFY → SSE でクライアントに届くこと。
func TestEvents_ReservationTriggersNotify(t *testing.T) {
	pool := testutil.SetupDB(t)
	hub, srv := startNotifier(t, pool)

	reader := openSSE(t, srv.URL)
	waitForClients(t, hub, 1)
	waitListening(t, hub)

	// 予約を作ると reservations トピックが届く。#27 で番組の事実のスナップショットが
	// program_snapshots に抽出され、reservations への FK が張られたため、
	// 予約行より先に program_snapshots を作る。
	q := sqlcgen.New(pool)
	ctx := context.Background()
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site:        defaultSite,
		ProgramID:   1,
		Title:       "テスト番組",
		StartAt:     time.Now().Add(time.Hour),
		DurationMs:  1800000,
		NetworkID:   32736,
		ServiceID:   1024,
		ChannelType: "GR",
		Channel:     "27",
		EventID:     1,
		ServiceName: "テスト局",
	}); err != nil {
		t.Fatalf("upserting program snapshot: %v", err)
	}
	_, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:      defaultSite,
		ProgramID: 1,
	})
	if err != nil {
		t.Fatalf("creating reservation: %v", err)
	}

	if got := readSSEEvent(t, reader); got != "reservations" {
		t.Errorf("event = %q, want reservations", got)
	}
}

// 録画と media_assets の変更はどちらも recordings トピックになること。
func TestEvents_RecordingAndMediaAssetTopics(t *testing.T) {
	pool := testutil.SetupDB(t)
	hub, srv := startNotifier(t, pool)

	reader := openSSE(t, srv.URL)
	waitForClients(t, hub, 1)
	waitListening(t, hub)

	recordingID := seedRecording(t, pool, "録画", time.Now().Truncate(time.Second), "recording", 1)
	if got := readSSEEvent(t, reader); got != "recordings" {
		t.Fatalf("event = %q, want recordings", got)
	}

	// media_assets の INSERT も recordings トピックに寄せている
	// （録画一覧のサイズ・ドロップ統計に現れるため）
	seedIngested(t, pool, recordingID, 1000, map[int32][4]int64{0x100: {100, 0, 0, 0}})
	if got := readSSEEvent(t, reader); got != "recordings" {
		t.Errorf("event = %q, want recordings", got)
	}
}

// 明示的な pg_notify（EPG 同期ジョブが使う経路）も届くこと。
func TestEvents_ExplicitNotify(t *testing.T) {
	pool := testutil.SetupDB(t)
	hub, srv := startNotifier(t, pool)

	reader := openSSE(t, srv.URL)
	waitForClients(t, hub, 1)
	waitListening(t, hub)

	if err := sqlcgen.New(pool).NotifyTopic(context.Background(), "epg"); err != nil {
		t.Fatalf("NotifyTopic: %v", err)
	}
	if got := readSSEEvent(t, reader); got != "epg" {
		t.Errorf("event = %q, want epg", got)
	}
}

// 複数クライアントに同じトピックが配られること。
func TestEvents_FanOut(t *testing.T) {
	pool := testutil.SetupDB(t)
	hub, srv := startNotifier(t, pool)

	r1 := openSSE(t, srv.URL)
	r2 := openSSE(t, srv.URL)
	waitForClients(t, hub, 2)
	waitListening(t, hub)

	if err := sqlcgen.New(pool).NotifyTopic(context.Background(), "epg"); err != nil {
		t.Fatalf("NotifyTopic: %v", err)
	}
	if got := readSSEEvent(t, r1); got != "epg" {
		t.Errorf("client 1 event = %q, want epg", got)
	}
	if got := readSSEEvent(t, r2); got != "epg" {
		t.Errorf("client 2 event = %q, want epg", got)
	}
}

// notifier は複数レプリカで動いてもよい（シングルトンではない）。
//
// 2 つの独立した EventHub（別々の LISTEN コネクション、別々の SSE クライアント）
// が同じ NOTIFY を取りこぼさず、それぞれ自分の購読者に配ること。
// docs/data.md §3 の「各レプリカが自分で LISTEN するだけで Redis アダプタ等の
// 追加基盤は要らない」を確認する。
func TestNotifier_MultipleReplicasNotSingleton(t *testing.T) {
	pool := testutil.SetupDB(t)

	hub1, srv1 := startNotifier(t, pool)
	hub2, srv2 := startNotifier(t, pool)

	r1 := openSSE(t, srv1.URL)
	r2 := openSSE(t, srv2.URL)
	waitForClients(t, hub1, 1)
	waitForClients(t, hub2, 1)
	waitListening(t, hub1)
	waitListening(t, hub2)

	if err := sqlcgen.New(pool).NotifyTopic(context.Background(), "epg"); err != nil {
		t.Fatalf("NotifyTopic: %v", err)
	}
	if got := readSSEEvent(t, r1); got != "epg" {
		t.Errorf("replica 1 event = %q, want epg", got)
	}
	if got := readSSEEvent(t, r2); got != "epg" {
		t.Errorf("replica 2 event = %q, want epg", got)
	}
}

// 同一トピックの連続通知が合流されること（トリガーは行単位で発火するため）。
func TestEventHub_Coalesce(t *testing.T) {
	hub := NewEventHub()
	sub, unsubscribe := hub.Subscribe()
	defer unsubscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	topics := make(chan string)
	waitErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = hub.coalesce(ctx, topics, waitErr)
	}()

	// 同じトピックを 5 回、別のトピックを 1 回、窓の中で送る
	for range 5 {
		topics <- "recordings"
	}
	topics <- "reservations"

	got := map[string]int{}
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case topic := <-sub:
			got[topic]++
		case <-deadline:
			t.Fatalf("timed out; got %v", got)
		}
	}

	// 窓が閉じた後に余分な通知が来ないこと
	select {
	case topic := <-sub:
		t.Errorf("unexpected extra notification for %q (got %v)", topic, got)
	case <-time.After(2 * coalesceWindow):
	}

	if got["recordings"] != 1 || got["reservations"] != 1 {
		t.Errorf("coalesced counts = %v, want each exactly 1", got)
	}

	cancel()
	<-done
}

// バッファが埋まったクライアントがいても Publish が詰まらないこと。
func TestEventHub_PublishDoesNotBlockOnSlowClient(t *testing.T) {
	hub := NewEventHub()
	_, unsubscribe := hub.Subscribe()
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// clientBuffer を超えて送る。読み手がいないので捨てられるだけで、
		// ブロックしてはいけない。
		for range clientBuffer * 3 {
			hub.Publish("recordings")
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Publish blocked on a client whose buffer is full")
	}
}

// 購読解除後にチャネルが閉じられ、二重解除でも panic しないこと。
func TestEventHub_Unsubscribe(t *testing.T) {
	hub := NewEventHub()
	sub, unsubscribe := hub.Subscribe()

	if hub.clientCount() != 1 {
		t.Fatalf("clients = %d, want 1", hub.clientCount())
	}

	unsubscribe()
	if hub.clientCount() != 0 {
		t.Errorf("clients = %d, want 0", hub.clientCount())
	}
	if _, open := <-sub; open {
		t.Error("channel should be closed after unsubscribe")
	}

	unsubscribe() // 二重解除
	hub.Publish("recordings")
}
