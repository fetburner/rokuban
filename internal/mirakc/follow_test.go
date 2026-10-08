package mirakc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type followTestResponse struct {
	body   io.ReadCloser
	length int64
	err    error
}

type followTestRecord struct {
	status string
	err    error
}

type followTestClient struct {
	mu sync.Mutex

	responses []followTestResponse
	records   []followTestRecord
	offsets   []int64
	gets      int
	events    []string
}

func (c *followTestClient) StreamRecord(_ context.Context, _ string, offset int64) (io.ReadCloser, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offsets = append(c.offsets, offset)
	c.events = append(c.events, fmt.Sprintf("range:%d", offset))
	if len(c.responses) == 0 {
		return nil, 0, ErrRangeNotSatisfiable
	}
	response := c.responses[0]
	c.responses = c.responses[1:]
	return response.body, response.length, response.err
}

func (c *followTestClient) GetRecord(context.Context, string) (*Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	c.events = append(c.events, "status")
	if len(c.records) == 0 {
		return &Record{Recording: RecordInfo{Status: "finished"}}, nil
	}
	record := c.records[0]
	if len(c.records) > 1 {
		c.records = c.records[1:]
	}
	if record.err != nil {
		return nil, record.err
	}
	return &Record{Recording: RecordInfo{Status: record.status}}, nil
}

func (c *followTestClient) snapshot() ([]int64, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.offsets...), c.gets
}

func (c *followTestClient) eventSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

func newFastRecordFollowReader(ctx context.Context, client RecordFollowClient, recordID string, offset int64, body io.ReadCloser, options RecordFollowOptions) *RecordFollowReader {
	reader := NewRecordFollowReader(ctx, client, recordID, offset, body, options)
	reader.retryDelay = func(int) time.Duration { return 0 }
	return reader
}

func followBody(data string, length int64) followTestResponse {
	return followTestResponse{body: io.NopCloser(stringReader(data)), length: length}
}

func emptyFollowRange() followTestResponse {
	return followTestResponse{body: io.NopCloser(stringReader("")), length: 0}
}

func stringReader(value string) io.Reader {
	return &followStringReader{value: value}
}

type followStringReader struct{ value string }

func (r *followStringReader) Read(p []byte) (int, error) {
	if len(r.value) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.value)
	r.value = r.value[n:]
	return n, nil
}

func TestRecordFollowReaderDrainsFinalRangeAfterFinished(t *testing.T) {
	client := &followTestClient{
		responses: []followTestResponse{
			{err: ErrRangeNotSatisfiable},
			followBody("tail", 4),
			{err: ErrRangeNotSatisfiable},
		},
		records: []followTestRecord{{status: "finished"}},
	}
	var observed []string
	var rangeEnds []string
	reader := newFastRecordFollowReader(context.Background(), client, "record", 0,
		io.NopCloser(stringReader("head")), RecordFollowOptions{
			StallTimeout: time.Second,
			OnRecord: func(record *Record, offset int64) error {
				observed = append(observed, fmt.Sprintf("%s:%d", record.Recording.Status, offset))
				return nil
			},
			OnRangeEnd: func(offset, bodyBytes int64) {
				rangeEnds = append(rangeEnds, fmt.Sprintf("%d:%d", offset, bodyBytes))
			},
		})
	data, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatalf("io.ReadAll() error = %v", err)
	}
	if string(data) != "headtail" {
		t.Fatalf("read data = %q, want headtail", data)
	}
	if got, want := observed, []string{"finished:4"}; !equalStrings(got, want) {
		t.Errorf("status observations = %v, want %v", got, want)
	}
	if got, want := rangeEnds, []string{"4:4", "8:4"}; !equalStrings(got, want) {
		t.Errorf("completed bodies = %v, want %v", got, want)
	}
	offsets, gets := client.snapshot()
	if got, want := offsets, []int64{4, 4, 8}; !equalInt64s(got, want) {
		t.Errorf("Range offsets = %v, want %v", got, want)
	}
	if gets != 1 {
		t.Errorf("GetRecord calls = %d, want 1", gets)
	}
	if got, want := client.eventSnapshot(), []string{"range:4", "status", "range:4", "range:8"}; !equalStrings(got, want) {
		t.Errorf("request order = %v, want %v (status is fetched only after an empty Range)", got, want)
	}
}

