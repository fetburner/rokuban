package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/testutil"
)

// capacityOverageResp は GET /api/capacity/overages のレスポンス要素。
type capacityOverageResp struct {
	Site        string    `json:"site"`
	StartAt     time.Time `json:"startAt"`
	EndAt       time.Time `json:"endAt"`
	Shortfall   int       `json:"shortfall"`
	JammedTypes []string  `json:"jammedTypes"`
}

func insertCapacityTuner(t *testing.T, pool *pgxpool.Pool, index int, types []string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO tuner_sync (site, tuner_index, name, types, is_available, is_fault)
VALUES ('default', $1, $2, $3, true, false)`, index, fmt.Sprintf("tuner-%d", index), types); err != nil {
		t.Fatalf("inserting tuner_sync row: %v", err)
	}
}

// insertCapacityReservation は program_snapshots + reservations 行を作る。
// #27 で番組の事実のスナップショット（title / 開始時刻 / 尺 / チャンネル識別）が
// program_snapshots に抽出され、reservations への FK が張られたため、
// 予約行より先に program_snapshots を作る。
func insertCapacityReservation(t *testing.T, pool *pgxpool.Pool, programID int64, channelType, channel string, startAt time.Time, duration time.Duration) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms,
  network_id, service_id, channel_type, channel
) VALUES ('default', $1, 'テスト番組', $2, $3, 32678, 5168, $4, $5)`,
		programID, startAt, duration.Milliseconds(), channelType, channel); err != nil {
		t.Fatalf("inserting program_snapshot row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO reservations (site, program_id, base) VALUES ('default', $1, '{}')`,
		programID); err != nil {
		t.Fatalf("inserting reservation row: %v", err)
	}
}

func overagesURL(baseURL string, start, end time.Time) string {
	q := url.Values{}
	q.Set("start", start.Format(time.RFC3339Nano))
	q.Set("end", end.Format(time.RFC3339Nano))
	return baseURL + "/api/capacity/overages?" + q.Encode()
}

func getOverages(t *testing.T, baseURL string, start, end time.Time) (int, []capacityOverageResp) {
	t.Helper()
	resp, err := http.Get(overagesURL(baseURL, start, end))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var got []capacityOverageResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return resp.StatusCode, got
}

func newCapacityServer(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{Pool: pool}))
	t.Cleanup(srv.Close)
	return srv
}

// GR 1 本しかない構成で、別チャンネル 2 予約の区間が不足本数と詰まった種別つきで返る。
func TestListCapacityOverages_ReturnsShortfallAndJammedTypes(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newCapacityServer(t, pool)
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)

	insertCapacityTuner(t, pool, 0, []string{"GR"})
	insertCapacityReservation(t, pool, 100, "GR", "27", start, time.Hour)
	insertCapacityReservation(t, pool, 101, "GR", "25", start, time.Hour)

	status, got := getOverages(t, srv.URL, start.Add(-time.Hour), start.Add(2*time.Hour))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(got) != 1 {
		t.Fatalf("overages = %+v, want 1", got)
	}
	o := got[0]
	if o.Site != "default" {
		t.Errorf("site = %q, want default", o.Site)
	}
	if !o.StartAt.Equal(start) || !o.EndAt.Equal(start.Add(time.Hour)) {
		t.Errorf("interval = %v..%v, want %v..%v", o.StartAt, o.EndAt, start, start.Add(time.Hour))
	}
	if o.Shortfall != 1 {
		t.Errorf("shortfall = %d, want 1", o.Shortfall)
	}
	if len(o.JammedTypes) != 1 || o.JammedTypes[0] != "GR" {
		t.Errorf("jammedTypes = %v, want [GR]", o.JammedTypes)
	}
}

// 収まる構成では何も返らない。**沈黙が「収まる」の主張ではない**（返さないことで
// 「収まります」と言わないのが正しい）ので、レスポンスは空配列でしかない。
func TestListCapacityOverages_SaysNothingWhenNotExceeded(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newCapacityServer(t, pool)
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)

	insertCapacityTuner(t, pool, 0, []string{"GR"})
	insertCapacityTuner(t, pool, 1, []string{"GR"})
	insertCapacityReservation(t, pool, 100, "GR", "27", start, time.Hour)
	insertCapacityReservation(t, pool, 101, "GR", "25", start, time.Hour)

	status, got := getOverages(t, srv.URL, start.Add(-time.Hour), start.Add(2*time.Hour))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(got) != 0 {
		t.Errorf("overages = %+v, want none", got)
	}
}

