package asynqadapter

import (
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/jobq"
)

func TestNewFromRedisURLRejectsInvalidURLWithoutCredentials(t *testing.T) {
	for _, tc := range []struct{ name, url, want string }{
		{"scheme", "http://user:secret@redis.example/0", "expected a redis:// or rediss:// URL with a host"},
		{"host", "redis://user:secret@/0", "expected a redis:// or rediss:// URL with a host"},
		{"syntax", "redis://user:secret@redis.example:invalid/0", "expected a redis:// or rediss:// URL with a host"},
		{"database", "redis://user:secret@redis.example/invalid-db", "invalid Redis database or connection options"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, closeAdapter, err := NewFromRedisURL(tc.url, jobq.NewRegistry(),
				func(jobq.Args) (string, error) { return "default", nil }, Config{})
			if closeAdapter != nil {
				t.Cleanup(func() {
					if err := closeAdapter(); err != nil {
						t.Error(err)
					}
				})
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal("expected a Redis URL validation error")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "user:") {
				t.Fatal("Redis URL error exposes credentials")
			}
		})
	}
}
