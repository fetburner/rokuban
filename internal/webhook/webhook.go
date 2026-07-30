// Package webhook は録画ライフサイクルを外部へ通知する汎用 HTTP webhook クライアント
// （M3-11 / issue #73）。
//
// EPGStation の複数種外部コマンドフックを 1 本の HTTP POST に置き換える。
// ローカル exec は持たない（配布物・権限・ハングの面で負け筋）。
//
// 配送意味論:
//   - URL が空なら no-op
//   - at-least-once: 失敗時は同期で 1 回だけ再試行
//   - 本処理（ingest / encode 等）は webhook 成否で止めない。呼び出し側は
//     戻り値をログして握り潰す（Notify 自体は error を返す）
//
// ペイロードに絶対パスや credentials は載せない。
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/config"
)

// イベント type 定数。encode / deleted は後続マイルストーンで発火する。
const (
	EventRecordingFinished = "recording.finished"
	EventRecordingFailed   = "recording.failed"
	EventEncodeFinished    = "encode.finished"
	EventEncodeFailed      = "encode.failed"
	EventRecordingDeleted  = "recording.deleted"
)

// HeaderSecret は共有秘密を載せる HTTP ヘッダ名。
const HeaderSecret = "X-Rokuban-Webhook-Secret"

const defaultTimeout = 5 * time.Second

// Event は webhook で配送する 1 件のイベント。
//
// id は配送単位の UUID（受け側の冪等処理用）。同じ recordingId に対して
// type が再送されることがある（at-least-once）。
type Event struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	At          time.Time `json:"at"`
	RecordingID int64     `json:"recordingId"`
	Site        string    `json:"site"`
	Title       string    `json:"title,omitempty"`
	Status      string    `json:"status,omitempty"`
}

// Client は webhook 配送クライアント。
//
// ゼロ値や New に空 URL を渡した Client の Notify は常に no-op で成功する。
type Client struct {
	url     string
	secret  string
	timeout time.Duration
	// events は allowlist。空なら全 type を配送する。
	events map[string]struct{}
	http   *http.Client
}

// New は設定から Client を作る。
//
// cfg.URL が空でも Client は返し、Notify が no-op になる（呼び出し側で nil 判定しなくてよい）。
func New(cfg config.WebhookConfig) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	var allow map[string]struct{}
	if len(cfg.Events) > 0 {
		allow = make(map[string]struct{}, len(cfg.Events))
		for _, e := range cfg.Events {
			if e == "" {
				continue
			}
			allow[e] = struct{}{}
		}
	}

	return &Client{
		url:     cfg.URL,
		secret:  cfg.Secret,
		timeout: timeout,
		events:  allow,
		http:    &http.Client{Timeout: timeout},
	}
}

// Notify は ev を webhook 先へ POST する。
//
// URL が空、または events allowlist に type が含まれない場合は no-op で nil を返す。
// id / at が空ならここで埋める。失敗しても呼び出し側の本処理を止めない前提で、
// 同期 1 回まで再試行し、それでもダメなら error を返す（ログは呼び出し側）。
func (c *Client) Notify(ctx context.Context, ev Event) error {
	if c == nil || c.url == "" {
		return nil
	}
	if c.events != nil {
		if _, ok := c.events[ev.Type]; !ok {
			return nil
		}
	}

	if ev.ID == "" {
		ev.ID = uuid.NewString()
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	} else {
		ev.At = ev.At.UTC()
	}

	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshalling webhook event: %w", err)
	}

	if err := c.post(ctx, body); err != nil {
		// 同期 1 回だけ再試行（at-least-once の最小）。
		slog.Warn("webhook post failed, retrying once",
			"type", ev.Type, "recording_id", ev.RecordingID, "err", err)
		if retryErr := c.post(ctx, body); retryErr != nil {
			return fmt.Errorf("webhook post (after retry): %w", retryErr)
		}
	}
	return nil
}

func (c *Client) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		req.Header.Set(HeaderSecret, c.secret)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// レスポンス本体は捨てる（ログ用に少し読む）。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}
