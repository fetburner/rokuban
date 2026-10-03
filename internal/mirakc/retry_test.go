package mirakc

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestRetryDelay(t *testing.T) {
	want := []time.Duration{
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
		3200 * time.Millisecond,
		5 * time.Second,
		5 * time.Second,
	}
	for attempt, w := range want {
		if got := RetryDelay(attempt); got != w {
			t.Errorf("RetryDelay(%d) = %v, want %v", attempt, got, w)
		}
	}
	if got := RetryDelay(80); got != 5*time.Second {
		t.Errorf("RetryDelay(80) = %v, want the 5s cap even when the shift overflows", got)
	}
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "502", err: &APIError{StatusCode: http.StatusBadGateway}, want: true},
		{name: "wrapped 503", err: fmt.Errorf("getting record: %w", &APIError{StatusCode: http.StatusServiceUnavailable}), want: true},
		{name: "network", err: errors.New("sending request: connection reset by peer"), want: true},
		{name: "404", err: &APIError{StatusCode: http.StatusNotFound}, want: false},
		{name: "400", err: &APIError{StatusCode: http.StatusBadRequest}, want: false},
		{name: "range ignored", err: &APIError{StatusCode: http.StatusOK}, want: false},
	}
	for _, tt := range tests {
		if got := IsRetryable(tt.err); got != tt.want {
			t.Errorf("IsRetryable(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
