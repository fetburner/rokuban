package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// readSSEEvent は次の SSE イベントの event 名を返す。
// data 行・retry 行・コメント（keep-alive）行は読み飛ばす。
func readSSEEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE stream: %v", err)
		}
		if name, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "event: "); ok {
			return name
		}
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

func startHub(t *testing.T, pool *pgxpool.Pool) (*EventHub, *httptest.Server) {
	t.Helper()
	hub := NewEventHub()

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

	srv := httptest.NewServer(NewRouter(nil, nil, pool, hub))
	t.Cleanup(srv.Close)
	return hub, srv
}

// DB の変更がトリガー → NOTIFY → SSE でクライアントに届くこと。
func TestEvents_ReservationTriggersNotify(t *testing.T) {
	pool := testutil.SetupDB(t)
	hub, srv := startHub(t, pool)

	reader := openSSE(t, srv.URL)
	waitForClients(t, hub, 1)

	// 予約を作ると reservations トピックが届く
	_, err := sqlcgen.New(pool).CreateManualReservation(context.Background(), sqlcgen.CreateManualReservationParams{
		Site:              defaultSite,
		ProgramID:         1,
		Overrides:         []byte(`{}`),
		Title:             "テスト番組",
		ProgramStartAt:    time.Now().Add(time.Hour),
		ProgramDurationMs: 1800000,
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
	hub, srv := startHub(t, pool)

	reader := openSSE(t, srv.URL)
	waitForClients(t, hub, 1)

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
	hub, srv := startHub(t, pool)

	reader := openSSE(t, srv.URL)
	waitForClients(t, hub, 1)

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
	hub, srv := startHub(t, pool)

	r1 := openSSE(t, srv.URL)
	r2 := openSSE(t, srv.URL)
	waitForClients(t, hub, 2)

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

// hub が nil なら SSE エンドポイントは登録されない（api ロール以外での構成）。
func TestEvents_DisabledWithoutHub(t *testing.T) {
	srv := httptest.NewServer(NewRouter(nil, nil, nil, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Error("SSE endpoint should not be registered without a hub")
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
