package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListEncodeProfiles(t *testing.T) {
	router := NewRouter(RouterConfig{
		EncodeProfileNames: []string{"h264", "hevc"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/encode-profiles")
	if err != nil {
		t.Fatalf("GET /api/encode-profiles: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body []EncodeProfileSummary
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(body) != 2 {
		t.Fatalf("len = %d, want 2: %#v", len(body), body)
	}
	if body[0].Name != "h264" || body[1].Name != "hevc" {
		t.Errorf("names = [%q, %q], want [h264, hevc]", body[0].Name, body[1].Name)
	}
	// 機微情報を載せない（container も未注入なら無い）。
	if body[0].Container != nil {
		t.Errorf("container should be omitted when only names are injected, got %v", *body[0].Container)
	}
}

func TestListEncodeProfiles_EmptyWhenNotConfigured(t *testing.T) {
	router := NewRouter(RouterConfig{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/encode-profiles")
	if err != nil {
		t.Fatalf("GET /api/encode-profiles: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body []EncodeProfileSummary
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	// null ではなく空配列。UI が length で分岐できるようにする。
	if body == nil {
		t.Fatal("body is null, want empty array")
	}
	if len(body) != 0 {
		t.Errorf("len = %d, want 0", len(body))
	}
}
