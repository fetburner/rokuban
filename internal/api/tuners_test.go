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

	// tuner_index 降順に投入し、応答が tuner_index 昇順であることを確かめる。
	seedTunerSync(t, pool, "default", 1, "chardev1", []string{"GR"}, true, false)
	seedTunerSync(t, pool, "default", 0, "chardev0", []string{"GR", "BS"}, true, true)

	var got []Tuner
	resp := getJSON(t, srv.URL+"/api/sites/default/tuners", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 2 {
		t.Fatalf("tuners = %d, want 2", len(got))
	}
	if got[0].Index != 0 || got[0].Name != "chardev0" {
		t.Errorf("tuner[0] = %+v", got[0])
	}
	if got[1].Index != 1 || got[1].Name != "chardev1" {
		t.Errorf("tuner[1] = %+v", got[1])
	}

	// SQL 側で is_available / is_fault を絞らない
	// （internal/db/queries/tuner_sync.sql の ListTunerSync のコメントと同じ理由）。
	// 「存在するが数えない」（isFault=true）を UI 側にもそのまま渡す必要がある ---
	// ここでフィルタしてしまうと故障チューナーが応答から消え、UI がそもそも
	// 気づけなくなる。
	if !got[0].IsFault {
		t.Errorf("tuner[0].IsFault = false, want true (fault rows must not be filtered out)")
	}
	if len(got[0].Types) != 2 || got[0].Types[0] != "GR" || got[0].Types[1] != "BS" {
		t.Errorf("tuner[0].Types = %+v, want [GR BS]", got[0].Types)
	}
	if got[0].ObservedAt.After(time.Now()) || got[0].ObservedAt.IsZero() {
		t.Errorf("tuner[0].ObservedAt = %v, want a recent non-zero timestamp", got[0].ObservedAt)
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
