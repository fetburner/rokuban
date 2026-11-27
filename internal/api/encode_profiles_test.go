package api

import (
	"testing"
)

func TestListEncodeProfiles(t *testing.T) {
	srv := NewTestServer(t, nil, func(c *RouterConfig) {
		c.EncodeProfileNames = []string{"h264", "hevc"}
	})

	body := GetJSON[[]EncodeProfileSummary](t, srv.URL+"/api/encode-profiles")
	if len(body) != 2 {
		t.Fatalf("len = %d, want 2: %#v", len(body), body)
	}
	if body[0].Name != "h264" || body[1].Name != "hevc" {
		t.Errorf("names = [%q, %q], want [h264, hevc]", body[0].Name, body[1].Name)
	}
}

func TestListEncodeProfiles_EmptyWhenNotConfigured(t *testing.T) {
	srv := NewTestServer(t, nil)

	body := GetJSON[[]EncodeProfileSummary](t, srv.URL+"/api/encode-profiles")
	// null ではなく空配列。UI が length で分岐できるようにする。
	if body == nil {
		t.Fatal("body is null, want empty array")
	}
	if len(body) != 0 {
		t.Errorf("len = %d, want 0", len(body))
	}
}
