package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthz(t *testing.T) {
	router := NewRouter(nil)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want %q", body.Status, "ok")
	}
}

func TestGetVersion(t *testing.T) {
	router := NewRouter(nil)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/version")
	if err != nil {
		t.Fatalf("GET /api/version: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body VersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Version == "" {
		t.Error("version is empty")
	}
}

func TestAllowedHosts_ValidHost(t *testing.T) {
	router := NewRouter([]string{"localhost"})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestAllowedHosts_InvalidHost(t *testing.T) {
	router := NewRouter([]string{"localhost"})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.example.com"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestAllowedHosts_EmptyAllowsAll(t *testing.T) {
	router := NewRouter(nil)
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "anything.example.com"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d (empty allowlist should allow all)", resp.StatusCode, http.StatusOK)
	}
}
