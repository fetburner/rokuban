package streamer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mirakc"
)

type fakeChaseRecordClient struct {
	mu        sync.Mutex
	errors    []error
	calls     int
	recordIDs []string
}

func (c *fakeChaseRecordClient) StreamRecordFollow(_ context.Context, recordID string) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.recordIDs = append(c.recordIDs, recordID)
	if len(c.errors) > 0 {
		err := c.errors[0]
		c.errors = c.errors[1:]
		if err != nil {
			return nil, err
		}
	}
	return io.NopCloser(strings.NewReader("fake-record")), nil
}

// StreamRecord と GetRecord は、追従配信が閉じた後の Range の続き（followChaseRecord）が
// 使う。この偽物の録画は追従配信の分で終わっている。
func (c *fakeChaseRecordClient) StreamRecord(context.Context, string, int64) (io.ReadCloser, int64, error) {
	return nil, 0, mirakc.ErrRangeNotSatisfiable
}

func (c *fakeChaseRecordClient) GetRecord(context.Context, string) (*mirakc.Record, error) {
	return &mirakc.Record{Recording: mirakc.RecordInfo{Status: "finished"}}, nil
}

// StreamService は使われない（このクライアントは追っかけ専用）。newLiveStreamer が
// 要求する mirakcLiveClient を満たすためだけにある。
func (c *fakeChaseRecordClient) StreamService(context.Context, int64, int) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

func (c *fakeChaseRecordClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func withPlaylistStartupTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	previous := playlistStartupTimeout
	playlistStartupTimeout = timeout
	t.Cleanup(func() { playlistStartupTimeout = previous })
}

func TestParseCanonicalRecordingID(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int64
		ok   bool
	}{
		{name: "zero", raw: "0", want: 0, ok: true},
		{name: "positive", raw: "123", want: 123, ok: true},
		{name: "leading zero", raw: "0123"},
		{name: "negative", raw: "-1"},
		{name: "plus sign", raw: "+1"},
		{name: "empty", raw: ""},
		{name: "overflow", raw: "9223372036854775808"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseCanonicalRecordingID(tt.raw)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("parseCanonicalRecordingID(%q) = (%d, %v), want (%d, %v)", tt.raw, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestParseCanonicalChaseOffset(t *testing.T) {
	tests := []struct {
		raw  string
		want int64
		ok   bool
	}{
		{raw: "", want: 0, ok: true},
		{raw: "0", want: 0, ok: true},
		{raw: "90", want: 90, ok: true},
		{raw: "090"},
		{raw: "-1"},
		{raw: "+1"},
		{raw: "9223372036854775808"},
	}
	for _, tt := range tests {
		got, ok := parseCanonicalChaseOffset(tt.raw)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseCanonicalChaseOffset(%q) = (%d, %v), want (%d, %v)", tt.raw, got, ok, tt.want, tt.ok)
		}
	}
}

func TestChaseStartByteOffsetUsesRecordingMetadata(t *testing.T) {
	length := uint64(188 * 1200)
	duration := int64(120_000)
	record := &mirakc.Record{
		Recording: mirakc.RecordInfo{
			Status:   "finished",
			Duration: &duration,
		},
		Content: mirakc.ContentInfo{Length: &length},
	}

	got, err := chaseStartByteOffset(record, 30)
	if err != nil {
		t.Fatalf("chaseStartByteOffset() = %v, want success", err)
	}
	if want := int64(188 * 300); got != want {
		t.Fatalf("chaseStartByteOffset() = %d, want %d", got, want)
	}
	if _, err := chaseStartByteOffset(record, 120); !errors.Is(err, errChaseOffsetUnavailable) {
		t.Fatalf("out-of-range offset error = %v, want errChaseOffsetUnavailable", err)
	}
}

type fakeSeekChaseRecordClient struct {
	mu      sync.Mutex
	offsets []int64
	chunks  []string
}

func (c *fakeSeekChaseRecordClient) StreamRecord(_ context.Context, recordID string, offset int64) (io.ReadCloser, int64, error) {
	if recordID != "opaque-record-id" {
		return nil, 0, errors.New("unexpected record id")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offsets = append(c.offsets, offset)
	if len(c.chunks) == 0 {
		return nil, 0, mirakc.ErrRangeNotSatisfiable
	}
	chunk := c.chunks[0]
	c.chunks = c.chunks[1:]
	return io.NopCloser(strings.NewReader(chunk)), int64(len(chunk)), nil
}

func (c *fakeSeekChaseRecordClient) StreamRecordFollow(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("follow endpoint must not be used for a seek")
}

func (c *fakeSeekChaseRecordClient) GetRecord(context.Context, string) (*mirakc.Record, error) {
	return &mirakc.Record{Recording: mirakc.RecordInfo{Status: "finished"}}, nil
}

func TestWaitForChaseRecordAtOffsetDoesNotReadAndDiscardHead(t *testing.T) {
	withFastChaseTimings(t)
	client := &fakeSeekChaseRecordClient{chunks: []string{"abc", "def"}}
	body, err := waitForChaseRecordAtOffset(context.Background(), client, "opaque-record-id", 188)
	if err != nil {
		t.Fatalf("waitForChaseRecordAtOffset() = %v, want success", err)
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatalf("reading resumed chase body: %v", err)
	}
	if string(data) != "abcdef" {
		t.Fatalf("resumed chase body = %q, want %q", data, "abcdef")
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.offsets) < 2 || client.offsets[0] != 188 || client.offsets[1] != 191 {
		t.Fatalf("Range offsets = %v, want first 188 then 191", client.offsets)
	}
}

type finalizingSeekChaseRecordClient struct {
	mu      sync.Mutex
	offsets []int64
	chunks  []string
}

func (c *finalizingSeekChaseRecordClient) StreamRecord(_ context.Context, recordID string, offset int64) (io.ReadCloser, int64, error) {
	if recordID != "opaque-record-id" {
		return nil, 0, errors.New("unexpected record id")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offsets = append(c.offsets, offset)
	if len(c.offsets) == 1 {
		return nil, 0, mirakc.ErrRangeNotSatisfiable
	}
	if len(c.chunks) == 0 {
		return nil, 0, mirakc.ErrRangeNotSatisfiable
	}
	chunk := c.chunks[0]
	c.chunks = c.chunks[1:]
	return io.NopCloser(strings.NewReader(chunk)), int64(len(chunk)), nil
}

func (c *finalizingSeekChaseRecordClient) StreamRecordFollow(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("follow endpoint must not be used for a seek")
}

func (c *finalizingSeekChaseRecordClient) GetRecord(context.Context, string) (*mirakc.Record, error) {
	return &mirakc.Record{Recording: mirakc.RecordInfo{Status: "finished"}}, nil
}

func TestChaseRangeFollowReaderDrainsDataAppendedBeforeRecordingFinished(t *testing.T) {
	withFastChaseTimings(t)
	client := &finalizingSeekChaseRecordClient{chunks: []string{"tail"}}
	reader := newChaseRangeFollowReader(context.Background(), client, "opaque-record-id", 188, nil)

	data, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatalf("reading final chase data: %v", err)
	}
	if string(data) != "tail" {
		t.Fatalf("final chase data = %q, want %q", data, "tail")
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.offsets) < 2 || client.offsets[0] != 188 || client.offsets[1] != 188 {
		t.Fatalf("final Range offsets = %v, want two attempts at 188", client.offsets)
	}
}

// withFastChaseTimings は追っかけの Range 追従の間隔と再試行の待ちを短くする。
func withFastChaseTimings(t *testing.T) {
	t.Helper()
	pollMin, pollMax := chaseRangePollMin, chaseRangePollMax
	retryBase, retryMax := chaseRetryBaseDelay, chaseRetryMaxDelay
	chaseRangePollMin, chaseRangePollMax = 5*time.Millisecond, 20*time.Millisecond
	chaseRetryBaseDelay, chaseRetryMaxDelay = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		chaseRangePollMin, chaseRangePollMax = pollMin, pollMax
		chaseRetryBaseDelay, chaseRetryMaxDelay = retryBase, retryMax
	})
}

// scriptedChaseRecord は録画中の mirakc record を模す。content のうち visible バイトまでが
// 書かれていて、StreamRecord は要求された offset から visible までを返す（offset が visible
// を超えたら違反として記録する）。追従配信は先頭から followBytes バイトを返し、followErr で
// 終わる（nil なら正常な EOF）。rangeErrs / statusErrs は応じる前に順に返す失敗。
// onStatus は失敗を返さなかった GetRecord のたびに呼ばれ、録画を進める（n は何回目か）。
type scriptedChaseRecord struct {
	mu          sync.Mutex
	content     string
	visible     int
	recording   bool
	gone        bool
	chunked     bool
	followBytes int
	followErr   error
	followGate  chan struct{}
	rangeErrs   []error
	statusErrs  []error
	onStatus    func(c *scriptedChaseRecord, n int)
	offsets     []int64
	violations  []int64
	statusCalls int
}

// errAfterReader は data を返した後に err で終わる本文（途中で切れた接続）。
type errAfterReader struct {
	data []byte
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (c *scriptedChaseRecord) StreamRecordFollow(context.Context, string) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data := []byte(c.content[:c.followBytes])
	var body io.Reader = bytes.NewReader(data)
	if c.followErr != nil {
		body = &errAfterReader{data: data, err: c.followErr}
	}
	if c.followGate != nil {
		body = &gatedReader{gate: c.followGate, r: body}
	}
	return io.NopCloser(body), nil
}

// gatedReader は gate が閉じるまで最初の Read を待たせる。
type gatedReader struct {
	gate <-chan struct{}
	r    io.Reader
}

func (g *gatedReader) Read(p []byte) (int, error) {
	<-g.gate
	return g.r.Read(p)
}

func (c *scriptedChaseRecord) StreamRecord(_ context.Context, _ string, offset int64) (io.ReadCloser, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offsets = append(c.offsets, offset)
	if len(c.rangeErrs) > 0 {
		err := c.rangeErrs[0]
		c.rangeErrs = c.rangeErrs[1:]
		return nil, 0, err
	}
	if c.gone {
		return nil, 0, &mirakc.APIError{StatusCode: http.StatusNotFound, Status: "404 Not Found"}
	}
	if offset > int64(c.visible) {
		c.violations = append(c.violations, offset)
		return nil, 0, mirakc.ErrRangeNotSatisfiable
	}
	data := c.content[offset:c.visible]
	if c.chunked {
		// Content-Length の無い本文（Go の http は ContentLength -1 を返す）。空でもそのまま返す。
		return io.NopCloser(strings.NewReader(data)), -1, nil
	}
	if len(data) == 0 {
		return nil, 0, mirakc.ErrRangeNotSatisfiable
	}
	return io.NopCloser(strings.NewReader(data)), int64(len(data)), nil
}

func (c *scriptedChaseRecord) GetRecord(context.Context, string) (*mirakc.Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.statusErrs) > 0 {
		err := c.statusErrs[0]
		c.statusErrs = c.statusErrs[1:]
		return nil, err
	}
	c.statusCalls++
	if c.onStatus != nil {
		c.onStatus(c, c.statusCalls)
	}
	if c.gone {
		return nil, &mirakc.APIError{StatusCode: http.StatusNotFound, Status: "404 Not Found"}
	}
	status := "finished"
	if c.recording {
		status = "recording"
	}
	return &mirakc.Record{Recording: mirakc.RecordInfo{Status: status}}, nil
}

