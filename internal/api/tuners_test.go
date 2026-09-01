package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// seedTunerSync は tuner_sync に 1 行 upsert する。
func seedTunerSync(t *testing.T, pool *pgxpool.Pool, site string, index int32, name string, types []string, isAvailable, isFault bool) {
	t.Helper()
	err := sqlcgen.New(pool).UpsertTunerSync(context.Background(), []sqlcgen.UpsertTunerSyncParams{{
		Site:        site,
		TunerIndex:  index,
		Name:        name,
		Types:       types,
		IsAvailable: isAvailable,
		IsFault:     isFault,
	}}).Close()
	if err != nil {
		t.Fatalf("seeding tuner_sync: %v", err)
	}
}

func TestListTuners(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()

	// tuner_index 降順に投入し、応答が tuner_index 昇順であることを確かめる。
	seedTunerSync(t, pool, "default", 1, "chardev1", []string{"GR"}, true, false)
	seedTunerSync(t, pool, "default", 0, "chardev0", []string{"GR", "BS"}, true, true)
	// 設定で無効化された行（is_available=false）も同時に見る --- IsAvailable が
	// 常に true を返す変異（`IsAvailable: r.IsAvailable` を `true` に変える等）を
	// 拾うには、少なくとも 1 行は false でなければならない。
	seedTunerSync(t, pool, "default", 2, "chardev2", []string{"GR"}, false, false)

	// UpsertTunerSync は observed_at を DB 側の now() で書くため、変えずに投入
	// しただけでは「常に取れたての顔をする」変異（`ObservedAt: r.ObservedAt` を
	// `time.Now()` に変える）を検出できない（レビュー指摘: 実測でこの変異でも
	// 既存の 3 テストが全部通っていた）。SQL で直接過去の時刻に書き換え、
	// 返ってきた値がその時刻と一致することをリテラルの許容差で確認する。
	wantObservedAt := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if _, err := pool.Exec(ctx,
		`UPDATE tuner_sync SET observed_at = $1 WHERE site = $2 AND tuner_index = $3`,
		wantObservedAt, "default", int32(0)); err != nil {
		t.Fatalf("backdating observed_at: %v", err)
	}

	var got []Tuner
	resp := getJSON(t, srv.URL+"/api/sites/default/tuners", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 3 {
		t.Fatalf("tuners = %d, want 3", len(got))
	}
	if got[0].Index != 0 || got[0].Name != "chardev0" {
		t.Errorf("tuner[0] = %+v", got[0])
	}
	if got[1].Index != 1 || got[1].Name != "chardev1" {
		t.Errorf("tuner[1] = %+v", got[1])
	}
	if got[2].Index != 2 || got[2].Name != "chardev2" {
		t.Errorf("tuner[2] = %+v", got[2])
	}

	// SQL 側で is_available / is_fault を絞らない
	// （internal/db/queries/tuner_sync.sql の ListTunerSync のコメントと同じ理由）。
	// 「存在するが数えない」（isFault=true）を UI 側にもそのまま渡す必要がある ---
	// ここでフィルタしてしまうと故障チューナーが応答から消え、UI がそもそも
	// 気づけなくなる。
	if !got[0].IsFault {
		t.Errorf("tuner[0].IsFault = false, want true (fault rows must not be filtered out)")
	}
	if !got[0].IsAvailable {
		t.Errorf("tuner[0].IsAvailable = false, want true")
	}
	if got[2].IsAvailable {
		t.Errorf("tuner[2].IsAvailable = true, want false (disabled rows must not be filtered out either)")
	}
	if len(got[0].Types) != 2 || got[0].Types[0] != "GR" || got[0].Types[1] != "BS" {
		t.Errorf("tuner[0].Types = %+v, want [GR BS]", got[0].Types)
	}

	const tolerance = time.Second
	if diff := got[0].ObservedAt.Sub(wantObservedAt); diff < -tolerance || diff > tolerance {
		t.Errorf("tuner[0].ObservedAt = %v, want %v (within %v)", got[0].ObservedAt, wantObservedAt, tolerance)
	}
	// tuner[1] は今回バックデートしていないので、素直に「最近」であることだけ見る
	// （バックデートした行と取り違えて常に一致する変異を許さないための対比）。
	if got[1].ObservedAt.Before(wantObservedAt.Add(time.Hour)) {
		t.Errorf("tuner[1].ObservedAt = %v, want a recent timestamp (not the backdated one)", got[1].ObservedAt)
	}
}

// TestListTuners_EmptyProjection は射影が 1 行も無いサイトでも 404 にせず空配列を
// 返すことを確かめる（docs/data/capacity.md §6.5「射影が 1 行も無いサイトは
// 何も主張しない」。空配列そのものがその主張）。
func TestListTuners_EmptyProjection(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	var got []Tuner
	resp := getJSON(t, srv.URL+"/api/sites/default/tuners", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 0 {
		t.Fatalf("tuners = %d, want 0", len(got))
	}
}

func TestListTuners_UnknownSite_Returns404(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	resp := getJSON(t, srv.URL+"/api/sites/nonexistent/tuners", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