func TestRecordFollowReaderUnknownStatusRetriesInsteadOfEnding(t *testing.T) {
	client := &followTestClient{
		responses: []followTestResponse{
			{err: ErrRangeNotSatisfiable},
			{err: ErrRangeNotSatisfiable},
			followBody("tail", -1),
			{err: ErrRangeNotSatisfiable},
		},
		records: []followTestRecord{{status: "unknown"}, {status: "finished"}},
	}
	reader := NewRecordFollowReader(context.Background(), client, "record", 0, nil, RecordFollowOptions{
		StallTimeout: time.Second,
	})
	data, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatalf("io.ReadAll() error = %v", err)
	}
	if string(data) != "tail" {
		t.Fatalf("read data = %q, want tail (unknown status must not end the stream)", data)
	}
	offsets, gets := client.snapshot()
	if gets != 2 {
		t.Errorf("GetRecord calls = %d, want 2 (unknown status must be retried)", gets)
	}
	if got, want := offsets, []int64{0, 0, 0, 4}; !equalInt64s(got, want) {
		t.Errorf("Range offsets = %v, want %v", got, want)
	}
}

func TestRecordFollowReaderRetriesTransientRangeFailuresAtLimit(t *testing.T) {
	serverError := &APIError{StatusCode: 502, Status: "502 Bad Gateway"}
	client := &followTestClient{
		responses: []followTestResponse{
			{err: serverError},
			{err: serverError},
			{err: serverError},
			{err: serverError},
			{err: serverError},
			followBody("tail", -1),
			{err: ErrRangeNotSatisfiable},
			{err: ErrRangeNotSatisfiable},
		},
		records: []followTestRecord{{status: "finished"}},
	}
	reader := newFastRecordFollowReader(context.Background(), client, "record", 0, nil, RecordFollowOptions{})
	data, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(data) != "tail" {
		t.Fatalf("io.ReadAll() = %q, %v; want tail after five transient failures", data, err)
	}
	offsets, gets := client.snapshot()
	if len(offsets) != 8 || offsets[0] != 0 || offsets[5] != 0 || offsets[6] != 4 || offsets[7] != 4 {
		t.Errorf("Range offsets = %v, want five failed requests and recovery at 0, then final reads at 4", offsets)
	}
	if gets != 1 {
		t.Errorf("GetRecord calls = %d, want 1 after the recovered range reaches EOF", gets)
	}
}

func TestRecordFollowReaderStopsAfterSixthTransientFailure(t *testing.T) {
	serverError := &APIError{StatusCode: 502, Status: "502 Bad Gateway"}
	client := &followTestClient{responses: make([]followTestResponse, MaxConsecutiveRetries+1)}
	for i := range client.responses {
		client.responses[i] = followTestResponse{err: serverError}
	}
	reader := newFastRecordFollowReader(context.Background(), client, "record", 0, nil, RecordFollowOptions{})
	_, err := io.ReadAll(reader)
	_ = reader.Close()
	if err == nil || !strings.Contains(err.Error(), "failed 6 consecutive times") {
		t.Fatalf("io.ReadAll() error = %v, want an error on the sixth consecutive failure", err)
	}
	offsets, gets := client.snapshot()
	if len(offsets) != 6 || gets != 0 {
		t.Errorf("requests after retry limit = %d Range and %d status, want six Range and no status", len(offsets), gets)
	}
}

func TestRecordFollowReaderDoesNotRetryPermanentRangeFailure(t *testing.T) {
	client := &followTestClient{responses: []followTestResponse{{err: &APIError{StatusCode: 400, Status: "400 Bad Request"}}}}
	reader := newFastRecordFollowReader(context.Background(), client, "record", 0, nil, RecordFollowOptions{})
	_, err := io.ReadAll(reader)
	_ = reader.Close()
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		t.Fatalf("io.ReadAll() error = %v, want permanent 400 APIError", err)
	}
	offsets, gets := client.snapshot()
	if len(offsets) != 1 || gets != 0 {
		t.Errorf("requests after permanent failure = %d Range and %d status, want one Range and no status", len(offsets), gets)
	}
}

