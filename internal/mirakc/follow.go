package mirakc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// RecordFollowClient は録画追従に使う Client の一部である。
type RecordFollowClient interface {
	// StreamRecord は offset から始まる有限の Range 本文を要求する。
	StreamRecord(ctx context.Context, id string, offset int64) (io.ReadCloser, int64, error)
	// GetRecord は id の現在のメタデータと status を返す。
	GetRecord(ctx context.Context, id string) (*Record, error)
}

// RecordFollowOptions は RecordFollowReader の callback を設定する。
type RecordFollowOptions struct {
	// StallTimeout は Read 1 回が止まる時間を制限する。消費側が Read を呼んで
	// いない間は timer が動かないため、遅い writer を mirakc の stall と誤認しない。
	StallTimeout time.Duration

	// OnRecord は GetRecord の成功応答ごとに現在の byte offset とともに呼ばれる。
	// error を返すと reader を終了する。
	OnRecord func(record *Record, offset int64) error

	// OnRecordNotFound は reader の cancelable context、現在の byte offset、元の error とともに
	// 404 時に呼ばれる。caller がこの offset を録画の commit 済み終端と証明できる場合だけ
	// io.EOF を返す。それ以外の戻り値では、その error で reader を終了する。
	OnRecordNotFound func(ctx context.Context, offset int64, cause error) error

	// OnRangeInterrupted は本文が byte を返した後にエラー（読み取りエラー・stall）で終わった
	// とき、応答本文を閉じた後で呼ばれる。正常 EOF の Range と ctx キャンセルでは呼ばない。
	// 正常経路の進捗は消費側の書き込みで足りるので、再試行の前に最新位置を確定させたい
	// 呼び出し側だけが使う。Read を呼んだ goroutine 上で、byte を消費側へ返した後に実行する。
	OnRangeInterrupted func(offset, bodyBytes int64)

	// PollMin / PollMax は追い付いた状態のポーリング間隔の下限・上限。0 なら既定
	// （recordFollowPollMin / recordFollowPollMax）。RetryDelay は再試行前の待ち。
	// nil なら RetryDelay 関数。3 つとも、実時間の待ちを払いたくないテストが
	// 差し替えるための口で、本番の呼び出し側は設定しない。
	PollMin    time.Duration
	PollMax    time.Duration
	RetryDelay func(attempt int) time.Duration
}

// RecordFollowReader は有限の Range 応答をつなぎ、1 本の長命な reader として公開する。
//
// 終了判定の真実には record status を使う。空 Range の後に status を取得し、終端 status
// なら同じ offset に最後の Range をすぐ要求する。追加分が無いと分かるまで、空でない応答を
// 読み続ける。既知の終端 status は finished、canceled、failed である。caller は OnRecord
// で canceled / failed を拒否できる。hook が無ければ最後の Range を確認して stream を終える。
// 未知の status は連続した一時失敗として数え、stream を終端しない。
//
// 通常の Range 要求は 500ms 以上あける。追い付き状態の recording では間隔を 500ms から
// 1 秒へ広げる。再試行には RetryDelay を使い、終端 status 後の最後の Range はすぐ要求する。
// GetRecord は空応答の後だけ呼ぶ。この polling 方針を ingest と chase 再生で共有する。
//
// Read と並行して Close を呼べる。StallTimeout は現在の本文の Read が止まっている間だけ
// 適用され、消費側が Read を呼ぶまでの待ち時間は mirakc の stall に数えない。
type RecordFollowReader struct {
	ctx      context.Context
	cancel   context.CancelFunc
	client   RecordFollowClient
	recordID string
	options  RecordFollowOptions

	mu     sync.Mutex
	body   *recordFollowBody
	closed bool

	// State below is read and written only by the goroutine calling Read.
	nextOffset     int64
	finished       bool
	done           bool
	err            error
	nextRequestAt  time.Time
	idleWait       time.Duration
	pollMin        time.Duration
	pollMax        time.Duration
	failures       int
	retryDelay     func(attempt int) time.Duration
	pendingBody    *recordFollowBody
	pendingBodyErr error
}

type recordFollowBody struct {
	body   io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once

	// bytesRead is owned by RecordFollowReader's Read goroutine.
	bytesRead int64
}

func (b *recordFollowBody) close() error {
	var err error
	b.once.Do(func() {
		if b.cancel != nil {
			b.cancel()
		}
		err = b.body.Close()
	})
	return err
}

