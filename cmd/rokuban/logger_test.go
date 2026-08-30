package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
)

// TestNewLogHandler_Level は log.level がレベル判定に実際に効くことを、
// debug/warn の両方向で固定する（不変条件 8: レベル判定を壊すと落ちる形）。
func TestNewLogHandler_Level(t *testing.T) {
	t.Run("debug lets slog.Debug through", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(newLogHandler(config.LogConfig{Level: "debug", Format: "json"}, &buf))
		logger.Debug("hello debug")
		if !strings.Contains(buf.String(), "hello debug") {
			t.Fatalf("expected debug record to be written, got %q", buf.String())
		}
	})

	t.Run("warn drops slog.Info", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(newLogHandler(config.LogConfig{Level: "warn", Format: "json"}, &buf))
		logger.Info("hello info")
		if buf.Len() != 0 {
			t.Fatalf("expected info record to be dropped at warn level, got %q", buf.String())
		}
		logger.Warn("hello warn")
		if !strings.Contains(buf.String(), "hello warn") {
			t.Fatalf("expected warn record to be written, got %q", buf.String())
		}
	})
}

// TestNewLogHandler_Format は format: json が実際に 1 行 1 レコードの JSON に
// なることと、text がそうならないことを固定する。
func TestNewLogHandler_Format(t *testing.T) {
	t.Run("json produces one JSON record per line", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(newLogHandler(config.LogConfig{Level: "info", Format: "json"}, &buf))
		logger.Info("hello", "key", "value")

		var record map[string]any
		if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
			t.Fatalf("expected valid JSON line, got %q: %v", buf.String(), err)
		}
		if record["msg"] != "hello" || record["key"] != "value" {
			t.Fatalf("unexpected record: %v", record)
		}
	})

	t.Run("text does not produce JSON", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(newLogHandler(config.LogConfig{Level: "info", Format: "text"}, &buf))
		logger.Info("hello", "key", "value")

		var record map[string]any
		if err := json.Unmarshal(buf.Bytes(), &record); err == nil {
			t.Fatalf("expected text output, got parseable JSON: %q", buf.String())
		}
		if !strings.Contains(buf.String(), "msg=hello") {
			t.Fatalf("expected text handler output, got %q", buf.String())
		}
	})
}

// TestNewLogHandler_EmptyIsDefault は空文字（config.LogConfig.validate が
// 「未設定」として通す値）が defaults() と同じ info/json になることを固定する
// （internal/config の TestLoad_EmptyLogValuesAreTreatedAsUnset と対になる
// 消費側のテスト）。
func TestNewLogHandler_EmptyIsDefault(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(newLogHandler(config.LogConfig{}, &buf))

	logger.Debug("should be dropped at default info level")
	if buf.Len() != 0 {
		t.Fatalf("expected debug record to be dropped by default, got %q", buf.String())
	}

	logger.Info("hello")
	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("expected default format to be JSON, got %q: %v", buf.String(), err)
	}
}
