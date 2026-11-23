package queuecounthttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type counterFunc func(context.Context, string) (int64, error)

func (f counterFunc) CountDeliverable(ctx context.Context, queue string) (int64, error) {
	return f(ctx, queue)
}

func TestHandlerReturnsKEDAMetricValue(t *testing.T) {
	var gotQueue string
	handler := NewHandler(counterFunc(func(_ context.Context, queue string) (int64, error) {
		gotQueue = queue
		return 7, nil
	}))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/internal/queues/ingest_tokyo/deliverable-count", nil)
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if gotQueue != "ingest_tokyo" {
		t.Errorf("physical queue = %q, want %q", gotQueue, "ingest_tokyo")
	}
	if got, want := response.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := response.Body.String(), "{\"value\":7}\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestHandlerReturns5xxWhenCountFails(t *testing.T) {
	handler := NewHandler(counterFunc(func(context.Context, string) (int64, error) {
		return 0, errors.New("Redis unavailable")
	}))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/internal/queues/ingest_tokyo/deliverable-count", nil)
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if got, want := response.Body.String(), "{\"error\":\"queue count unavailable\"}\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}