const (
	recordFollowPollMin = 500 * time.Millisecond
	recordFollowPollMax = time.Second
)

var errRecordFollowStalled = errors.New("mirakc record response stalled")

// NewRecordFollowReader は offset から読む reader を作成する。initialBody は既に開いた
// follow stream または Range 応答を渡せる。nil の場合は Range 要求から開始する。
// 返された reader は caller が Close する。
func NewRecordFollowReader(ctx context.Context, client RecordFollowClient, recordID string, offset int64, initialBody io.ReadCloser, options RecordFollowOptions) *RecordFollowReader {
	ctx, cancel := context.WithCancel(ctx)
	reader := &RecordFollowReader{
		ctx:        ctx,
		cancel:     cancel,
		client:     client,
		recordID:   recordID,
		options:    options,
		nextOffset: offset,
		retryDelay: RetryDelay,
		pollMin:    recordFollowPollMin,
		pollMax:    recordFollowPollMax,
	}
	if options.PollMin > 0 {
		reader.pollMin = options.PollMin
	}
	if options.PollMax > 0 {
		reader.pollMax = options.PollMax
	}
	if options.RetryDelay != nil {
		reader.retryDelay = options.RetryDelay
	}
	if initialBody != nil {
		reader.body = &recordFollowBody{body: initialBody}
	}
	return reader
}

// Read は現在の byte offset から録画を追従する。
func (r *RecordFollowReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		body, closed := r.currentBody()
		switch {
		case closed:
			return 0, io.ErrClosedPipe
		case r.err != nil:
			return 0, r.err
		case r.done:
			return 0, io.EOF
		case r.pendingBody != nil:
			if err := r.finishBody(r.pendingBody, r.pendingBodyErr); err != nil {
				r.err = err
			}
			r.pendingBody = nil
			r.pendingBodyErr = nil
			continue
		case body == nil:
			if err := r.requestNext(); err != nil {
				r.err = err
			}
			continue
		}

		n, readErr, stalled := r.readBody(body, p)
		if r.isClosed() {
			return 0, io.ErrClosedPipe
		}
		r.nextOffset += int64(n)
		body.bytesRead += int64(n)
		if n > 0 {
			r.failures = 0
			r.idleWait = 0
		}
		if ctxErr := r.ctx.Err(); ctxErr != nil {
			if n > 0 {
				// Deliver bytes already read before reporting cancellation. io.Copy
				// writes them before its next Read processes the pending body end.
				r.pendingBody = body
				r.pendingBodyErr = ctxErr
				return n, nil
			}
			return 0, ctxErr
		}

		endErr := readErr
		bodyEnded := false
		switch {
		case stalled:
			endErr = errRecordFollowStalled
			bodyEnded = true
		case errors.Is(readErr, io.EOF):
			endErr = nil
			bodyEnded = true
		case readErr != nil:
			bodyEnded = true
		}
		if bodyEnded {
			if n > 0 {
				// io.Copy writes these bytes before asking for another Read.
				// Finish the body on that next call so progress never leads the
				// destination file when its Write fails.
				r.pendingBody = body
				r.pendingBodyErr = endErr
				return n, nil
			}
			if err := r.finishBody(body, endErr); err != nil {
				r.err = err
			}
			continue
		}
		if n > 0 {
			return n, nil
		}
	}
}

func (r *RecordFollowReader) readBody(body *recordFollowBody, p []byte) (int, error, bool) {
	if r.options.StallTimeout <= 0 {
		n, err := body.body.Read(p)
		return n, err, false
	}

	fired := make(chan struct{})
	var stalled atomic.Bool
	timer := time.AfterFunc(r.options.StallTimeout, func() {
		stalled.Store(true)
		_ = body.close()
		close(fired)
	})
	n, err := body.body.Read(p)
	if !timer.Stop() {
		<-fired
	}
	return n, err, stalled.Load()
}

// finishBody closes a completed response and handles its EOF or failure. It is
// called only by the goroutine that owns Read.
func (r *RecordFollowReader) finishBody(body *recordFollowBody, bodyErr error) error {
	r.mu.Lock()
	if r.body != body {
		r.mu.Unlock()
		return nil
	}
	r.body = nil
	r.mu.Unlock()
	_ = body.close()

	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if bodyErr != nil && body.bytesRead > 0 && r.options.OnRangeInterrupted != nil {
		r.options.OnRangeInterrupted(r.nextOffset, body.bytesRead)
	}
	if bodyErr != nil {
		return r.failure(fmt.Errorf("reading record %s at offset %d: %w", r.recordID, r.nextOffset, bodyErr))
	}
	if body.bytesRead == 0 {
		return r.caughtUp()
	}
	return nil
}

