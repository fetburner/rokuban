package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRulesEPGStationKeyConflict(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := NewRouter(RouterConfig{Ready: pool.Ping, RuleStore: pgstoretest.Rules(pool, "default"), ProgramSearch: pgstoretest.EPG(pool)})
	send := func(method, path, body string, want int) Rule {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s: status %d, want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		var row Rule
		if want == 201 || want == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil {
				t.Fatal(err)
			}
		}
		return row
	}
	body := `{"name":"first","metadata":{"epgstation":{"ruleId":42}}}`
	first := send(http.MethodPost, "/api/rules", body, 201)
	send(http.MethodPost, "/api/rules", body, 409)
	second := send(http.MethodPost, "/api/rules", `{"name":"second"}`, 201)
	send(http.MethodPatch, "/api/rules/"+itoa(second.Id), body, 409)
	send(http.MethodPatch, "/api/rules/"+itoa(first.Id), body, 200)
	send(http.MethodPost, "/api/rules", `{"name":"other","metadata":{"epgstation":{"ruleId":43}}}`, 201)
	for range 2 {
		send(http.MethodPost, "/api/rules", `{"name":"missing"}`, 201)
		send(http.MethodPost, "/api/rules", `{"name":"null","metadata":{"epgstation":{"ruleId":null}}}`, 201)
	}
	send(http.MethodPost, "/api/rules", `{"name":"string","metadata":{"epgstation":{"ruleId":"42"}}}`, 409)
}