func TestRecordFollowReaderRetriesTransientStatusFailure(t *testing.T) {
	client := &followTestClient{
		responses: []followTestResponse{
			{err: ErrRangeNotSatisfiable},
			{err: ErrRangeNotSatisfiable},
			{err: ErrRangeNotSatisfiable},
		},
		records: []followTestRecord{
			{err: &APIError{StatusCode: 503, Status: "503 Service Unavailable"}},
			{status: "finished"},
		},
	}
	reader := newFastRecordFollowReader(context.Background(), client, "record", 0, nil, RecordFollowOptions{})
	data, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || len(data) != 0 {
		t.Fatalf("io.ReadAll() = %q, %v; want empty EOF after status retry", data, err)
	}
	offsets, gets := client.snapshot()
	if gets != 2 || !equalInt64s(offsets, []int64{0, 0, 0}) {
		t.Errorf("status retry calls = %d GetRecord, offsets %v; want 2 and [0 0 0]", gets, offsets)
	}
}

func TestRecordFollowReaderTreatsCanceledAndFailedAsTerminal(t *testing.T) {
	for _, status := range []string{"canceled", "failed"} {
		t.Run(status, func(t *testing.T) {
			client := &followTestClient{
				responses: []followTestResponse{{err: ErrRangeNotSatisfiable}, {err: ErrRangeNotSatisfiable}},
				records:   []followTestRecord{{status: status}},
			}
			reader := newFastRecordFollowReader(context.Background(), client, "record", 0, nil, RecordFollowOptions{})
			data, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil || len(data) != 0 {
				t.Fatalf("io.ReadAll() = %q, %v; want empty EOF", data, err)
			}
			offsets, gets := client.snapshot()
			if gets != 1 || !equalInt64s(offsets, []int64{0, 0}) {
				t.Errorf("terminal follow calls = %d GetRecord, offsets %v; want 1 and two final Range requests", gets, offsets)
			}
		})
	}
}

func TestRecordFollowReaderTreatsEmptyResponsesAsCatchUp(t *testing.T) {
	tests := []struct {
		name     string
		response followTestResponse
	}{
		{name: "204", response: followTestResponse{err: ErrRecordNotReady}},
		{name: "416", response: followTestResponse{err: ErrRangeNotSatisfiable}},
		{name: "206 with zero bytes", response: emptyFollowRange()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &followTestClient{
				responses: []followTestResponse{tt.response, {err: ErrRangeNotSatisfiable}},
				records:   []followTestRecord{{status: "finished"}},
			}
			reader := NewRecordFollowReader(context.Background(), client, "record", 0, nil, RecordFollowOptions{
				StallTimeout: time.Second,
			})
			data, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil {
				t.Fatalf("io.ReadAll() error = %v", err)
			}
			if len(data) != 0 {
				t.Errorf("read %d bytes, want none", len(data))
			}
			offsets, gets := client.snapshot()
			if gets != 1 || len(offsets) != 2 {
				t.Errorf("GetRecord calls = %d, Range calls = %d; want 1 and 2", gets, len(offsets))
			}
		})
	}
}