func (r *RecordFollowReader) requestNext() error {
	if err := sleepRecordFollowUntil(r.ctx, r.nextRequestAt); err != nil {
		return err
	}
	if err := r.ctx.Err(); err != nil {
		return err
	}
	r.nextRequestAt = time.Now().Add(r.pollMin)
	attemptCtx, cancel := context.WithCancel(r.ctx)
	body, length, err := r.client.StreamRecord(attemptCtx, r.recordID, r.nextOffset)
	if err != nil {
		cancel()
		switch {
		case isRecordNotFound(err):
			return r.recordNotFound(err)
		case errors.Is(err, ErrRecordNotReady), errors.Is(err, ErrRangeNotSatisfiable):
			return r.caughtUp()
		default:
			return r.failure(fmt.Errorf("requesting record %s at offset %d: %w", r.recordID, r.nextOffset, err))
		}
	}
	if body == nil || length == 0 {
		cancel()
		if body != nil {
			_ = body.Close()
		}
		return r.caughtUp()
	}
	return r.setBody(&recordFollowBody{body: body, cancel: cancel})
}

func (r *RecordFollowReader) caughtUp() error {
	if r.finished {
		r.done = true
		return nil
	}
	record, err := r.client.GetRecord(r.ctx, r.recordID)
	if err != nil {
		if isRecordNotFound(err) {
			return r.recordNotFound(err)
		}
		return r.failure(fmt.Errorf("checking record %s status: %w", r.recordID, err))
	}
	if record == nil {
		return r.failure(errors.New("mirakc returned an empty record status response"))
	}
	if r.options.OnRecord != nil {
		if err := r.options.OnRecord(record, r.nextOffset); err != nil {
			return err
		}
	}
	switch record.Recording.Status {
	case "recording":
		r.failures = 0
		if r.idleWait == 0 {
			r.idleWait = r.pollMin
		} else {
			r.idleWait = min(r.idleWait*2, r.pollMax)
		}
		r.nextRequestAt = time.Now().Add(r.idleWait)
	case "finished", "canceled", "failed":
		r.failures = 0
		r.finished = true
		r.idleWait = 0
		r.nextRequestAt = time.Time{}
	default:
		return r.failure(fmt.Errorf("mirakc returned unknown record status %q", record.Recording.Status))
	}
	return nil
}

func (r *RecordFollowReader) recordNotFound(cause error) error {
	if r.options.OnRecordNotFound == nil {
		return fmt.Errorf("record %s not found at offset %d: %w", r.recordID, r.nextOffset, cause)
	}
	if err := r.options.OnRecordNotFound(r.ctx, r.nextOffset, cause); errors.Is(err, io.EOF) {
		r.done = true
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("record not found handler returned nil without confirming the end")
}

func (r *RecordFollowReader) failure(err error) error {
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if !IsRetryable(err) {
		return err
	}
	r.failures++
	if r.failures > MaxConsecutiveRetries {
		return fmt.Errorf("record %s follow failed %d consecutive times at offset %d: %w", r.recordID, r.failures, r.nextOffset, err)
	}
	delay := r.retryDelay(r.failures - 1)
	slog.Warn("mirakc: transient record follow failure, retrying",
		"record_id", r.recordID, "offset", r.nextOffset, "consecutive_failures", r.failures, "delay", delay, "err", err)
	r.nextRequestAt = time.Now().Add(delay)
	return nil
}

func (r *RecordFollowReader) currentBody() (*recordFollowBody, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body, r.closed
}

func (r *RecordFollowReader) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func (r *RecordFollowReader) setBody(body *recordFollowBody) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = body.close()
		return io.ErrClosedPipe
	}
	r.body = body
	r.mu.Unlock()
	return nil
}

// Close は保留中の要求または Read をキャンセルする。Read と並行して呼べる。
func (r *RecordFollowReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	body := r.body
	r.body = nil
	r.mu.Unlock()
	r.cancel()
	if body == nil {
		return nil
	}
	return body.close()
}

func isRecordNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func sleepRecordFollowUntil(ctx context.Context, at time.Time) error {
	wait := time.Until(at)
	if wait <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
