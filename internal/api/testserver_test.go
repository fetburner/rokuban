package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
)

// NewTestServer は API サーバーを起動し、t.Cleanup で閉じる。pool が nil なら
// 既定は空の RouterConfig（DB を使わないテスト用）、非 nil なら pool から組んだ
// 全ストアの構成になる。テストごとの差は overrides で渡す
// （例: func(c *RouterConfig) { c.Mounter = m }）。api_test パッケージからも
// 使うので、名前を大文字で出している。
func NewTestServer(t *testing.T, pool *pgxpool.Pool, overrides ...func(*RouterConfig)) *httptest.Server {
	t.Helper()
	var cfg RouterConfig
	if pool != nil {
		cfg = defaultTestRouterConfig(pool)
	}
	for _, override := range overrides {
		override(&cfg)
	}
	srv := httptest.NewServer(NewRouter(cfg))
	t.Cleanup(srv.Close)
	return srv
}

// defaultTestRouterConfig は pool の全ストアを載せた既定の RouterConfig。
func defaultTestRouterConfig(pool *pgxpool.Pool) RouterConfig {
	tunerStore := pgstoretest.Tuners(pool)
	media := pgstoretest.Media(pool)
	recordingStore := pgstoretest.Recordings(pool)
	return RouterConfig{
		Ready:               pool.Ping,
		EPGReader:           pgstoretest.EPG(pool),
		ProgramSearch:       pgstoretest.EPG(pool),
		MediaReader:         media,
		StorageObservation:  media,
		TunerReader:         tunerStore,
		CapacityStore:       tunerStore,
		PlaybackStateStore:  pgstoretest.Playback(pool),
		RecordingTrashStore: recordingStore,
		RecordingQuery:      recordingStore,
		ReservationStore:    pgstoretest.Reservations(pool),
		LabelRules:          pgstoretest.LabelRules(pool),
		Dedupe:              pgstoretest.Reservations(pool),
	}
}

// GetJSON は url を GET し、状態コードが 200 でなければ本文を含めて失敗し、
// 200 なら応答を T にデコードして返す。非 200 を期待するテストは GetStatus を使う。
func GetJSON[T any](t *testing.T, url string) T {
	t.Helper()
	var out T
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status = %d, want 200 (body=%s)", url, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding GET %s: %v", url, err)
	}
	return out
}

// GetStatus は url を GET して状態コードを返す。応答本文は読み捨てる（非 200 の検査用）。
func GetStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}