func (c *scriptedChaseRecord) StreamService(context.Context, int64, int) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

// appendThenFinish は 1 回目の GetRecord（まだ録画中）で残りを追記し、2 回目で録画を終える。
func appendThenFinish(c *scriptedChaseRecord, n int) {
	switch n {
	case 1:
		c.visible = len(c.content)
	case 2:
		c.recording = false
	}
}

// readChaseInput は先頭からの追っかけの入力を読み切る。読み終わらなければ失敗にする。
func readChaseInput(t *testing.T, client *scriptedChaseRecord) (string, error) {
	t.Helper()
	body, err := followChaseRecord(context.Background(), client, "opaque-record-id")
	if err != nil {
		t.Fatalf("followChaseRecord() = %v, want success", err)
	}
	defer func() { _ = body.Close() }()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(body)
		done <- result{data, err}
	}()
	select {
	case r := <-done:
		return string(r.data), r.err
	case <-time.After(5 * time.Second):
		t.Fatal("chase input did not end within 5s")
		return "", nil
	}
}

// assertOffsetsAdvance は Range の offset が追従配信の続き（4 バイト目）から始まり、戻らず、
// 書かれた範囲を超えていないことを見る。
func assertOffsetsAdvance(t *testing.T, client *scriptedChaseRecord) {
	t.Helper()
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.violations) > 0 {
		t.Fatalf("Range offsets beyond the written size: %v (all %v)", client.violations, client.offsets)
	}
	if len(client.offsets) == 0 || client.offsets[0] != 4 {
		t.Fatalf("Range offsets = %v, want the first one at 4 (the byte after the follow stream)", client.offsets)
	}
	for i := 1; i < len(client.offsets); i++ {
		if client.offsets[i] < client.offsets[i-1] {
			t.Fatalf("Range offsets went backwards: %v", client.offsets)
		}
	}
}

