package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestEPGEndpoint_UnregisteredDefaultSiteStill404 keeps site validation scoped to the
// configured mirakc registry when the store breaker route is added.
func TestEPGEndpoint_UnregisteredDefaultSiteStill404(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := api.NewRouter(api.RouterConfig{Ready: pool.Ping, Sites: []string{"tokyo", "takamatsu"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/sites/default/programs?start=2020-01-01T00:00:00Z&end=2020-01-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET .../sites/default/programs status = %d, want 404", resp.StatusCode)
	}
}
