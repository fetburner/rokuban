package mirakc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ErrRecordNotReady は mirakc が records/{id}/stream に 204 No Content を返した
// ことを表す。録画が始まった直後は content file がまだ 0 バイト（または未作成）
// で、mirakc は理由を本文に書かずに 204 を返す。**これは正常な状態であって
// 接続失敗ではない**ので、ingest は接続リトライの予算を消費せずに待つ。
var ErrRecordNotReady = errors.New("mirakc: record content is not ready")

// ErrRangeNotSatisfiable は mirakc が records/{id}/stream に 416 を返したこと
// を表す。`Range: bytes=N-` の N が現在のサイズ以上、つまり**読むものが無い
// （追い付いた）**という意味である。録画中の record では `ContentRange::
// without_size` が first/last を検査しないため同じ状態が 206 + 0 バイトでも
// 現れる（どちらも呼び側は「追い付いた」として扱う）。
var ErrRangeNotSatisfiable = errors.New("mirakc: range not satisfiable")

// Client は mirakc の Web API クライアント。
// M1-1 のスコープ: schedules CRUD / records list・get・delete / records stream (Range, HEAD) / version。
type Client struct {
	baseURL string
	// httpClient は短命な JSON / HEAD 呼び出し用。全体タイムアウトを持つ。
	httpClient *http.Client
	// streamClient は長命な転送（record ストリーム・SSE）用。全体タイムアウトを持たない。
	streamClient *http.Client
}

// 短命な呼び出しの全体タイムアウト。ListPrograms は数千件の JSON を返すので
// 秒単位では足りず、かといって無制限だと mirakc の無応答でループが止まる。
const shortRequestTimeout = 60 * time.Second

// 接続確立と「最初のバイトが返るまで」の上限。これは**転送時間を制限しない**ので、
// 長命なストリームにも安全にかけられる。
const (
	dialTimeout           = 10 * time.Second
	responseHeaderTimeout = 30 * time.Second
)

// NewClient は指定の baseURL に対する mirakc クライアントを作成する。
//
// httpClient を渡した場合はストリーミング経路にも同じものを使う（テスト用。
// タイムアウトの責務は呼び出し側に移る）。nil なら 2 種類のクライアントを組む。
//
// **ストリーミングと SSE に全体タイムアウトを付けてはならない。** `http.Client.Timeout`
// はボディ読み出しまで含めた時間に効くため、687MB の record 転送や常時接続の SSE を
// 途中で切ってしまう（River のジョブタイムアウトで ingest が死んだのと同じ失敗）。
// 代わりに接続確立とレスポンスヘッダまでの時間だけを縛り、転送の停滞は
// ingest 側のストール検知（docs/recording.md §5.3）と ctx で扱う。
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient != nil {
		return &Client{baseURL: baseURL, httpClient: httpClient, streamClient: httpClient}
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
		baseURL:      baseURL,
		httpClient:   &http.Client{Transport: transport, Timeout: shortRequestTimeout},
		streamClient: &http.Client{Transport: transport},
	}
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

// StreamRecord は GET /api/recording/records/{id}/stream を呼び、offset 以降の
// 差分を返す。呼び出し側が body を Close する責任を持つ。
//
// **offset 0 でも必ず `Range: bytes=N-` を送り、206 を期待する。**
// mirakc の `ContentSource::new` は `(None, RecordingStatus::Recording)` のとき
// だけ `tail -f -c +0` を選ぶ。これは常に先頭から配る追従配信で、切断後に
// 途中オフセットから戻る手段が mirakc 側に無い。Range を送れば
// `(Some(range), _)` の 1 分岐に閉じ、応答は常に「リクエスト時点のサイズまでの
// 差分」になる（`last` はリクエスト時に確定し、以後の追記を追わない）。
// 経路が 2 本にならないことが、録画中の追従と切断後の再開を同じ契約で
// 扱える根拠である（docs/recording/ingest.md §5.1）。
//
// 戻り値の ContentLength は 206 の**本文長**（差分の長さ）であって総サイズでは
// ない。録画中は `Content-Range` の total が `*` になる。
func (c *Client) StreamRecord(ctx context.Context, id string, offset int64) (io.ReadCloser, int64, error) {
	path := fmt.Sprintf("/api/recording/records/%s/stream", id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))

	resp, err := c.streamClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("sending request: %w", err)
	}

	// 204 / 416 はどちらも「いま読めるものが無い」の表現で、録画中の正常な
	// 状態である。接続失敗と同じ扱いにすると、録画開始直後の 204 が続く窓や
	// 追い付いた後の 416 がリトライ予算を食い潰す。呼び側が区別できるよう
	// sentinel に落とす。
	switch resp.StatusCode {
	case http.StatusNoContent:
		_ = resp.Body.Close()
		return nil, 0, ErrRecordNotReady
	case http.StatusRequestedRangeNotSatisfiable:
		_ = resp.Body.Close()
		return nil, 0, ErrRangeNotSatisfiable
	case http.StatusOK:
		// RFC 9110 はサーバが Range を無視して 200 で完全な表現を返すことを
		// 許す。offset 0 ではその本文が求めた差分と同一なので受理する
		// （Range 非対応のサーバやフィルタ併用時に、完了済み record の
		// 全量取得が壊れない）。
		//
		// **offset > 0 では受理しない。** 本文は先頭から始まるので、そのまま
		// 追記すると先頭から offset ぶんが二重になり、先頭を捨てて読むと
		// ポーリングごとに offset バイトを再転送する黙った O(n^2) になる。
		// どちらも取らないので、原因がログに残る形で失敗させる。
		if offset > 0 {
			_ = resp.Body.Close()
			return nil, 0, &APIError{
				StatusCode: resp.StatusCode,
				Status:     resp.Status,
				Body:       "Range was ignored on a resumed request; appending would duplicate the head",
			}
		}
		return resp.Body, resp.ContentLength, nil
	}

	if err := checkStatus(resp, http.StatusPartialContent); err != nil {
		_ = resp.Body.Close()
		return nil, 0, err
	}

	return resp.Body, resp.ContentLength, nil
}