// TestFollowChaseRecordContinuesWithRangeAfterFollowCloses は、先頭からの追っかけで mirakc の
// 追従配信が録画中に閉じても、ffmpeg の入力を EOF にしない（= playlist に ENDLIST を付けない）
// ことを固定する。ChasePlaylistForTarget を通り、偽 ffmpeg は stdin を全部ファイルへ書き、
// EOF で ENDLIST 付きの playlist を書く。
func TestFollowChaseRecordContinuesWithRangeAfterFollowCloses(t *testing.T) {
	withFastChaseTimings(t)
	dir := t.TempDir()
	ffmpeg := filepath.Join(dir, "fake-ffmpeg-chase-stdin")
	script := `#!/bin/sh
playlist=""
for a in "$@"; do case "$a" in *.m3u8) playlist="$a";; esac; done
outdir=$(dirname "$playlist")
cat > "$outdir/input.bin"
printf '#EXTM3U\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nsegments/x.ts\n#EXT-X-ENDLIST\n' > "$playlist"
exit 0
`
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := LiveConfig{
		Enabled:     true,
		FFmpeg:      ffmpeg,
		SegmentDir:  t.TempDir(),
		MaxSessions: 4,
		IdleTimeout: time.Minute,
		Profiles:    []LiveProfile{{Name: "hd", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6}},
	}
	client := &scriptedChaseRecord{content: "headtail", visible: 4, followBytes: 4, recording: true, onStatus: appendThenFinish}
	ls := newLiveStreamer(client, cfg)
	t.Cleanup(ls.shutdown)
	target := ChaseTarget{
		RecordingID:     42,
		Site:            "default",
		RecordID:        "record-42",
		Status:          "recording",
		RecordingStatus: "recording",
	}
	req := httptest.NewRequest(http.MethodGet, "/api/sites/default/recordings/42/chase/playlist.m3u8?profile=hd", nil)
	resp := httptest.NewRecorder()
	ls.ChasePlaylistForTarget(resp, req, target)
	if resp.Code != http.StatusOK {
		t.Fatalf("playlist status = %d, want 200 (%s)", resp.Code, resp.Body.String())
	}
	ls.mu.Lock()
	s := ls.chaseSessions[chaseSessionKeyFor(42, 0)]
	ls.mu.Unlock()
	if s == nil {
		t.Fatal("completed chase session was not retained")
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "input.bin"))
	if err != nil {
		t.Fatal(err)
	}
	// 追従が閉じた時点で EOF にすると "head" で終わり、録画中の playlist に ENDLIST が付く。
	if string(data) != "headtail" {
		t.Fatalf("ffmpeg input = %q, want %q", data, "headtail")
	}
	assertOffsetsAdvance(t, client)
}

// TestChasePlaylistRequiresSeekClientAtHead は、先頭からの追っかけでも Range の続きを読める
// クライアントを要求することを固定する（読めないなら追従配信だけで黙って続けない）。
func TestChasePlaylistRequiresSeekClientAtHead(t *testing.T) {
	cfg := LiveConfig{
		Enabled:     true,
		FFmpeg:      installCompletedChaseFFmpeg(t),
		SegmentDir:  t.TempDir(),
		MaxSessions: 4,
		IdleTimeout: time.Minute,
		Profiles:    []LiveProfile{{Name: "hd", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6}},
	}
	ls := newLiveStreamer(followOnlyChaseRecordClient{}, cfg)
	t.Cleanup(ls.shutdown)
	req := httptest.NewRequest(http.MethodGet, "/api/sites/default/recordings/42/chase/playlist.m3u8?profile=hd", nil)
	resp := httptest.NewRecorder()
	ls.ChasePlaylistForTarget(resp, req, ChaseTarget{
		RecordingID: 42, Site: "default", RecordID: "record-42", Status: "recording", RecordingStatus: "recording",
	})
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("playlist status = %d, want 503 for a client without Range", resp.Code)
	}
}

// followOnlyChaseRecordClient は追従配信しか持たないクライアント。
type followOnlyChaseRecordClient struct{}

func (followOnlyChaseRecordClient) StreamRecordFollow(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("head")), nil
}

func (followOnlyChaseRecordClient) StreamService(context.Context, int64, int) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

// TestChaseRangeFollowReaderEndsCleanlyWhenRecordIsPurged は、録画の終わりに ingest が record を
// purge して 404 になっても、それを録画の終端（EOF）として扱うことを固定する。エラーにすると
// 正常に終わった追っかけがセッションごと捨てられる。
func TestChaseRangeFollowReaderEndsCleanlyWhenRecordIsPurged(t *testing.T) {
	withFastChaseTimings(t)
	notFound := &mirakc.APIError{StatusCode: http.StatusNotFound, Status: "404 Not Found"}
	tests := []struct {
		name   string
		client *scriptedChaseRecord
	}{
		// 追従配信が最後まで渡して閉じた直後に、Range がもう 404。
		{name: "range 404", client: &scriptedChaseRecord{content: "headtail", visible: 8, followBytes: 8, gone: true}},
		// Range は追い付いた（416）が、その後の GetRecord が 404。
		{name: "status 404", client: &scriptedChaseRecord{
			content: "headtail", visible: 8, followBytes: 8, recording: true,
			statusErrs: []error{notFound},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := readChaseInput(t, tt.client)
			if err != nil {
				t.Fatalf("chase input ended with %v, want a clean EOF after the purge", err)
			}
			if data != "headtail" {
				t.Fatalf("chase input = %q, want %q", data, "headtail")
			}
		})
	}
}