// 窓は範囲検索。地平線全体を 1 回解いた結果を交差で切り出す。
func TestListCapacityOverages_FiltersByWindow(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newCapacityServer(t, pool)
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)

	insertCapacityTuner(t, pool, 0, []string{"GR"})
	// [start, start+1h) と [start+3h, start+4h) の 2 区間を作る。
	insertCapacityReservation(t, pool, 100, "GR", "27", start, time.Hour)
	insertCapacityReservation(t, pool, 101, "GR", "25", start, time.Hour)
	insertCapacityReservation(t, pool, 102, "GR", "24", start.Add(3*time.Hour), time.Hour)
	insertCapacityReservation(t, pool, 103, "GR", "22", start.Add(3*time.Hour), time.Hour)

	tests := []struct {
		name       string
		start, end time.Duration
		want       int
	}{
		{name: "両方を含む窓", start: -time.Hour, end: 5 * time.Hour, want: 2},
		{name: "前半だけを含む窓", start: 0, end: time.Hour, want: 1},
		{name: "後半だけを含む窓", start: 3 * time.Hour, end: 4 * time.Hour, want: 1},
		{name: "どちらにも掛からない窓（半開なので接するだけは含まない）", start: time.Hour, end: 3 * time.Hour, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, got := getOverages(t, srv.URL, start.Add(tc.start), start.Add(tc.end))
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if len(got) != tc.want {
				t.Errorf("overages = %+v, want %d", got, tc.want)
			}
		})
	}
}

// 不正な窓は 400。
func TestListCapacityOverages_RejectsInvalidWindow(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newCapacityServer(t, pool)
	now := time.Now().UTC()

	for _, tc := range []struct {
		name       string
		start, end time.Time
	}{
		{name: "end が start より前", start: now, end: now.Add(-time.Hour)},
		{name: "end が start と同じ", start: now, end: now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status, _ := getOverages(t, srv.URL, tc.start, tc.end); status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", status)
			}
		})
	}

	// 反対方向: 妥当な窓は 200。
	if status, _ := getOverages(t, srv.URL, now, now.Add(time.Hour)); status != http.StatusOK {
		t.Errorf("status = %d, want 200 for a valid window", status)
	}
}

// 種別部分集合への縮約が API 越しでも効いていること。GR 専用 1 本 +
// GR/BS 両対応 1 本に対する「GR 1 + BS 2」は総本数（2）が足りているのに
// {BS}: 2 ≤ 1 が破れる。素朴な「重なり数 vs 総本数」では検出できないケース。
func TestListCapacityOverages_DetectsJamThatTotalCountMisses(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newCapacityServer(t, pool)
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)

	insertCapacityTuner(t, pool, 0, []string{"GR"})
	insertCapacityTuner(t, pool, 1, []string{"GR", "BS"})
	insertCapacityReservation(t, pool, 100, "GR", "27", start, time.Hour)
	insertCapacityReservation(t, pool, 101, "BS", "BS15_0", start, time.Hour)
	insertCapacityReservation(t, pool, 102, "BS", "BS03_1", start, time.Hour)

	status, got := getOverages(t, srv.URL, start.Add(-time.Hour), start.Add(2*time.Hour))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(got) != 1 {
		t.Fatalf("overages = %+v, want 1 (BS が 1 本不足)", got)
	}
	if got[0].Shortfall != 1 {
		t.Errorf("shortfall = %d, want 1", got[0].Shortfall)
	}
	if len(got[0].JammedTypes) != 1 || got[0].JammedTypes[0] != "BS" {
		t.Errorf("jammedTypes = %v, want [BS]", got[0].JammedTypes)
	}

	// 反対方向: BS を 1 つ落とせば収まる（GR 1 + BS 1 は T1→GR, T2→BS で入る）。
	if _, err := pool.Exec(context.Background(), `DELETE FROM reservations WHERE program_id = 102`); err != nil {
		t.Fatalf("deleting reservation: %v", err)
	}
	if _, got := getOverages(t, srv.URL, start.Add(-time.Hour), start.Add(2*time.Hour)); len(got) != 0 {
		t.Errorf("overages = %+v, want none", got)
	}
}

// 同一物理チャンネルの複数予約は 1 本のチューナーに相乗りできるので需要 1。
func TestListCapacityOverages_SamePhysicalChannelSharesOneTuner(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newCapacityServer(t, pool)
	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)

	insertCapacityTuner(t, pool, 0, []string{"GR"})
	insertCapacityReservation(t, pool, 100, "GR", "27", start, time.Hour)
	insertCapacityReservation(t, pool, 101, "GR", "27", start.Add(10*time.Minute), 30*time.Minute)
	insertCapacityReservation(t, pool, 102, "GR", "27", start.Add(20*time.Minute), 30*time.Minute)

	if _, got := getOverages(t, srv.URL, start.Add(-time.Hour), start.Add(2*time.Hour)); len(got) != 0 {
		t.Errorf("overages = %+v, want none (同一物理チャンネルは需要 1)", got)
	}

	// 反対方向: 1 件を別チャンネルにすれば超過する。channel は #27 で
	// program_snapshots に抽出された。
	if _, err := pool.Exec(context.Background(),
		`UPDATE program_snapshots SET channel = '25' WHERE program_id = 102`); err != nil {
		t.Fatalf("updating reservation channel: %v", err)
	}
	if _, got := getOverages(t, srv.URL, start.Add(-time.Hour), start.Add(2*time.Hour)); len(got) != 1 {
		t.Errorf("overages = %+v, want 1 after moving one reservation to another channel", got)
	}
}
