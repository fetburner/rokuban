package mirakc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Client は mirakc の Web API クライアント。
// M1-1 のスコープ: schedules CRUD / records list・get・delete / records stream (Range, HEAD) / version。
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient は指定の baseURL に対する mirakc クライアントを作成する。
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: baseURL, httpClient: httpClient}
}

// GetVersion は GET /api/version を呼ぶ。
func (c *Client) GetVersion(ctx context.Context) (*Version, error) {
	var v Version
	if err := c.getJSON(ctx, "/api/version", &v); err != nil {
		return nil, fmt.Errorf("getting version: %w", err)
	}
	return &v, nil
}

// ListSchedules は GET /api/recording/schedules を呼ぶ。
func (c *Client) ListSchedules(ctx context.Context) ([]Schedule, error) {
	var schedules []Schedule
	if err := c.getJSON(ctx, "/api/recording/schedules", &schedules); err != nil {
		return nil, fmt.Errorf("listing schedules: %w", err)
	}
	return schedules, nil
}

// GetSchedule は GET /api/recording/schedules/{programID} を呼ぶ。
func (c *Client) GetSchedule(ctx context.Context, programID int64) (*Schedule, error) {
	var s Schedule
	if err := c.getJSON(ctx, fmt.Sprintf("/api/recording/schedules/%d", programID), &s); err != nil {
		return nil, fmt.Errorf("getting schedule %d: %w", programID, err)
	}
	return &s, nil
}

// CreateSchedule は POST /api/recording/schedules を呼ぶ。
func (c *Client) CreateSchedule(ctx context.Context, input ScheduleInput) (*Schedule, error) {
	var s Schedule
	if err := c.postJSON(ctx, "/api/recording/schedules", input, &s); err != nil {
		return nil, fmt.Errorf("creating schedule: %w", err)
	}
	return &s, nil
}

// DeleteSchedule は DELETE /api/recording/schedules/{programID} を呼ぶ。
func (c *Client) DeleteSchedule(ctx context.Context, programID int64) error {
	if err := c.delete(ctx, fmt.Sprintf("/api/recording/schedules/%d", programID)); err != nil {
		return fmt.Errorf("deleting schedule %d: %w", programID, err)
	}
	return nil
}

// ListRecords は GET /api/recording/records を呼ぶ。
func (c *Client) ListRecords(ctx context.Context) ([]Record, error) {
	var records []Record
	if err := c.getJSON(ctx, "/api/recording/records", &records); err != nil {
		return nil, fmt.Errorf("listing records: %w", err)
	}
	return records, nil
}

// GetRecord は GET /api/recording/records/{id} を呼ぶ。
func (c *Client) GetRecord(ctx context.Context, id string) (*Record, error) {
	var r Record
	if err := c.getJSON(ctx, fmt.Sprintf("/api/recording/records/%s", url.PathEscape(id)), &r); err != nil {
		return nil, fmt.Errorf("getting record %s: %w", id, err)
	}
	return &r, nil
}

// DeleteRecord は DELETE /api/recording/records/{id} を呼ぶ。
// purge=true の場合、コンテンツファイルも削除する。
func (c *Client) DeleteRecord(ctx context.Context, id string, purge bool) (*RecordRemovalResult, error) {
	path := fmt.Sprintf("/api/recording/records/%s", url.PathEscape(id))
	if purge {
		path += "?purge=true"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := checkStatus(resp, http.StatusOK); err != nil {
		return nil, err
	}

	var result RecordRemovalResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// StreamRecord は GET /api/recording/records/{id}/stream を呼ぶ。
// offset > 0 の場合、Range: bytes=offset- ヘッダーを付与する。
// 呼び出し側が body を Close する責任を持つ。
func (c *Client) StreamRecord(ctx context.Context, id string, offset int64) (io.ReadCloser, int64, error) {
	path := fmt.Sprintf("/api/recording/records/%s/stream", id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("building request: %w", err)
	}

	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("sending request: %w", err)
	}

	if offset > 0 {
		if err := checkStatus(resp, http.StatusPartialContent); err != nil {
			_ = resp.Body.Close()
			return nil, 0, err
		}
	} else {
		if err := checkStatus(resp, http.StatusOK); err != nil {
			_ = resp.Body.Close()
			return nil, 0, err
		}
	}

	return resp.Body, resp.ContentLength, nil
}

// HeadRecordStream は HEAD /api/recording/records/{id}/stream を呼ぶ。
// Content-Length を返す。
func (c *Client) HeadRecordStream(ctx context.Context, id string) (int64, error) {
	path := fmt.Sprintf("/api/recording/records/%s/stream", id)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.baseURL+path, nil)
	if err != nil {
		return 0, fmt.Errorf("building request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := checkStatus(resp, http.StatusOK); err != nil {
		return 0, err
	}

	return resp.ContentLength, nil
}

// ListServices は GET /api/services を呼ぶ。
func (c *Client) ListServices(ctx context.Context) ([]Service, error) {
	var services []Service
	if err := c.getJSON(ctx, "/api/services", &services); err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	return services, nil
}

// ListPrograms は GET /api/programs を呼ぶ。
func (c *Client) ListPrograms(ctx context.Context) ([]Program, error) {
	var programs []Program
	if err := c.getJSON(ctx, "/api/programs", &programs); err != nil {
		return nil, fmt.Errorf("listing programs: %w", err)
	}
	return programs, nil
}

// APIError は mirakc API がエラーステータスを返した場合のエラー。
type APIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *APIError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("mirakc API %s: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("mirakc API %s", e.Status)
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

func (c *Client) postJSON(ctx context.Context, path string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := checkStatus(resp, http.StatusCreated); err != nil {
		return err
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

func (c *Client) delete(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	return checkStatus(resp, http.StatusOK)
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