func TestRecordFollowReader404HookMustConfirmCommittedEnd(t *testing.T) {
	notFound := &APIError{StatusCode: 404, Status: "404 Not Found"}
	tests := []struct {
		name    string
		hook    func(int64, error) error
		wantErr error
	}{
		{
			name: "committed end",
			hook: func(offset int64, cause error) error {
				if offset != 12 || !errors.Is(cause, notFound) {
					t.Errorf("404 hook args = (%d, %v), want offset 12 and original 404", offset, cause)
				}
				return io.EOF
			},
		},
		{
			name:    "incomplete original",
			hook:    func(int64, error) error { return errFollowTestIncomplete },
			wantErr: errFollowTestIncomplete,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &followTestClient{responses: []followTestResponse{{err: notFound}}}
			reader := NewRecordFollowReader(context.Background(), client, "record", 12, nil, RecordFollowOptions{
				StallTimeout:     time.Second,
				OnRecordNotFound: tt.hook,
			})
			_, err := io.ReadAll(reader)
			_ = reader.Close()
			if tt.wantErr == nil && err != nil {
				t.Fatalf("io.ReadAll() error = %v, want EOF", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("io.ReadAll() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

var errFollowTestIncomplete = errors.New("incomplete committed original")

func TestRecordFollowReaderStallAndConsumerBackpressure(t *testing.T) {
	t.Run("blocked body is retried", func(t *testing.T) {
		body := newTimedFollowBody(300 * time.Millisecond)
		client := &followTestClient{
			responses: []followTestResponse{
				{body: body, length: -1},
				followBody("tail", -1),
				{err: ErrRangeNotSatisfiable},
			},
			records: []followTestRecord{{status: "finished"}},
		}
		reader := NewRecordFollowReader(context.Background(), client, "record", 0, nil, RecordFollowOptions{
			StallTimeout: 20 * time.Millisecond,
		})
		data, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatalf("io.ReadAll() error = %v", err)
		}
		if string(data) != "tail" {
			t.Fatalf("read data = %q, want tail after the stalled body", data)
		}
		select {
		case <-body.closedBeforeReadDeadline:
		default:
			t.Fatal("reader did not close the blocked body at StallTimeout")
		}
		offsets, _ := client.snapshot()
		if len(offsets) < 2 || offsets[0] != 0 || offsets[1] != 0 {
			t.Fatalf("Range offsets = %v, want a restart from offset 0", offsets)
		}
	})

	t.Run("time before Read is not a stall", func(t *testing.T) {
		body := newGatedFollowBody("head")
		client := &followTestClient{
			responses: []followTestResponse{
				{err: ErrRangeNotSatisfiable},
				{err: ErrRangeNotSatisfiable},
			},
			records: []followTestRecord{{status: "finished"}},
		}
		reader := NewRecordFollowReader(context.Background(), client, "record", 0, body, RecordFollowOptions{
			StallTimeout: 100 * time.Millisecond,
		})
		time.Sleep(50 * time.Millisecond)
		select {
		case <-body.closed:
			t.Fatal("reader closed the initial body before Read started")
		default:
		}
		type result struct {
			data string
			err  error
		}
		done := make(chan result, 1)
		go func() {
			data, err := io.ReadAll(reader)
			done <- result{data: string(data), err: err}
		}()
		select {
		case <-body.readStarted:
		case <-time.After(time.Second):
			t.Fatal("Read did not reach the initial body")
		}
		body.release()
		select {
		case got := <-done:
			_ = reader.Close()
			if got.err != nil || got.data != "head" {
				t.Fatalf("io.ReadAll() = %q, %v; want head without a stall", got.data, got.err)
			}
		case <-time.After(time.Second):
			_ = reader.Close()
			t.Fatal("reader did not finish after consumer started reading")
		}
		offsets, _ := client.snapshot()
		if len(offsets) != 2 {
			t.Errorf("Range calls = %d, want 2 after a healthy initial body", len(offsets))
		}
	})

	t.Run("slow consumer is not a stall", func(t *testing.T) {
		body := newTrackedFollowBody("headtail")
		client := &followTestClient{
			responses: []followTestResponse{{err: ErrRangeNotSatisfiable}},
			records:   []followTestRecord{{status: "finished"}},
		}
		reader := NewRecordFollowReader(context.Background(), client, "record", 0, body, RecordFollowOptions{
			StallTimeout: 20 * time.Millisecond,
		})
		writer := newBlockingFollowWriter()
		result := make(chan struct {
			data string
			err  error
		}, 1)
		go func() {
			var data strings.Builder
			_, err := io.Copy(io.MultiWriter(&data, writer), reader)
			result <- struct {
				data string
				err  error
			}{data: data.String(), err: err}
		}()
		select {
		case <-writer.writeStarted:
		case <-time.After(time.Second):
			_ = reader.Close()
			t.Fatal("consumer Write did not start")
		}
		time.Sleep(50 * time.Millisecond)
		select {
		case <-body.closed:
			writer.release()
			_ = reader.Close()
			t.Fatal("reader closed the body while the consumer was writing")
		default:
		}
		writer.release()
		select {
		case got := <-result:
			_ = reader.Close()
			if got.err != nil || got.data != "headtail" {
				t.Fatalf("io.Copy() = %q, %v; want all bytes without a stall", got.data, got.err)
			}
		case <-time.After(time.Second):
			_ = reader.Close()
			t.Fatal("reader did not finish after the consumer Write returned")
		}
	})
}

func TestRecordFollowReaderCloseStopsConcurrentRead(t *testing.T) {
	body := newBlockingFollowBody()
	client := &followTestClient{responses: []followTestResponse{{body: body, length: -1}}}
	reader := NewRecordFollowReader(context.Background(), client, "record", 0, nil, RecordFollowOptions{
		StallTimeout: time.Second,
	})
	done := make(chan error, 1)
	go func() {
		_, err := reader.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case <-body.readStarted:
	case <-time.After(time.Second):
		t.Fatal("Read did not reach the blocking body")
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case err := <-done:
		if err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("concurrent Read() error = %v, want a close error", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Close did not unblock the concurrent Read")
	}
	offsets, _ := client.snapshot()
	if len(offsets) != 1 {
		t.Errorf("Range calls after Close = %d, want no retry", len(offsets))
	}
}

type blockingFollowBody struct {
	readStarted chan struct{}
	closed      chan struct{}
	once        sync.Once
}

type timedFollowBody struct {
	readStarted              chan struct{}
	closed                   chan struct{}
	closedBeforeReadDeadline chan struct{}
	readDeadline             time.Duration
	startOnce                sync.Once
	closeOnce                sync.Once
	earlyCloseOnce           sync.Once
}

func newTimedFollowBody(readDeadline time.Duration) *timedFollowBody {
	return &timedFollowBody{
		readStarted:              make(chan struct{}),
		closed:                   make(chan struct{}),
		closedBeforeReadDeadline: make(chan struct{}),
		readDeadline:             readDeadline,
	}
}

func (b *timedFollowBody) Read([]byte) (int, error) {
	startedAt := time.Now()
	b.startOnce.Do(func() { close(b.readStarted) })
	timer := time.NewTimer(b.readDeadline)
	defer timer.Stop()
	select {
	case <-b.closed:
		if time.Since(startedAt) < b.readDeadline {
			b.earlyCloseOnce.Do(func() { close(b.closedBeforeReadDeadline) })
		}
		return 0, io.ErrClosedPipe
	case <-timer.C:
		return 0, io.ErrUnexpectedEOF
	}
}

func (b *timedFollowBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func newBlockingFollowBody() *blockingFollowBody {
	return &blockingFollowBody{readStarted: make(chan struct{}), closed: make(chan struct{})}
}

func (b *blockingFollowBody) Read([]byte) (int, error) {
	select {
	case <-b.readStarted:
	default:
		close(b.readStarted)
	}
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *blockingFollowBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

type gatedFollowBody struct {
	data        string
	readStarted chan struct{}
	released    chan struct{}
	closed      chan struct{}
	releaseOnce sync.Once
	closeOnce   sync.Once
	readOnce    sync.Once
}

type trackedFollowBody struct {
	reader *strings.Reader
	closed chan struct{}
	once   sync.Once
}

func newTrackedFollowBody(data string) *trackedFollowBody {
	return &trackedFollowBody{reader: strings.NewReader(data), closed: make(chan struct{})}
}

func (b *trackedFollowBody) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *trackedFollowBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func newGatedFollowBody(data string) *gatedFollowBody {
	return &gatedFollowBody{
		data:        data,
		readStarted: make(chan struct{}),
		released:    make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

func (b *gatedFollowBody) Read(p []byte) (int, error) {
	b.readOnce.Do(func() { close(b.readStarted) })
	<-b.released
	n := copy(p, b.data)
	b.data = b.data[n:]
	if n > 0 {
		return n, nil
	}
	return 0, io.EOF
}

func (b *gatedFollowBody) release() {
	b.releaseOnce.Do(func() { close(b.released) })
}

func (b *gatedFollowBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	b.release()
	return nil
}

type blockingFollowWriter struct {
	writeStarted chan struct{}
	released     chan struct{}
	startOnce    sync.Once
	releaseOnce  sync.Once
}

func newBlockingFollowWriter() *blockingFollowWriter {
	return &blockingFollowWriter{writeStarted: make(chan struct{}), released: make(chan struct{})}
}

func (w *blockingFollowWriter) Write(p []byte) (int, error) {
	w.startOnce.Do(func() { close(w.writeStarted) })
	<-w.released
	return len(p), nil
}

func (w *blockingFollowWriter) release() {
	w.releaseOnce.Do(func() { close(w.released) })
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equalInt64s(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
