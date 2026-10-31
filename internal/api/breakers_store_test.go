package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestListCircuitBreakers_ReturnsStoreScopes fixes the public identity of breaker rows:
// rows in two stores remain distinguishable even when their name is the same.
func TestListCircuitBreakers_ReturnsStoreScopes(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, BreakerAdmin: pgstoretest.Breakers(pool), Sites: []string{"tokyo"},
		StorageStoreNames: []string{"test-store", "s3main"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	insertCircuitBreakerFixtureForSite(t, pool, ctx, "test-store", "delete_reconcile", 10, 20, `{"total":10}`)
	insertCircuitBreakerFixtureForSite(t, pool, ctx, "s3main", "delete_reconcile", 11, 20, `{"total":11}`)
	insertCircuitBreakerFixtureForSite(t, pool, ctx, "tokyo", "ruler_deletes", 12, 30, `{"total":12}`)

	resp, err := http.Get(srv.URL + "/api/breakers")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/breakers status = %d, want 200", resp.StatusCode)
	}

	var got []circuitBreakerResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3: %+v", len(got), got)
	}
	byStore := make(map[string]circuitBreakerResp, 2)
	var siteBreaker *circuitBreakerResp
	for i := range got {
		if got[i].Name == "delete_reconcile" {
			byStore[got[i].Store] = got[i]
		} else if got[i].Name == "ruler_deletes" {
			siteBreaker = &got[i]
		}
	}
	for _, name := range []string{"test-store", "s3main"} {
		b, ok := byStore[name]
		if !ok {
			t.Errorf("no delete_reconcile row with store %q in %+v", name, got)
			continue
		}
		if b.Site != "" {
			t.Errorf("store-scoped row site = %q, want empty", b.Site)
		}
	}
	if siteBreaker == nil {
		t.Fatal("site-scoped ruler_deletes row is missing")
	}
	if siteBreaker.Site != "tokyo" || siteBreaker.Store != "" {
		t.Errorf("site-scoped row site/store = %q/%q, want tokyo/empty", siteBreaker.Site, siteBreaker.Store)
	}
}

// TestResumeStoreCircuitBreaker_OnlyDeletesRequestedStore proves the path's store
// reaches the DELETE key. Replacing req.Store with the configured FS name makes this test fail.
func TestResumeStoreCircuitBreaker_OnlyDeletesRequestedStore(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, BreakerAdmin: pgstoretest.Breakers(pool),
		StorageStoreNames: []string{"test-store", "s3main"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	for _, scope := range []string{"test-store", "s3main"} {
		insertCircuitBreakerFixtureForSite(t, pool, ctx, scope, "delete_reconcile", 10, 20, `{"total":10}`)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/stores/s3main/breakers/delete_reconcile/resume", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("resume status = %d, want 204", resp.StatusCode)
	}
	if !existsCircuitBreakerForSite(t, pool, ctx, "test-store", "delete_reconcile") {
		t.Error("unrequested FS store row was deleted")
	}
	if existsCircuitBreakerForSite(t, pool, ctx, "s3main", "delete_reconcile") {
		t.Error("requested S3 store row still exists after resume")
	}
	if n := countCircuitBreakers(t, pool, ctx); n != 1 {
		t.Errorf("remaining circuit breaker rows = %d, want 1", n)
	}
}

func TestResumeStoreCircuitBreaker_400WhenStoreIsNotConfigured(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, BreakerAdmin: pgstoretest.Breakers(pool),
		StorageStoreNames: []string{"test-store", "s3main"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/stores/unknown/breakers/delete_reconcile/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("resume for an unconfigured store status = %d, want 400", resp.StatusCode)
	}
}

func TestResumeStoreCircuitBreaker_400WhenNameIsNotStoreScoped(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, BreakerAdmin: pgstoretest.Breakers(pool),
		StorageStoreNames: []string{"test-store"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()
	insertCircuitBreakerFixtureForSite(t, pool, ctx, "default", "ruler_deletes", 10, 20, `{"total":10}`)

	resp, err := http.Post(srv.URL+"/api/stores/test-store/breakers/ruler_deletes/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("resume for a site-scoped breaker status = %d, want 400", resp.StatusCode)
	}
	if !existsCircuitBreakerForSite(t, pool, ctx, "default", "ruler_deletes") {
		t.Error("site-scoped breaker row was deleted through the store route")
	}
}

func TestResumeCircuitBreaker_400WhenNameIsStoreScoped(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, BreakerAdmin: pgstoretest.Breakers(pool),
		StorageStoreNames: []string{"test-store"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()
	insertCircuitBreakerFixtureForSite(t, pool, ctx, "test-store", "delete_reconcile", 10, 20, `{"total":10}`)

	resp, err := http.Post(srv.URL+"/api/sites/default/breakers/delete_reconcile/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("resume of a store-scoped breaker through the site route status = %d, want 400", resp.StatusCode)
	}
	if !existsCircuitBreakerForSite(t, pool, ctx, "test-store", "delete_reconcile") {
		t.Error("store-scoped breaker row was deleted through the site route")
	}
}

func TestResumeStoreCircuitBreaker_400WhenNameIsUnknown(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, BreakerAdmin: pgstoretest.Breakers(pool),
		StorageStoreNames: []string{"test-store"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/stores/test-store/breakers/unknown/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("resume for an unknown breaker status = %d, want 400", resp.StatusCode)
	}
}

func TestResumeStoreCircuitBreaker_404WhenNotTripped(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, BreakerAdmin: pgstoretest.Breakers(pool),
		StorageStoreNames: []string{"test-store"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/stores/test-store/breakers/delete_reconcile/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("resume for a non-tripped breaker status = %d, want 404", resp.StatusCode)
	}
}

func TestResumeLegacySitelessCircuitBreakerRouteIsRemoved(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, BreakerAdmin: pgstoretest.Breakers(pool),
		StorageStoreNames: []string{"test-store"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/breakers/delete_reconcile/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("legacy site-less resume route status = %d, want 404", resp.StatusCode)
	}
}