// TestChaseRangeFollowReaderRetriesTransientFailures は、Range と GetRecord の一過性の失敗
// （5xx・通信断）を再試行して、録画の最後まで読むことを固定する。
func TestChaseRangeFollowReaderRetriesTransientFailures(t *testing.T) {
	withFastChaseTimings(t)
	client := &scriptedChaseRecord{
		content: "headtail", visible: 4, followBytes: 4, recording: true, onStatus: appendThenFinish,
		rangeErrs: []error{
			&mirakc.APIError{StatusCode: http.StatusBadGateway, Status: "502 Bad Gateway"},
			fmt.Errorf("sending request: %w", syscall.ECONNRESET),
		},
		statusErrs: []error{&mirakc.APIError{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable"}},
	}
	data, err := readChaseInput(t, client)
	if err != nil {
		t.Fatalf("chase input ended with %v, want transient failures to be retried", err)
	}
	if data != "headtail" {
		t.Fatalf("chase input = %q, want %q", data, "headtail")
	}
	assertOffsetsAdvance(t, client)
}

// TestChaseRangeFollowReaderGivesUp は、一過性の失敗が上限を超えて続くか、再試行しても
// 変わらない失敗（4xx）なら EOF ではなくエラーで終えることを固定する（EOF にすると途中までの
// 入力に ENDLIST が付く）。
func TestChaseRangeFollowReaderGivesUp(t *testing.T) {
	withFastChaseTimings(t)
	unavailable := &mirakc.APIError{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable"}
	repeated := make([]error, chaseMaxConsecutiveFailures+1)
	for i := range repeated {
		repeated[i] = unavailable
	}
	tests := []struct {
		name      string
		rangeErrs []error
	}{
		{name: "transient beyond the budget", rangeErrs: repeated},
		{name: "permanent", rangeErrs: []error{&mirakc.APIError{StatusCode: http.StatusBadRequest, Status: "400 Bad Request"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &scriptedChaseRecord{content: "headtail", visible: 4, followBytes: 4, recording: true, rangeErrs: tt.rangeErrs}
			data, err := readChaseInput(t, client)
			if err == nil {
				t.Fatalf("chase input ended cleanly with %q, want an error", data)
			}
			if data != "head" {
				t.Fatalf("chase input before the error = %q, want %q", data, "head")
			}
		})
	}
}

// TestChaseRangeFollowReaderResumesAfterUncleanBodyClose は、追従配信や Range の本文が途中で
// 切れても（ErrUnexpectedEOF・接続リセット）、読んだ位置から Range で続けることを固定する。
func TestChaseRangeFollowReaderResumesAfterUncleanBodyClose(t *testing.T) {
	withFastChaseTimings(t)
	for _, followErr := range []error{io.ErrUnexpectedEOF, syscall.ECONNRESET} {
		t.Run(followErr.Error(), func(t *testing.T) {
			client := &scriptedChaseRecord{
				content: "headtail", visible: 4, followBytes: 4, followErr: followErr,
				recording: true, onStatus: appendThenFinish,
			}
			data, err := readChaseInput(t, client)
			if err != nil {
				t.Fatalf("chase input ended with %v, want the Range continuation", err)
			}
			if data != "headtail" {
				t.Fatalf("chase input = %q, want %q", data, "headtail")
			}
			assertOffsetsAdvance(t, client)
		})
	}
}

// TestChaseRangeFollowReaderReadsChunkedRangeBodies は、Content-Length の無い Range 本文
// （ContentLength -1）を空と見なさず読み、何も返さずに終わった本文だけを追い付いたと扱う
// ことを固定する。
func TestChaseRangeFollowReaderReadsChunkedRangeBodies(t *testing.T) {
	withFastChaseTimings(t)
	client := &scriptedChaseRecord{
		content: "headtail", visible: 4, followBytes: 4, recording: true, chunked: true, onStatus: appendThenFinish,
	}
	data, err := readChaseInput(t, client)
	if err != nil {
		t.Fatalf("chase input ended with %v", err)
	}
	if data != "headtail" {
		t.Fatalf("chase input = %q, want %q", data, "headtail")
	}
	assertOffsetsAdvance(t, client)
}

// TestChaseRangeFollowReaderBacksOffWhileCaughtUp は、録画中に追い付いたままのとき要求の
// 間隔を広げることを固定する。固定間隔だと、空の Range と GetRecord を毎回続ける。
func TestChaseRangeFollowReaderBacksOffWhileCaughtUp(t *testing.T) {
	withFastChaseTimings(t)
	chaseRangePollMin, chaseRangePollMax = 10*time.Millisecond, 80*time.Millisecond
	const window = 600 * time.Millisecond
	started := time.Now()
	client := &scriptedChaseRecord{
		content: "head", visible: 4, followBytes: 4, recording: true,
		onStatus: func(c *scriptedChaseRecord, _ int) {
			if time.Since(started) > window {
				c.recording = false
			}
		},
	}
	data, err := readChaseInput(t, client)
	if err != nil || data != "head" {
		t.Fatalf("chase input = %q, %v", data, err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	// 固定 10ms なら 600ms で約 60 回。10, 20, 40, 80, 80, ... なら 10 回前後。
	if len(client.offsets) > 20 || client.statusCalls > 20 {
		t.Fatalf("while caught up for %v: %d Range requests and %d GetRecord calls, want at most 20 each",
			window, len(client.offsets), client.statusCalls)
	}
}

// TestChaseInputErrorDoesNotWriteEndlist は、追っかけの入力がエラーで終わったら ffmpeg が
// ENDLIST を書く前に止めることを固定する。偽 ffmpeg は playlist を先に書き、stdin の EOF を
// 見たら ENDLIST を書いたことを印のファイルに残す。
func TestChaseInputErrorDoesNotWriteEndlist(t *testing.T) {
	withFastChaseTimings(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "endlist-written")
	ffmpeg := filepath.Join(dir, "fake-ffmpeg-chase-endlist")
	script := `#!/bin/sh
playlist=""
for a in "$@"; do case "$a" in *.m3u8) playlist="$a";; esac; done
printf '#EXTM3U\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nsegments/x.ts\n' > "$playlist"
cat > /dev/null
echo '#EXT-X-ENDLIST' >> "$playlist"
touch "` + marker + `"
exit 0
`
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := LiveConfig{
		Enabled:     true,
		FFmpeg:      ffmpeg,
		SegmentDir:  t.TempDir(),
		MaxSessions: 4,
		IdleTimeout: time.Minute,
		Profiles:    []LiveProfile{{Name: "hd", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6}},
	}
	// 追従配信が閉じた後の Range が、再試行しても変わらない 400 で失敗する。
	// 入力は playlist を返した後に流す（先に失敗すると playlist ができる前にセッションが消える）。
	gate := make(chan struct{})
	client := &scriptedChaseRecord{
		content: "head", visible: 4, followBytes: 4, recording: true, followGate: gate,
		rangeErrs: []error{&mirakc.APIError{StatusCode: http.StatusBadRequest, Status: "400 Bad Request"}},
	}
	ls := newLiveStreamer(client, cfg)
	t.Cleanup(ls.shutdown)
	req := httptest.NewRequest(http.MethodGet, "/api/sites/default/recordings/42/chase/playlist.m3u8?profile=hd", nil)
	resp := httptest.NewRecorder()
	ls.ChasePlaylistForTarget(resp, req, ChaseTarget{
		RecordingID: 42, Site: "default", RecordID: "record-42", Status: "recording", RecordingStatus: "recording",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("playlist status = %d, want 200 (%s)", resp.Code, resp.Body.String())
	}
	ls.mu.Lock()
	s := ls.chaseSessions[chaseSessionKeyFor(42, 0)]
	ls.mu.Unlock()
	if s == nil {
		t.Fatal("chase session disappeared before the input failed")
	}
	close(gate)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("chase session did not end after the input failed")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ffmpeg saw EOF and wrote ENDLIST after the chase input failed")
	}
	ls.mu.Lock()
	_, retained := ls.chaseSessions[chaseSessionKeyFor(42, 0)]
	ls.mu.Unlock()
	if retained {
		t.Fatal("a chase session whose input failed was retained")
	}
}

func TestWaitForChaseRecordRetriesNotReady(t *testing.T) {
	withPlaylistStartupTimeout(t, 350*time.Millisecond)
	client := &fakeChaseRecordClient{
		errors: []error{mirakc.ErrRecordNotReady, mirakc.ErrRecordNotReady, nil},
	}

	body, err := waitForChaseRecord(context.Background(), client, "opaque-record-id")
	if err != nil {
		t.Fatalf("waitForChaseRecord() = %v, want success after 204 retries", err)
	}
	defer func() { _ = body.Close() }()
	if got, want := client.callCount(), 3; got != want {
		t.Fatalf("StreamRecordFollow calls = %d, want %d", got, want)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if got := strings.Join(client.recordIDs, ","); got != "opaque-record-id,opaque-record-id,opaque-record-id" {
		t.Errorf("record ids = %q, want the same opaque mirakc id on every retry", got)
	}
}

func TestWaitForChaseRecordNotReadyTimesOutWithoutUpstreamError(t *testing.T) {
	withPlaylistStartupTimeout(t, 220*time.Millisecond)
	client := &fakeChaseRecordClient{errors: []error{
		mirakc.ErrRecordNotReady,
		mirakc.ErrRecordNotReady,
		mirakc.ErrRecordNotReady,
		mirakc.ErrRecordNotReady,
		mirakc.ErrRecordNotReady,
		mirakc.ErrRecordNotReady,
	}}

	start := time.Now()
	_, err := waitForChaseRecord(context.Background(), client, "record-still-empty")
	if !errors.Is(err, errChaseRecordNotReadyTimeout) {
		t.Fatalf("waitForChaseRecord() = %v, want errChaseRecordNotReadyTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waitForChaseRecord() took %v, want it bounded by startup timeout", elapsed)
	}
	if got := client.callCount(); got < 2 {
		t.Errorf("StreamRecordFollow calls = %d, want repeated 204 polling", got)
	}
}

type blockingChaseRecordClient struct{}

func (blockingChaseRecordClient) StreamRecordFollow(ctx context.Context, _ string) (io.ReadCloser, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestWaitForChaseRecordBoundsBlockedResponse(t *testing.T) {
	withPlaylistStartupTimeout(t, 40*time.Millisecond)
	start := time.Now()
	_, err := waitForChaseRecord(context.Background(), blockingChaseRecordClient{}, "blocked")
	if !errors.Is(err, errChaseRecordNotReadyTimeout) {
		t.Fatalf("waitForChaseRecord() = %v, want errChaseRecordNotReadyTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("waitForChaseRecord() took %v, want bounded response wait", elapsed)
	}
}

func TestBuildChaseFFmpegArgsUsesGrowingEventPlaylist(t *testing.T) {
	cfg := LiveConfig{
		Profiles: []LiveProfile{{
			Name:           "h264",
			VideoCodec:     "libx264",
			AudioCodec:     "aac",
			SegmentSeconds: 2,
			PlaylistSize:   6,
		}},
	}

	args := BuildChaseFFmpegArgs(cfg, "/tmp/segments/default/chase/42", false)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-hls_playlist_type event") {
		t.Fatalf("args = %q, want EVENT playlist", joined)
	}
	if !strings.Contains(joined, "-hls_list_size 0") {
		t.Fatalf("args = %q, want an unbounded playlist", joined)
	}
	if !strings.Contains(joined, "-hls_flags temp_file") {
		t.Fatalf("args = %q, want atomic playlist/segment writes", joined)
	}
	if strings.Contains(joined, "delete_segments") {
		t.Fatalf("args = %q, chase output must retain all segments", joined)
	}

	liveArgs := BuildLiveFFmpegArgs(cfg, "/tmp/segments/default/1", false)
	liveJoined := strings.Join(liveArgs, " ")
	if strings.Contains(liveJoined, "-hls_playlist_type event") || !strings.Contains(liveJoined, "delete_segments") {
		t.Fatalf("live args = %q, want the existing sliding live playlist", liveJoined)
	}
}

func TestBuildOriginalVODFFmpegArgsRetainsSeekableVODOutput(t *testing.T) {
	profiles := []LiveProfile{
		{Name: "hd", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
		{Name: "sd", VideoCodec: "libx264", AudioCodec: "aac", Height: 480, SegmentSeconds: 2, PlaylistSize: 6},
	}
	for _, tc := range []struct {
		name     string
		captions bool
		withSubs bool
	}{
		{name: "audio renditions", captions: false},
		{name: "captions and subtitles", captions: true, withSubs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := LiveConfig{Captions: tc.captions, Profiles: profiles}
			args := BuildOriginalVODFFmpegArgs(cfg, "/tmp/original-vod", tc.withSubs, 0)
			joined := strings.Join(args, " ")
			for _, want := range []string{"-hls_playlist_type event", "-hls_list_size 0", "-hls_flags temp_file", "-hls_base_url segments/"} {
				if !strings.Contains(joined, want) {
					t.Errorf("args = %q, want %q", joined, want)
				}
			}
			if strings.Contains(joined, "-hls_playlist_type vod") {
				t.Errorf("args = %q, vod writes no playlist until ffmpeg exits", joined)
			}
			if strings.Contains(joined, "delete_segments") {
				t.Errorf("args = %q, VOD segments must be retained until idle GC", joined)
			}
			if !tc.captions && !strings.Contains(joined, "a:0,agroup:aud") {
				t.Errorf("args = %q, want the established audio rendition map", joined)
			}
			if tc.withSubs && !strings.Contains(joined, "-map 0:s:0?") {
				t.Errorf("args = %q, want optional subtitle stream mapping", joined)
			}
			i := slices.Index(args, "-i")
			if i < 0 || i+1 >= len(args) || args[i+1] != originalVODFFmpegInputPath {
				t.Errorf("input args = %q, want seekable inherited fd %s", args, originalVODFFmpegInputPath)
			}
			if slices.Contains(args, "-ss") {
				t.Errorf("zero-offset args = %q, want no explicit -ss", args)
			}

			seekArgs := BuildOriginalVODFFmpegArgs(cfg, "/tmp/original-vod", tc.withSubs, 120)
			ss := slices.Index(seekArgs, "-ss")
			seekInput := slices.Index(seekArgs, "-i")
			if ss < 0 || ss+1 >= len(seekArgs) || seekArgs[ss+1] != "120" || seekInput <= ss {
				t.Errorf("offset args = %q, want input-side -ss 120 before -i", seekArgs)
			}
		})
	}
}

func TestFinishedChaseServesRetainedPlaylistWithoutRestarting(t *testing.T) {
	dir := t.TempDir()
	playlist := filepath.Join(dir, "h264.m3u8")
	if err := os.WriteFile(playlist, []byte("#EXTM3U\n#EXTINF:2.0,\nsegments/00001.ts\n#EXT-X-ENDLIST\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{})
	close(ready)
	done := make(chan struct{})
	close(done)
	ls := &LiveStreamer{
		mirakc: chaseTestLiveClient{},
		site:   "default",
		cfg: LiveConfig{Profiles: []LiveProfile{{
			Name:           "h264",
			VideoCodec:     "libx264",
			AudioCodec:     "aac",
			SegmentSeconds: 2,
			PlaylistSize:   6,
		}}},
		chaseSessions: map[sessionKey]*liveSession{
			{kind: chaseSessionKind, id: 42}: {
				key:    sessionKey{kind: chaseSessionKind, id: 42},
				dir:    dir,
				ready:  ready,
				done:   done,
				cancel: func() {},
			},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sites/default/recordings/42/chase/playlist.m3u8?profile=h264", nil)
	resp := httptest.NewRecorder()
	ls.ChasePlaylistForTarget(resp, req, ChaseTarget{
		RecordingID:     42,
		Site:            "default",
		RecordID:        "record-42",
		Status:          "finished",
		RecordingStatus: "finished",
	})

	if resp.Code != http.StatusOK {
		t.Fatalf("finished chase playlist status = %d, want 200", resp.Code)
	}
	if got := resp.Body.String(); !strings.Contains(got, "#EXT-X-ENDLIST") {
		t.Fatalf("finished chase playlist = %q, want retained ENDLIST", got)
	}

	delete(ls.chaseSessions, chaseSessionKeyFor(42, 0))
	resp = httptest.NewRecorder()
	ls.ChasePlaylistForTarget(resp, req, ChaseTarget{
		RecordingID:     42,
		Site:            "default",
		RecordID:        "record-42",
		Status:          "finished",
		RecordingStatus: "finished",
	})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("finished chase without retained session status = %d, want 404", resp.Code)
	}
}

func TestLiveAndChaseShareMaxSessions(t *testing.T) {
	readyLive := make(chan struct{})
	readyChase := make(chan struct{})
	close(readyLive)
	close(readyChase)
	ls := &LiveStreamer{
		cfg: LiveConfig{MaxSessions: 2},
		sessions: map[int64]*liveSession{
			1: {key: sessionKey{kind: liveSessionKind, id: 1}, serviceID: 1, ready: readyLive},
		},
		chaseSessions: map[sessionKey]*liveSession{
			{kind: chaseSessionKind, id: 42}: {key: sessionKey{kind: chaseSessionKind, id: 42}, ready: readyChase},
		},
	}

	_, err := ls.getOrCreateSessionOnceFor(context.Background(), sessionKey{
		kind: chaseSessionKind,
		id:   99,
	}, func(context.Context) (io.ReadCloser, error) {
		t.Fatal("session source called despite the combined session limit")
		return nil, nil
	})
	if !errors.Is(err, errSessionLimit) {
		t.Fatalf("getOrCreateSessionOnceFor() = %v, want errSessionLimit", err)
	}
}

type chaseTestLiveClient struct{}

func (chaseTestLiveClient) StreamService(context.Context, int64, int) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

func installCompletedChaseFFmpeg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ffmpeg-chase-complete")
	script := `#!/bin/sh
segfile=""
playlist=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-hls_segment_filename" ]; then segfile="$a"; fi
  case "$a" in *.m3u8) playlist="$a";; esac
  prev="$a"
done
outdir=$(dirname "$playlist")
mkdir -p "$outdir/segments"
seg=$(printf '%s' "$segfile" | sed 's/%05d/00001/')
printf 'fake-ts' > "$seg"
cat > "$playlist" <<EOF
#EXTM3U
#EXT-X-PLAYLIST-TYPE:EVENT
#EXT-X-TARGETDURATION:2
#EXTINF:2.0,
segments/$(basename "$seg")
#EXT-X-ENDLIST
EOF
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// installMultiProfileChaseFFmpeg は渡された出力パス（プロファイルごとの
// `NAME.m3u8`）のそれぞれへ EVENT playlist を書く偽 ffmpeg。**1 本の ffmpeg が
// 全プロファイルを同時に出力する**形（BuildChaseFFmpegArgs の追っかけ経路）を模す。
// installCompletedChaseFFmpeg は 1 本の playlist しか書かないので、画質の切替を
// 見るにはこちらが要る。
//
// finished=false は **ENDLIST を書かず、書いた後も生き続ける。** 録画中の追っかけ
// （ffmpeg が走っている間）のセッション再利用を見るためである。exit 0 で終わると
// 2 本目の要求は終了後の保持経路（keepCompletedChase）に当たり、本題の
// 経路を通らない。`exec` にするのは shutdown の kill を sleep へ直接届けるため。
// finished=true は全プレイリストに ENDLIST を書いて exit 0 する（録画終了後の経路）。
func installMultiProfileChaseFFmpeg(t *testing.T, finished bool) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ffmpeg-chase-profiles")
	script := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    *.m3u8)
      base=$(basename "$a" .m3u8)
      outdir=$(dirname "$a")
      mkdir -p "$outdir/segments"
      printf 'fake-ts' > "$outdir/segments/${base}_seg00001.ts"
      {
        echo '#EXTM3U'
        echo '#EXT-X-PLAYLIST-TYPE:EVENT'
        echo '#EXT-X-TARGETDURATION:2'
        echo '#EXTINF:2.0,'
        echo "segments/${base}_seg00001.ts"
        if [ -n "$FAKE_ENDLIST" ]; then echo '#EXT-X-ENDLIST'; fi
      } > "$a"
      ;;
  esac
done
if [ -n "$FAKE_ENDLIST" ]; then exit 0; fi
exec sleep 30
`
	if finished {
		script = strings.Replace(script, "#!/bin/sh\n", "#!/bin/sh\nFAKE_ENDLIST=1\n", 1)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestChaseProfileSwitchReusesOneSession は画質（プロファイル）の切替が
// 追っかけのセッションを作り直さないことを固定する（issue #874）。
//
// セッション鍵は `(recordingID, offset)` でプロファイルを含まないので、同じ録画・
// 同じ offset の別プロファイルは同じセッションの別プレイリストになる。ここが
// 崩れると、フロントが `LivePlayer` を作り直さない以上、画質の切替のたびに
// チューナー + ffmpeg がサーバー側だけで 1 本ずつ増える。
//
// **壊し方**: `chaseSessionKeyFor` の鍵に profile を混ぜる（2 本目の要求で
// セッションが増え、mirakc への録画 stream 要求も 2 件になる）。
func TestChaseProfileSwitchReusesOneSession(t *testing.T) {
	cfg := LiveConfig{
		Enabled:     true,
		FFmpeg:      installMultiProfileChaseFFmpeg(t, false),
		SegmentDir:  t.TempDir(),
		MaxSessions: 4,
		IdleTimeout: time.Minute,
		Profiles: []LiveProfile{
			{Name: "hd", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
			{Name: "sd", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
		},
	}
	client := &fakeChaseRecordClient{}
	ls := newLiveStreamer(client, cfg)
	t.Cleanup(ls.shutdown)
	target := ChaseTarget{
		RecordingID:     42,
		Site:            "default",
		RecordID:        "record-42",
		Status:          "recording",
		RecordingStatus: "recording",
	}
	fetch := func(profile string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet,
			"/api/sites/default/recordings/42/chase/playlist.m3u8?profile="+profile, nil)
		resp := httptest.NewRecorder()
		ls.ChasePlaylistForTarget(resp, req, target)
		return resp
	}

	if resp := fetch("hd"); resp.Code != http.StatusOK {
		t.Fatalf("hd playlist status = %d, want 200 (%s)", resp.Code, resp.Body.String())
	}
	key := chaseSessionKeyFor(42, 0)
	ls.mu.Lock()
	first := ls.chaseSessions[key]
	ls.mu.Unlock()
	if first == nil {
		t.Fatal("chase session was not created")
	}

	if resp := fetch("sd"); resp.Code != http.StatusOK {
		t.Fatalf("sd playlist status = %d, want 200 (%s)", resp.Code, resp.Body.String())
	}
	ls.mu.Lock()
	after := ls.chaseSessions[key]
	sessions := len(ls.chaseSessions)
	ls.mu.Unlock()
	if after != first {
		t.Fatal("画質の切替で追っかけのセッションが作り直された")
	}
	if sessions != 1 {
		t.Fatalf("chase sessions after the profile switch = %d, want 1", sessions)
	}
	// セッションが作り直されていなければ、mirakc への録画 stream 要求も 1 件のまま
	if got := client.callCount(); got != 1 {
		t.Fatalf("mirakc record stream calls = %d, want 1", got)
	}

	// 一覧に無い名前はセッションを起こす前に 400（フロントは先に落とすが、
	// 直リンク・手書き URL の受け皿として要る）。
	if resp := fetch("does-not-exist"); resp.Code != http.StatusBadRequest {
		t.Fatalf("unknown profile status = %d, want 400", resp.Code)
	}
	ls.mu.Lock()
	sessions = len(ls.chaseSessions)
	ls.mu.Unlock()
	if sessions != 1 {
		t.Fatalf("unknown profile created a session (sessions = %d)", sessions)
	}
}

// TestFinishedChaseProfileSwitchServesRetainedPlaylists は録画終了後（ENDLIST 済み・
// ffmpeg 終了済み）の追っかけでも、idle GC までは画質を切り替えられることを固定する
// （issue #874 の罠「確かめずに『切り替えられます』と書かない」）。保持ディレクトリには
// 全プロファイルのプレイリストが残るので、別プロファイルは再起動なしで 200 になる。
// GC 後は現行の追っかけと同じ 404 である。
//
// **壊し方**: 偽 ffmpeg が先頭プロファイルのプレイリストしか書かない（= 保持が
// 1 プロファイル分だけ）と sd の要求が 504 で落ちる（実測）。
func TestFinishedChaseProfileSwitchServesRetainedPlaylists(t *testing.T) {
	cfg := LiveConfig{
		Enabled:     true,
		FFmpeg:      installMultiProfileChaseFFmpeg(t, true),
		SegmentDir:  t.TempDir(),
		MaxSessions: 4,
		IdleTimeout: time.Minute,
		Profiles: []LiveProfile{
			{Name: "hd", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
			{Name: "sd", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
		},
	}
	client := &fakeChaseRecordClient{}
	ls := newLiveStreamer(client, cfg)
	t.Cleanup(ls.shutdown)
	fetch := func(profile, status string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet,
			"/api/sites/default/recordings/42/chase/playlist.m3u8?profile="+profile, nil)
		resp := httptest.NewRecorder()
		ls.ChasePlaylistForTarget(resp, req, ChaseTarget{
			RecordingID:     42,
			Site:            "default",
			RecordID:        "record-42",
			Status:          status,
			RecordingStatus: status,
		})
		return resp
	}

	if resp := fetch("hd", "recording"); resp.Code != http.StatusOK {
		t.Fatalf("hd playlist status = %d, want 200 (%s)", resp.Code, resp.Body.String())
	}
	key := chaseSessionKeyFor(42, 0)
	ls.mu.Lock()
	s := ls.chaseSessions[key]
	ls.mu.Unlock()
	if s == nil {
		t.Fatal("chase session was not created")
	}
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("finished chase ffmpeg did not exit")
	}

	resp := fetch("sd", "finished")
	if resp.Code != http.StatusOK {
		t.Fatalf("sd playlist after ENDLIST status = %d, want 200 (%s)", resp.Code, resp.Body.String())
	}
	if got := resp.Body.String(); !strings.Contains(got, "sd_seg00001.ts") || !strings.Contains(got, "#EXT-X-ENDLIST") {
		t.Fatalf("sd playlist after ENDLIST = %q, want retained sd playlist with ENDLIST", got)
	}
	if got := client.callCount(); got != 1 {
		t.Fatalf("mirakc record stream calls = %d, want 1 (switch must not restart)", got)
	}

	s.mu.Lock()
	s.lastAccess = time.Now().Add(-2 * time.Minute)
	s.mu.Unlock()
	ls.reapIdleAt(time.Now())
	if resp := fetch("hd", "finished"); resp.Code != http.StatusNotFound {
		t.Fatalf("playlist after idle GC status = %d, want 404", resp.Code)
	}
}

func installFailedChaseFFmpeg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ffmpeg-chase-fail")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompletedChaseRetainsEventFilesUntilIdleGC(t *testing.T) {
	cfg := LiveConfig{
		Enabled:     true,
		FFmpeg:      installCompletedChaseFFmpeg(t),
		SegmentDir:  t.TempDir(),
		MaxSessions: 2,
		IdleTimeout: time.Second,
		Profiles: []LiveProfile{{
			Name:           "h264",
			VideoCodec:     "libx264",
			AudioCodec:     "aac",
			SegmentSeconds: 2,
			PlaylistSize:   6,
		}},
	}
	ls := newLiveStreamer(chaseTestLiveClient{}, cfg)
	targetID := int64(42)
	s, err := ls.getOrCreateSessionFor(context.Background(), sessionKey{
		kind: chaseSessionKind,
		id:   targetID,
	}, func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("input")), nil
	})
	if err != nil {
		t.Fatalf("starting chase session: %v", err)
	}
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("completed chase ffmpeg did not exit")
	}

	playlist := filepath.Join(chaseSessionDir(cfg.SegmentDir, "default", targetID, 0), "h264.m3u8")
	data, err := os.ReadFile(playlist)
	if err != nil {
		t.Fatalf("reading retained EVENT playlist: %v", err)
	}
	if !strings.Contains(string(data), "#EXT-X-ENDLIST") {
		t.Fatalf("retained playlist = %q, want ENDLIST", data)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(playlist), "segments", "h264_seg00001.ts")); err != nil {
		t.Fatalf("retained segment: %v", err)
	}

	ls.mu.Lock()
	_, present := ls.chaseSessions[chaseSessionKeyFor(targetID, 0)]
	ls.mu.Unlock()
	if !present {
		t.Fatal("completed chase session was removed before idle GC")
	}

	s.mu.Lock()
	s.lastAccess = time.Now().Add(-2 * time.Second)
	s.mu.Unlock()
	ls.reapIdleAt(time.Now())
	if _, err := os.Stat(filepath.Dir(playlist)); !os.IsNotExist(err) {
		t.Errorf("chase session directory still exists after idle GC, stat err = %v", err)
	}
	if ls.sessionCount() != 0 {
		t.Errorf("sessionCount after idle GC = %d, want 0", ls.sessionCount())
	}
}

func TestChaseSessionCleanupDoesNotRemoveSiblingOffsets(t *testing.T) {
	segmentDir := t.TempDir()
	zeroDir := chaseSessionDir(segmentDir, "default", 42, 0)
	offsetDir := chaseSessionDir(segmentDir, "default", 42, 30)
	if filepath.Dir(zeroDir) != filepath.Dir(offsetDir) {
		t.Fatalf("chase session directories are not siblings: %q vs %q", zeroDir, offsetDir)
	}
	if err := os.MkdirAll(filepath.Join(zeroDir, "segments"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(offsetDir, "segments", "keep.ts")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	cleanupSessionDir(&liveSession{
		key: sessionKey{kind: chaseSessionKind, id: 42, offsetSeconds: 0},
		dir: zeroDir,
	})

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("sibling offset files were removed: %v", err)
	}
	if _, err := os.Stat(zeroDir); !os.IsNotExist(err) {
		t.Fatalf("offset 0 directory still exists after cleanup, stat err = %v", err)
	}
}

func TestFailedChaseIsRemovedAndCanRestart(t *testing.T) {
	cfg := LiveConfig{
		Enabled:     true,
		FFmpeg:      installFailedChaseFFmpeg(t),
		SegmentDir:  t.TempDir(),
		MaxSessions: 1,
		IdleTimeout: time.Minute,
		Profiles: []LiveProfile{{
			Name:           "h264",
			VideoCodec:     "libx264",
			AudioCodec:     "aac",
			SegmentSeconds: 2,
			PlaylistSize:   6,
		}},
	}
	ls := newLiveStreamer(chaseTestLiveClient{}, cfg)
	t.Cleanup(ls.shutdown)

	start := func() *liveSession {
		s, err := ls.getOrCreateSessionFor(context.Background(), sessionKey{
			kind: chaseSessionKind,
			id:   43,
		}, func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("input")), nil
		})
		if err != nil {
			t.Fatalf("starting chase session: %v", err)
		}
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			t.Fatal("failed chase ffmpeg did not exit")
		}
		return s
	}

	first := start()
	ls.mu.Lock()
	_, present := ls.chaseSessions[first.key]
	ls.mu.Unlock()
	if present {
		t.Fatal("failed chase session remained in the session map")
	}
	if _, err := os.Stat(first.dir); !os.IsNotExist(err) {
		t.Fatalf("failed chase session directory still exists, stat err = %v", err)
	}

	second := start()
	if second == first {
		t.Fatal("restart reused the failed chase session")
	}
}

func TestEvictingCompletedChaseCleansRetainedFiles(t *testing.T) {
	setShortLiveMirakcReleaseWait(t, 0)
	cfg := LiveConfig{
		Enabled:     true,
		FFmpeg:      installCompletedChaseFFmpeg(t),
		SegmentDir:  t.TempDir(),
		MaxSessions: 1,
		IdleTimeout: time.Minute,
		Profiles: []LiveProfile{{
			Name:           "h264",
			VideoCodec:     "libx264",
			AudioCodec:     "aac",
			SegmentSeconds: 2,
			PlaylistSize:   6,
		}},
	}
	ls := newLiveStreamer(chaseTestLiveClient{}, cfg)
	t.Cleanup(ls.shutdown)

	chase, err := ls.getOrCreateSessionFor(context.Background(), sessionKey{
		kind: chaseSessionKind,
		id:   44,
	}, func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("input")), nil
	})
	if err != nil {
		t.Fatalf("starting completed chase session: %v", err)
	}
	select {
	case <-chase.done:
	case <-time.After(2 * time.Second):
		t.Fatal("completed chase ffmpeg did not exit")
	}
	if _, err := os.Stat(chase.dir); err != nil {
		t.Fatalf("retained chase directory before eviction: %v", err)
	}

	chase.mu.Lock()
	chase.lastAccess = time.Now().Add(-cfg.idleEvictionThreshold() - time.Second)
	chase.mu.Unlock()

	live, err := ls.getOrCreateSessionFor(context.Background(), sessionKey{
		kind: liveSessionKind,
		id:   99,
	}, func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("input")), nil
	})
	if err != nil {
		t.Fatalf("starting live session after chase eviction: %v", err)
	}
	select {
	case <-live.done:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement live ffmpeg did not exit")
	}

	if _, err := os.Stat(chase.dir); !os.IsNotExist(err) {
		t.Errorf("evicted completed chase directory still exists, stat err = %v", err)
	}
}
