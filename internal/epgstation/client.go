// Package epgstation は EPGStation からの移行に使う REST API 型とクライアントを提供する。
package epgstation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client は EPGStation の Web API クライアント。
type Client struct {
	baseURL string
	// httpClient は短命な JSON 呼び出し用。全体タイムアウトを持つ。
	httpClient *http.Client
}

// 短命な呼び出しの全体タイムアウト。/api/rules はページングで何度も呼ぶが、
// 1 回あたりは軽量な JSON なので mirakc クライアントと同じ値を使う。
const shortRequestTimeout = 60 * time.Second

// 接続確立と「最初のバイトが返るまで」の上限。
const (
	dialTimeout           = 10 * time.Second
	responseHeaderTimeout = 30 * time.Second
)

// NewClient は指定の baseURL に対する EPGStation クライアントを作成する。
// httpClient を渡した場合はそれをそのまま使う（テスト用。タイムアウトの責務は
// 呼び出し側に移る）。nil ならタイムアウト付きのものを組む。
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient != nil {
		return &Client{baseURL: baseURL, httpClient: httpClient}
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   4,
	}
	return &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{Transport: transport, Timeout: shortRequestTimeout},
	}
}

// APIError は EPGStation API がエラーステータスを返した場合のエラー。
type APIError struct {
	StatusCode int
	Status     string
	Body       string
}

// Error はエラーメッセージを返す。
func (e *APIError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("epgstation API %s: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("epgstation API %s", e.Status)
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := checkStatus(resp, http.StatusOK); err != nil {
		return err
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

func checkStatus(resp *http.Response, expected int) error {
	if resp.StatusCode == expected {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return &APIError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       string(body),
	}
}
