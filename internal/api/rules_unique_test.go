package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRulesMetadataDoesNotIdentifyImport(t *testing.T) {
	pool := testutil.SetupDB(t)
	rules := pgstoretest.Rules(pool, "default")
	router := NewRouter(RouterConfig{Ready: pool.Ping, RuleStore: rules, ProgramSearch: pgstoretest.EPG(pool)})
	send := func(method, path string, want int) Rule {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(`{"name":"user","metadata":{"epgstation":{"ruleId":42}}}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s status = %d, want %d: %s", method, w.Code, want, w.Body.String())
		}
		var row Rule
		if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	first := send(http.MethodPost, "/api/rules", 201)
	second := send(http.MethodPost, "/api/rules", 201)
	send(http.MethodPatch, "/api/rules/"+itoa(second.Id), 200)
	created, err := rules.ImportEPGStationRule(t.Context(), 42, store.RuleInput{Name: "imported"})
	if err != nil || !created {
		t.Fatalf("import = %t, %v, want new rule", created, err)
	}
	rows, err := rules.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rules = %d, want 3", len(rows))
	}
	var ruleID int64
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*), min(rule_id) FROM rule_import_keys").Scan(&count, &ruleID); err != nil {
		t.Fatal(err)
	}
	if count != 1 || ruleID == first.Id || ruleID == second.Id {
		t.Fatalf("keys = %d, owner=%d; want imported rule only", count, ruleID)
	}
	imported, err := rules.Get(t.Context(), ruleID)
	if err != nil || imported.Name != "imported" {
		t.Fatalf("key owner = %+v, %v", imported, err)
	}
	for _, id := range []int64{first.Id, second.Id} {
		row, err := rules.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if row.Metadata["epgstation"] == nil {
			t.Fatalf("user metadata lost: %+v", row)
		}
	}
}
