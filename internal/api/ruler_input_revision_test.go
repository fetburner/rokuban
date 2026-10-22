package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestAPIInputWritesBumpRulerInputRevisionWithoutEnqueuer(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	router := api.NewRouter(api.RouterConfig{
		Ready:            pool.Ping,
		Sites:            []string{db.DefaultSite},
		RuleStore:        pgstore.NewRules(pool, []string{db.DefaultSite}),
		ReservationStore: pgstore.NewReservations(pool),
		ProgramSearch:    pgstore.NewEPG(pool),
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	wantRevision := int64(0)
	assertRevision := func() {
		t.Helper()
		wantRevision++
		var got int64
		if err := pool.QueryRow(ctx, `
SELECT COALESCE((SELECT revision FROM ruler_input_revisions WHERE site = $1), 0)::bigint`, db.DefaultSite).Scan(&got); err != nil {
			t.Fatalf("reading ruler input revision: %v", err)
		}
		if got != wantRevision {
			t.Fatalf("ruler input revision = %d, want %d", got, wantRevision)
		}
	}

	body := doRevisionAPIRequest(t, srv, http.MethodPost, "/api/rules", `{"name":"rule","sites":["default"]}`, http.StatusCreated)
	var created api.Rule
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decoding created rule: %v", err)
	}
	assertRevision()

	rulePath := "/api/rules/" + itoa(created.Id)
	doRevisionAPIRequest(t, srv, http.MethodPatch, rulePath, `{"name":"updated rule","sites":["default"]}`, http.StatusOK)
	assertRevision()
	doRevisionAPIRequest(t, srv, http.MethodDelete, rulePath, "", http.StatusOK)
	assertRevision()

	const programID int64 = 327360102415397
	insertProgramFixture(t, pool, ctx, programID, 32736, 1024)
	programPath := "/api/sites/default/programs/" + itoa(programID)
	doRevisionAPIRequest(t, srv, http.MethodPut, programPath+"/intent", `{"action":"record"}`, http.StatusNoContent)
	assertRevision()
	doRevisionAPIRequest(t, srv, http.MethodDelete, programPath+"/intent", "", http.StatusNoContent)
	assertRevision()
	doRevisionAPIRequest(t, srv, http.MethodPatch, programPath+"/overrides", `{"priority":7}`, http.StatusNoContent)
	assertRevision()
	doRevisionAPIRequest(t, srv, http.MethodDelete, programPath+"/overrides", "", http.StatusNoContent)
	assertRevision()
}

func doRevisionAPIRequest(t *testing.T, srv *httptest.Server, method, path, body string, wantStatus int) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var responseBody []byte
	if resp.Body != nil {
		responseBody, err = io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading %s %s response: %v", method, path, err)
		}
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s status = %d, want %d: %s", method, path, resp.StatusCode, wantStatus, responseBody)
	}
	return responseBody
}
