package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/config"
)

func TestNotify_EmptyURLIsNoop(t *testing.T) {
	c := New(config.WebhookConfig{})
	// 実サーバに飛ばないこと（url 空）。パニックせず nil を返す。
	if err := c.Notify(context.Background(), Event{
		Type:        EventRecordingFinished,
		RecordingID: 1,
		Site:        "default",
		Status:      "finished",
	}); err != nil {
		t.Fatalf("Notify with empty URL: %v", err)
	}

	// nil Client も no-op
	var nilClient *Client
	if err := nilClient.Notify(context.Background(), Event{Type: EventRecordingFinished}); err != nil {
		t.Fatalf("nil Client.Notify: %v", err)
	}
}

func TestNotify_SendsSecretHeaderAndPayload(t *testing.T) {
	var gotSecret string
	var gotBody []byte
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		gotCT = r.Header.Get("Content-Type")
		gotSecret = r.Header.Get(HeaderSecret)
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(config.WebhookConfig{
		URL:     srv.URL,
		Secret:  "topsecret",
		Timeout: 2 * time.Second,
	})
	if err := c.Notify(context.Background(), Event{
		Type:        EventRecordingFinished,
		RecordingID: 42,
		Site:        "default",
		Title:       "テスト番組",
		Status:      "finished",
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotSecret != "topsecret" {
		t.Errorf("secret header = %q, want topsecret", gotSecret)
	}

	var payload Event
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("unmarshalling payload: %v\nbody=%s", err, gotBody)
	}
	if payload.ID == "" {
		t.Error("payload.id is empty")
	}
	if payload.Type != EventRecordingFinished {
		t.Errorf("type = %q, want %q", payload.Type, EventRecordingFinished)
	}
	if payload.RecordingID != 42 {
		t.Errorf("recordingId = %d, want 42", payload.RecordingID)
	}
	if payload.Site != "default" {
		t.Errorf("site = %q, want default", payload.Site)
	}
	if payload.Title != "テスト番組" {
		t.Errorf("title = %q, want テスト番組", payload.Title)
	}
	if payload.Status != "finished" {
		t.Errorf("status = %q, want finished", payload.Status)
	}
	if payload.At.IsZero() {
		t.Error("at is zero")
	}
	// 絶対パスや credentials が載っていないこと（フィールドが無いこと自体が保証）
	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"path", "password", "secret", "token", "credential"} {
		if _, ok := raw[banned]; ok {
			t.Errorf("payload must not contain %q: %v", banned, raw)
		}
	}
}

func TestNotify_NoSecretHeaderWhenEmpty(t *testing.T) {
	var saw bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(HeaderSecret) != "" {
			saw = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(config.WebhookConfig{URL: srv.URL})
	if err := c.Notify(context.Background(), Event{Type: EventRecordingFailed, RecordingID: 1, Status: "failed"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if saw {
		t.Error("secret header should be absent when secret is empty")
	}
}

func TestNotify_EventsAllowlist(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(config.WebhookConfig{
		URL:    srv.URL,
		Events: []string{EventRecordingFinished},
	})

	if err := c.Notify(context.Background(), Event{Type: EventRecordingFailed, RecordingID: 1}); err != nil {
		t.Fatalf("Notify (filtered): %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("filtered event was sent (%d hits)", hits.Load())
	}

	if err := c.Notify(context.Background(), Event{Type: EventRecordingFinished, RecordingID: 1}); err != nil {
		t.Fatalf("Notify (allowed): %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("allowed event hits = %d, want 1", hits.Load())
	}
}

func TestNotify_ErrorPathDoesNotPanic_RetriesOnce(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(config.WebhookConfig{
		URL:     srv.URL,
		Timeout: time.Second,
	})
	err := c.Notify(context.Background(), Event{
		Type:        EventRecordingFinished,
		RecordingID: 7,
		Status:      "finished",
	})
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	// 初回 + 1 回再試行
	if got := hits.Load(); got != 2 {
		t.Errorf("hits = %d, want 2 (initial + one retry)", got)
	}
}

func TestNotify_TimeoutDoesNotPanic(t *testing.T) {
	// ハンドラが応答しないサーバ。Client timeout で切れること。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	c := New(config.WebhookConfig{
		URL:     srv.URL,
		Timeout: 50 * time.Millisecond,
	})
	err := c.Notify(context.Background(), Event{
		Type:        EventRecordingFailed,
		RecordingID: 9,
		Status:      "failed",
	})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestNotify_SuccessAfterRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(config.WebhookConfig{URL: srv.URL, Timeout: time.Second})
	if err := c.Notify(context.Background(), Event{Type: EventRecordingFinished, RecordingID: 1}); err != nil {
		t.Fatalf("Notify after retry success: %v", err)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2", hits.Load())
	}
}
