package mirakc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Event は SSE で受信したイベント。
type Event struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// RecordSavedData は recording.record-saved イベントのペイロード。
type RecordSavedData struct {
	RecordID        string `json:"recordId"`
	RecordingStatus string `json:"recordingStatus"`
}

// RecordingFailedData は recording.failed イベントのペイロード。
type RecordingFailedData struct {
	ProgramID int64        `json:"programId"`
	Reason    FailedReason `json:"reason"`
}

// RecordBrokenData は recording.record-broken イベントのペイロード。
type RecordBrokenData struct {
	RecordID string `json:"recordId"`
	Reason   string `json:"reason"`
}

// ProgramsUpdatedData は epg.programs-updated イベントのペイロード。
type ProgramsUpdatedData struct {
	ServiceID int64 `json:"serviceId"`
}

// SSEConfig は SSE クライアントの設定。
type SSEConfig struct {
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

func defaultSSEConfig() SSEConfig {
	return SSEConfig{
		InitialBackoff: 1 * time.Second,
		MaxBackoff:     30 * time.Second,
	}
}

// Subscribe は /events SSE を購読し、受信イベントを ch に送る。
// 切断時は指数バックオフで自動再接続する。ctx がキャンセルされるまでブロックする。
func (c *Client) Subscribe(ctx context.Context, ch chan<- Event, cfg *SSEConfig) error {
	if cfg == nil {
		d := defaultSSEConfig()
		cfg = &d
	}

	backoff := cfg.InitialBackoff
	var lastEventID string
	for {
		connected, id, err := c.subscribeOnce(ctx, ch, lastEventID)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if id != "" {
			lastEventID = id
		}
		if connected {
			backoff = cfg.InitialBackoff
		}
		slog.Warn("SSE connection lost, reconnecting", "err", err, "backoff", backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, cfg.MaxBackoff)
	}
}

func (c *Client) subscribeOnce(ctx context.Context, ch chan<- Event, lastEventID string) (bool, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/events", nil)
	if err != nil {
		return false, "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	resp, err := c.streamClient.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("connecting to SSE: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return false, "", fmt.Errorf("SSE endpoint returned %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var eventType string
	var dataLines []string
	var currentID string

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			if eventType != "" && len(dataLines) > 0 {
				data := strings.Join(dataLines, "\n")
				select {
				case ch <- Event{Type: eventType, Data: json.RawMessage(data)}:
				case <-ctx.Done():
					return true, currentID, ctx.Err()
				}
			}
			eventType = ""
			dataLines = dataLines[:0]
			continue
		}

		if strings.HasPrefix(line, ":") {
			continue
		}

		if strings.HasPrefix(line, "event:") {
			eventType = parseSSEValue(line, "event:")
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, parseSSEValue(line, "data:"))
		} else if strings.HasPrefix(line, "id:") {
			currentID = parseSSEValue(line, "id:")
		}
	}

	if err := scanner.Err(); err != nil {
		return true, currentID, fmt.Errorf("reading SSE stream: %w", err)
	}
	return true, currentID, fmt.Errorf("SSE stream closed by server")
}

// parseSSEValue は SSE 仕様に従いフィールド値を取り出す。
// コロン直後の最初のスペース 1 文字だけを除去する。
func parseSSEValue(line, prefix string) string {
	value := strings.TrimPrefix(line, prefix)
	return strings.TrimPrefix(value, " ")
}