// StreamRecordFollow は GET /api/recording/records/{id}/stream を Range なしで
// 呼び、録画中の content file を先頭から追従する body を返す。
//
// StreamRecord は ingest の差分転送用であり、Range を付けると mirakc は「要求時点
// までの有限な差分」を返す。一方この経路は streamer の追っかけ再生用なので、
// mirakc の `(None, RecordingStatus::Recording)` 分岐（tail -f -c +0）を明示的に選ぶ。
// このメソッドへ Range や X-Mirakurun-Priority を足してはいけない。
//
// 204 は録画開始直後の content file 0 バイトを表す正常状態であり、呼び出し側が
// 予算を消費せずに待つため ErrRecordNotReady として返す。
func (c *Client) StreamRecordFollow(ctx context.Context, id string) (io.ReadCloser, error) {
	path := fmt.Sprintf("/api/recording/records/%s/stream", url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := c.streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	if resp.StatusCode == http.StatusNoContent {
		_ = resp.Body.Close()
		return nil, ErrRecordNotReady
	}
	if err := checkStatus(resp, http.StatusOK); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
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

// StreamService は GET /api/services/{id}/stream を呼ぶ（ライブ視聴、issue #91）。
//
// **decode=1 を常に付ける。** mirakc は 1.0.30 未満で decode クエリ無指定だと復号しない
// 非互換があった。明示することでバージョン差に依存しない。
//
// priority は X-Mirakurun-Priority ヘッダに載せる。mirakc はこれと schedule
// options.priority を同じ優先度スケールで扱ってチューナーを調停する
// （docs/recording/delegation.md §2「チューナー調停」）。ライブは録画より低い
// priority を渡し、チューナー枯渇時に録画側が常に勝つようにする（呼び出し側の
// 責務。streamer.LiveConfig.TunerPriority が既定 1、ruler の schedule 既定 priority は 10）。
//
// StreamRecord と同じ規約で、返す ReadCloser は呼び出し側が Close する。
// streamClient を使うため全体タイムアウトが無く、ライブの間ずっと張り続けられる
// （不変条件 2: mirakc とのやりとりは常に API）。
func (c *Client) StreamService(ctx context.Context, serviceID int64, priority int) (io.ReadCloser, error) {
	path := fmt.Sprintf("/api/services/%d/stream?decode=1", serviceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("X-Mirakurun-Priority", strconv.Itoa(priority))

	resp, err := c.streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	if err := checkStatus(resp, http.StatusOK); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}

	return resp.Body, nil
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

// ListTuners は GET /api/tuners を呼ぶ。
//
// 返るのはチューナーの静的な構成（Tuner のコメント参照）。実行時状態は
// デコードしないので、これを容量判定の「今の空き」として使うことはできない
// （そもそも引かない。docs/data.md §6.5）。
func (c *Client) ListTuners(ctx context.Context) ([]Tuner, error) {
	var tuners []Tuner
	if err := c.getJSON(ctx, "/api/tuners", &tuners); err != nil {
		return nil, fmt.Errorf("listing tuners: %w", err)
	}
	return tuners, nil
}

// APIError は mirakc API がエラーステータスを返した場合のエラー。
type APIError struct {
	StatusCode int
	Status     string
	Body       string
}

// Error はエラーメッセージを返す。
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
