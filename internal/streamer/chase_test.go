package streamer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/testutil"
)

type fakeChaseRecordClient struct {
	mu        sync.Mutex
	errors    []error
	calls     int
	recordIDs []string
}

type retryingChaseStartupClient struct {
	mu                 sync.Mutex
	followCalls        int
	firstFollowEntered chan struct{}
	releaseFirstFollow chan struct{}
}

func (c *retryingChaseStartupClient) StreamRecordFollow(ctx context.Context, _ string) (io.ReadCloser, error) {
	c.mu.Lock()
	c.followCalls++
	call := c.followCalls
	c.mu.Unlock()
	if call == 1 {
		close(c.firstFollowEntered)
		select {
		case <-c.releaseFirstFollow:
			return nil, &mirakc.APIError{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable"}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return io.NopCloser(strings.NewReader("fake-record")), nil
}

func (c *retryingChaseStartupClient) StreamRecord(context.Context, string, int64) (io.ReadCloser, int64, error) {
	return nil, 0, mirakc.ErrRangeNotSatisfiable
}

func (c *retryingChaseStartupClient) GetRecord(context.Context, string) (*mirakc.Record, error) {
	return &mirakc.Record{Recording: mirakc.RecordInfo{Status: "finished"}}, nil
}

func (c *retryingChaseStartupClient) StreamService(context.Context, int64, int) (io.ReadCloser, error) {
	return nil, errors.New("not used")
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
	body, err := waitForChaseRecordAtOffset(context.Background(), client, "opaque-record-id", 188, nil)
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
	reader := newChaseRangeFollowReader(context.Background(), client, "opaque-record-id", 188, nil, nil)

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
	pollMin, pollMax, retryDelay := chaseRangePollMin, chaseRangePollMax, chaseRetryDelay
	chaseRangePollMin, chaseRangePollMax = 5*time.Millisecond, 20*time.Millisecond
	chaseRetryDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() {
		chaseRangePollMin, chaseRangePollMax, chaseRetryDelay = pollMin, pollMax, retryDelay
	})
}

// scriptedChaseRecord は録画中の mirakc record を模す。content のうち visible バイトまでが
// 書かれていて、StreamRecord は要求された offset から visible までを返す（offset が visible
// を超えたら違反として記録する）。追従配信は先頭から followBytes バイトを返し、followErr で
// 終わる（nil なら正常な EOF）。followGate があれば、それが閉じるまで追従配信を流さない。
// rangeErrs / statusErrs は応じる前に順に返す失敗（404 なら record が消えた）。
// onStatus は失敗を返さなかった GetRecord のたびに呼ばれ、録画を進める（n は何回目か）。
type scriptedChaseRecord struct {
	mu          sync.Mutex
	content     string
	visible     int
	recording   bool
	chunked     bool
	followBytes int
	followErr   error
	followGate  chan struct{}
	followCalls int
	rangeErrs   []error
	statusErrs  []error
	onStatus    func(c *scriptedChaseRecord, n int)
	offsets     []int64
	rangeAt     []time.Time
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

// gatedReader は gate が閉じるまで最初の Read を待たせる。
type gatedReader struct {
	gate <-chan struct{}
	r    io.Reader
}

func (g *gatedReader) Read(p []byte) (int, error) {
	<-g.gate
	return g.r.Read(p)
}

func (c *scriptedChaseRecord) StreamRecordFollow(context.Context, string) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.followCalls++
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

func (c *scriptedChaseRecord) StreamRecord(_ context.Context, _ string, offset int64) (io.ReadCloser, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offsets = append(c.offsets, offset)
	c.rangeAt = append(c.rangeAt, time.Now())
	if len(c.rangeErrs) > 0 {
		err := c.rangeErrs[0]
		c.rangeErrs = c.rangeErrs[1:]
		return nil, 0, err
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
	status := "finished"
	if c.recording {
		status = "recording"
	}
	return &mirakc.Record{Recording: mirakc.RecordInfo{Status: status}}, nil
}

func (c *scriptedChaseRecord) StreamService(context.Context, int64, int) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

func (c *scriptedChaseRecord) rangeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.offsets)
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

var errNotFoundForTest = &mirakc.APIError{StatusCode: http.StatusNotFound, Status: "404 Not Found"}

// readChaseInput は先頭からの追っかけの入力を読み切る。5 秒で終わらなければ失敗にする。
func readChaseInput(t *testing.T, client *scriptedChaseRecord, committedSize chaseCommittedSize) (string, error) {
	t.Helper()
	body, err := followChaseRecord(context.Background(), client, "opaque-record-id", committedSize)
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

// chaseTestConfig は偽 ffmpeg で 1 プロファイルの追っかけを回す設定を返す。
func chaseTestConfig(t *testing.T, ffmpeg string) LiveConfig {
	t.Helper()
	return LiveConfig{
		Enabled:     true,
		FFmpeg:      ffmpeg,
		SegmentDir:  t.TempDir(),
		MaxSessions: 4,
		IdleTimeout: time.Minute,
		Profiles:    []LiveProfile{{Name: "hd", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6}},
	}
}

// installEndlistMarkerFFmpeg は playlist を先に書き、stdin を input.bin へ写し、EOF を見たら
// ENDLIST を足して marker を作る偽 ffmpeg。kill されると marker は作られない。
func installEndlistMarkerFFmpeg(t *testing.T) (ffmpeg, marker string) {
	t.Helper()
	marker = filepath.Join(t.TempDir(), "endlist-written")
	ffmpeg = installChaseFFmpegScript(t, `cat > "$(dirname "$playlist")/input.bin"
echo '#EXT-X-ENDLIST' >> "$playlist"
touch "`+marker+`"
exit 0
`)
	return ffmpeg, marker
}

func requestHeadChasePlaylist(ls *LiveStreamer, recordingID int64, recordingStatus string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/sites/default/recordings/42/chase/playlist.m3u8?profile=hd", nil)
	resp := httptest.NewRecorder()
	ls.ChasePlaylistForTarget(resp, req, ChaseTarget{
		RecordingID: recordingID, Site: "default", RecordID: "record-42",
		Status: recordingStatus, RecordingStatus: recordingStatus,
	})
	return resp
}

func requestChasePlaylistAtOffset(ls *LiveStreamer, recordingID, offsetSeconds int64) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet,
		"/api/sites/default/recordings/42/chase/offset/"+strconv.FormatInt(offsetSeconds, 10)+"/playlist.m3u8?profile=hd", nil)
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("offset", strconv.FormatInt(offsetSeconds, 10))
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
	resp := httptest.NewRecorder()
	ls.ChasePlaylistForTarget(resp, req, ChaseTarget{
		RecordingID:     recordingID,
		Site:            "default",
		RecordID:        "record-42",
		Status:          "recording",
		RecordingStatus: "recording",
	})
	return resp
}

// startGatedHeadChase は先頭からの追っかけを 1 本起こし、playlist が返った後に入力を流す。
// セッションが終わるまで待って返す。
func startGatedHeadChase(t *testing.T, ls *LiveStreamer, client *scriptedChaseRecord, recordingID int64) *liveSession {
	t.Helper()
	if resp := requestHeadChasePlaylist(ls, recordingID, "recording"); resp.Code != http.StatusOK {
		t.Fatalf("playlist status = %d, want 200 (%s)", resp.Code, resp.Body.String())
	}
	ls.mu.Lock()
	s := ls.chaseSessions[chaseSessionKeyFor(recordingID, 0)]
	ls.mu.Unlock()
	if s == nil {
		t.Fatal("chase session disappeared before the input was released")
	}
	close(client.followGate)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("chase session did not end")
	}
	return s
}

// TestFollowChaseRecordContinuesWithRangeAfterFollowCloses は、先頭からの追っかけで mirakc の
// 追従配信が録画中に閉じても、ffmpeg の入力を EOF にしない（= playlist に ENDLIST を付けない）
// ことを固定する。ChasePlaylistForTarget を通り、偽 ffmpeg は stdin を全部ファイルへ書く。
func TestFollowChaseRecordContinuesWithRangeAfterFollowCloses(t *testing.T) {
	withFastChaseTimings(t)
	ffmpeg, marker := installEndlistMarkerFFmpeg(t)
	client := &scriptedChaseRecord{
		content: "headtail", visible: 4, followBytes: 4, recording: true, onStatus: appendThenFinish,
		followGate: make(chan struct{}),
	}
	ls := newLiveStreamer(client, chaseTestConfig(t, ffmpeg))
	t.Cleanup(ls.shutdown)
	s := startGatedHeadChase(t, ls, client, 42)
	data, err := os.ReadFile(filepath.Join(s.dir, "input.bin"))
	if err != nil {
		t.Fatal(err)
	}
	// 追従が閉じた時点で EOF にすると "head" で終わり、録画中の playlist に ENDLIST が付く。
	if string(data) != "headtail" {
		t.Fatalf("ffmpeg input = %q, want %q", data, "headtail")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("ffmpeg did not see EOF after the recording finished: %v", err)
	}
	assertOffsetsAdvance(t, client)
}

// TestChasePlaylistRequiresSeekClientAtHead は、先頭からの追っかけでも Range の続きを読める
// クライアントを要求することを固定する（読めないなら追従配信だけで黙って続けない）。
func TestChasePlaylistRequiresSeekClientAtHead(t *testing.T) {
	ls := newLiveStreamer(followOnlyChaseRecordClient{}, chaseTestConfig(t, installCompletedChaseFFmpeg(t)))
	t.Cleanup(ls.shutdown)
	if resp := requestHeadChasePlaylist(ls, 42, "recording"); resp.Code != http.StatusServiceUnavailable {
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

// TestChaseRangeFollowReaderTreatsPurgeAsEndOnlyWhenComplete は、record が 404 になったとき、
// 読んだ位置がコミット済み原本の終端と一致するときだけ EOF にし、それ以外はエラーにすることを
// 固定する（ffmpeg が先端より遅れていて purge が先に来ると、残りを読めない）。
func TestChaseRangeFollowReaderTreatsPurgeAsEndOnlyWhenComplete(t *testing.T) {
	withFastChaseTimings(t)
	committed := func(size int64, ok bool) chaseCommittedSize {
		return func(context.Context) (int64, bool, error) { return size, ok, nil }
	}
	tests := []struct {
		name      string
		client    func() *scriptedChaseRecord
		committed chaseCommittedSize
		wantErr   bool
	}{
		{
			name: "range 404 at the committed end",
			client: func() *scriptedChaseRecord {
				return &scriptedChaseRecord{content: "headtail", visible: 8, followBytes: 8, rangeErrs: []error{errNotFoundForTest}}
			},
			committed: committed(8, true),
		},
		{
			name: "status 404 at the committed end",
			client: func() *scriptedChaseRecord {
				return &scriptedChaseRecord{content: "headtail", visible: 8, followBytes: 8, recording: true, statusErrs: []error{errNotFoundForTest}}
			},
			committed: committed(8, true),
		},
		{
			name: "purged behind the committed end",
			client: func() *scriptedChaseRecord {
				return &scriptedChaseRecord{content: "headtail", visible: 8, followBytes: 4, rangeErrs: []error{errNotFoundForTest}}
			},
			committed: committed(8, true),
			wantErr:   true,
		},
		{
			name: "no committed original",
			client: func() *scriptedChaseRecord {
				return &scriptedChaseRecord{content: "headtail", visible: 8, followBytes: 8, rangeErrs: []error{errNotFoundForTest}}
			},
			committed: committed(0, false),
			wantErr:   true,
		},
		{
			name: "no committed size source",
			client: func() *scriptedChaseRecord {
				return &scriptedChaseRecord{content: "headtail", visible: 8, followBytes: 8, rangeErrs: []error{errNotFoundForTest}}
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readChaseInput(t, tt.client(), tt.committed)
			if tt.wantErr && !errors.Is(err, errChaseRecordPurged) {
				t.Fatalf("chase input ended with %v, want errChaseRecordPurged", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("chase input ended with %v, want a clean EOF at the committed end", err)
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
	data, err := readChaseInput(t, client, nil)
	if err != nil {
		t.Fatalf("chase input ended with %v, want transient failures to be retried", err)
	}
	if data != "headtail" {
		t.Fatalf("chase input = %q, want %q", data, "headtail")
	}
	assertOffsetsAdvance(t, client)
}

// TestChaseRangeFollowReaderRetryLimit は、一過性の失敗を連続 5 回までは再試行して読み切り、
// 6 回目でエラーにすることを固定する。再試行しても変わらない失敗（4xx）は 1 回目でエラーにする。
func TestChaseRangeFollowReaderRetryLimit(t *testing.T) {
	withFastChaseTimings(t)
	unavailable := &mirakc.APIError{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable"}
	failures := func(n int) []error {
		errs := make([]error, n)
		for i := range errs {
			errs[i] = unavailable
		}
		return errs
	}
	tests := []struct {
		name      string
		rangeErrs []error
		wantErr   bool
	}{
		{name: "5 consecutive transient failures", rangeErrs: failures(5)},
		{name: "6 consecutive transient failures", rangeErrs: failures(6), wantErr: true},
		{name: "permanent", rangeErrs: []error{&mirakc.APIError{StatusCode: http.StatusBadRequest, Status: "400 Bad Request"}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &scriptedChaseRecord{
				content: "headtail", visible: 4, followBytes: 4, recording: true, onStatus: appendThenFinish,
				rangeErrs: tt.rangeErrs,
			}
			data, err := readChaseInput(t, client, nil)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("chase input ended cleanly with %q, want an error", data)
				}
				if data != "head" {
					t.Fatalf("chase input before the error = %q, want %q", data, "head")
				}
				return
			}
			if err != nil || data != "headtail" {
				t.Fatalf("chase input = %q, %v; want %q after retrying", data, err, "headtail")
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
			data, err := readChaseInput(t, client, nil)
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
	data, err := readChaseInput(t, client, nil)
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
	data, err := readChaseInput(t, client, nil)
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

// TestChaseRangeFollowReaderBoundsEdgeLag は、変換の入力が録画の先端から遅れる幅を固定する。
// 録画が止まっていた後の再開は chaseRangePollMax 以内に取りに行き、データが続く間は
// chaseRangePollMin 間隔で取る（docs/frontend/live.md の「サーバーが追いつく」前提）。
func TestChaseRangeFollowReaderBoundsEdgeLag(t *testing.T) {
	withFastChaseTimings(t)
	chaseRangePollMin, chaseRangePollMax = 10*time.Millisecond, 40*time.Millisecond
	client := &scriptedChaseRecord{content: strings.Repeat("x", 4+200), visible: 4, followBytes: 4, recording: true}
	resumedAt := make(chan time.Time, 1)
	go func() {
		// 400ms 止まる（上限の無いバックオフなら 10, 20, ..., 320ms まで間隔が伸びる）。
		time.Sleep(400 * time.Millisecond)
		client.mu.Lock()
		client.visible = 5
		resumedAt <- time.Now()
		client.mu.Unlock()
		// その後は 5ms ごとに 1 バイトずつ書かれ続け、最後に録画が終わる。
		for {
			time.Sleep(5 * time.Millisecond)
			client.mu.Lock()
			client.visible++
			if client.visible == len(client.content) {
				client.recording = false
				client.mu.Unlock()
				return
			}
			client.mu.Unlock()
		}
	}()
	data, err := readChaseInput(t, client, nil)
	if err != nil || data != client.content {
		t.Fatalf("chase input = %d bytes, %v; want %d bytes", len(data), err, len(client.content))
	}
	resumed := <-resumedAt
	client.mu.Lock()
	defer client.mu.Unlock()
	var firstAfterResume time.Time
	var maxGap time.Duration
	for i, at := range client.rangeAt {
		if at.Before(resumed) {
			continue
		}
		if firstAfterResume.IsZero() {
			firstAfterResume = at
		} else if gap := at.Sub(client.rangeAt[i-1]); gap > maxGap {
			maxGap = gap
		}
	}
	if lag := firstAfterResume.Sub(resumed); lag > 100*time.Millisecond {
		t.Fatalf("first Range after the recording resumed came %v later, want within the 40ms backoff cap (+slack)", lag)
	}
	if maxGap > 50*time.Millisecond {
		t.Fatalf("largest gap between Range requests while data kept coming = %v, want about the 10ms minimum", maxGap)
	}
}

// TestChaseRangeFollowReaderCloseStopsConcurrentRead は、Read が進行中でも別の goroutine の
// Close がそれを打ち切り、以後 mirakc へ要求しないことを固定する（-race で回す）。
func TestChaseRangeFollowReaderCloseStopsConcurrentRead(t *testing.T) {
	withFastChaseTimings(t)
	tests := []struct {
		name string
		body func() io.ReadCloser
	}{
		// 録画中で追い付いたまま、次の Range まで待っている（待ちは 2 秒にする）。
		{name: "waiting", body: func() io.ReadCloser { return nil }},
		// 本文の Read で止まっている（追従配信が何も送ってこない）。
		{name: "blocked body", body: func() io.ReadCloser { pr, _ := io.Pipe(); return pr }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chaseRangePollMin, chaseRangePollMax = 2*time.Second, 2*time.Second
			client := &scriptedChaseRecord{content: "head", visible: 4, recording: true}
			reader := newChaseRangeFollowReader(context.Background(), client, "opaque-record-id", 4, tt.body(), nil)
			done := make(chan error, 1)
			go func() {
				_, err := io.ReadAll(reader)
				done <- err
			}()
			time.Sleep(50 * time.Millisecond)
			if err := reader.Close(); err != nil {
				t.Fatalf("Close() = %v", err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("Read after Close ended with a clean EOF, want an error")
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("Close did not stop the concurrent Read within 500ms")
			}
			before := client.rangeCount()
			time.Sleep(100 * time.Millisecond)
			if after := client.rangeCount(); after != before {
				t.Fatalf("Range requests after Close: %d → %d, want none", before, after)
			}
		})
	}
}

// TestChaseInputErrorDoesNotWriteEndlist は、追っかけ入力のエラー後にセッションを破棄し、
// 同じ録画の全 offset で cooldown が切れるまで再作成しないこと、切れた後は再生できることを固定する。
func TestChaseInputErrorDoesNotWriteEndlist(t *testing.T) {
	withFastChaseTimings(t)
	previousCooldown := chaseInputFailureCooldown
	cooldown := 30 * time.Millisecond
	chaseInputFailureCooldown = cooldown
	t.Cleanup(func() { chaseInputFailureCooldown = previousCooldown })
	ffmpeg, marker := installEndlistMarkerFFmpeg(t)
	// 追従配信が閉じた後の Range が、再試行しても変わらない 400 で失敗する。
	client := &scriptedChaseRecord{
		content: "head", visible: 4, followBytes: 4, recording: true, followGate: make(chan struct{}),
		rangeErrs: []error{&mirakc.APIError{StatusCode: http.StatusBadRequest, Status: "400 Bad Request"}},
	}
	ls := newLiveStreamer(client, chaseTestConfig(t, ffmpeg))
	t.Cleanup(ls.shutdown)
	startGatedHeadChase(t, ls, client, 42)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ffmpeg saw EOF and wrote ENDLIST after the chase input failed")
	}
	ls.mu.Lock()
	_, retained := ls.chaseSessions[chaseSessionKeyFor(42, 0)]
	_, failed := ls.failedChaseInputs[42]
	ls.mu.Unlock()
	if retained {
		t.Fatal("a chase session whose input failed was retained")
	}
	if !failed {
		t.Fatal("input failure was not recorded under the recording ID")
	}

	if resp := requestHeadChasePlaylist(ls, 42, "finished"); resp.Code != http.StatusNotFound {
		t.Fatalf("playlist status after the recording finished = %d, want 404", resp.Code)
	}

	if resp := requestHeadChasePlaylist(ls, 42, "recording"); resp.Code != http.StatusBadGateway {
		t.Fatalf("playlist status during the input failure cooldown = %d, want 502 (%s)", resp.Code, resp.Body.String())
	} else {
		if got := resp.Body.String(); !strings.Contains(got, chaseInputCooldownMessage) {
			t.Fatalf("cooldown response body = %q, want %q", got, chaseInputCooldownMessage)
		}
		if got := resp.Header().Get("Retry-After"); got != "1" {
			t.Fatalf("Retry-After = %q, want 1 for the short test cooldown", got)
		}
	}

	// The frontend retries at the playback position, which has a different offset key.
	// Concurrent requests for any offset must see the same recording-level marker.
	var sourceMu sync.Mutex
	sourceCalls := 0
	var requestWG sync.WaitGroup
	requestErrs := make(chan error, 12)
	for offset := int64(1); offset <= 12; offset++ {
		requestWG.Add(1)
		go func(offset int64) {
			defer requestWG.Done()
			_, err := ls.getOrCreateSessionFor(context.Background(), sessionKey{
				kind: chaseSessionKind, id: 42, offsetSeconds: offset,
			}, func(context.Context) (io.ReadCloser, error) {
				sourceMu.Lock()
				sourceCalls++
				sourceMu.Unlock()
				return io.NopCloser(strings.NewReader("unexpected")), nil
			})
			requestErrs <- err
		}(offset)
	}
	requestWG.Wait()
	close(requestErrs)
	for err := range requestErrs {
		if !errors.Is(err, errChaseInputCoolingDown) {
			t.Fatalf("different-offset getOrCreateSessionFor() = %v, want input cooldown", err)
		}
	}
	sourceMu.Lock()
	gotSourceCalls := sourceCalls
	sourceMu.Unlock()
	if gotSourceCalls != 0 {
		t.Fatalf("session sources started during cooldown = %d, want 0", gotSourceCalls)
	}
	client.mu.Lock()
	followCallsDuringCooldown := client.followCalls
	client.visible = len(client.content)
	client.recording = false
	client.mu.Unlock()
	if followCallsDuringCooldown != 1 {
		t.Fatalf("mirakc follow requests during cooldown = %d, want 1", followCallsDuringCooldown)
	}

	time.Sleep(cooldown + 20*time.Millisecond)
	if resp := requestHeadChasePlaylist(ls, 42, "recording"); resp.Code != http.StatusOK {
		t.Fatalf("playlist status after the cooldown and input recovery = %d, want 200 (%s)", resp.Code, resp.Body.String())
	}
	ls.mu.Lock()
	recovered := ls.chaseSessions[chaseSessionKeyFor(42, 0)]
	ls.mu.Unlock()
	if recovered == nil {
		t.Fatal("recovered chase session was not retained")
	}
	select {
	case <-recovered.done:
	case <-time.After(5 * time.Second):
		t.Fatal("recovered chase session did not finish")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("recovered session did not write ENDLIST: %v", err)
	}
	client.mu.Lock()
	followCalls := client.followCalls
	client.mu.Unlock()
	if followCalls != 2 {
		t.Fatalf("mirakc follow requests after recovery = %d, want 2", followCalls)
	}
}

func TestChaseCooldownSkipsMetadataAndAllowsExistingOffset(t *testing.T) {
	client := &scriptedChaseRecord{statusErrs: []error{errors.New("mirakc metadata unavailable")}}
	ls := newLiveStreamer(client, chaseTestConfig(t, "unused-ffmpeg"))
	t.Cleanup(ls.shutdown)

	ls.mu.Lock()
	ls.failedChaseInputs[42] = time.Now().Add(time.Minute)
	ls.mu.Unlock()

	resp := requestChasePlaylistAtOffset(ls, 42, 17)
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("nonzero-offset playlist status during cooldown = %d, want 502 (%s)", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Retry-After"); got == "" {
		t.Fatal("nonzero-offset cooldown response is missing Retry-After")
	}
	if !strings.Contains(resp.Body.String(), chaseInputCooldownMessage) {
		t.Fatalf("nonzero-offset cooldown body = %q, want %q", resp.Body.String(), chaseInputCooldownMessage)
	}
	client.mu.Lock()
	metadataWasNotRead := len(client.statusErrs) == 1
	client.mu.Unlock()
	if !metadataWasNotRead {
		t.Fatal("nonzero-offset request queried mirakc metadata before checking the cooldown")
	}

	// A failed offset must not keep viewers from joining a different, healthy offset.
	healthyDir := t.TempDir()
	playlist := []byte("#EXTM3U\n#EXTINF:2.0,\nsegment_00000.ts\n")
	if err := os.WriteFile(filepath.Join(healthyDir, "hd.m3u8"), playlist, 0o600); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	done := make(chan struct{})
	close(done)
	healthy := &liveSession{
		key:        chaseSessionKeyFor(42, 4),
		dir:        healthyDir,
		ready:      ready,
		done:       done,
		cancel:     func() {},
		lastAccess: time.Now(),
	}
	ls.mu.Lock()
	ls.putSessionLocked(healthy)
	ls.mu.Unlock()

	resp = requestChasePlaylistAtOffset(ls, 42, 4)
	if resp.Code != http.StatusOK {
		t.Fatalf("existing healthy offset during cooldown = %d, want 200 (%s)", resp.Code, resp.Body.String())
	}
	if !bytes.Equal(resp.Body.Bytes(), playlist) {
		t.Fatalf("existing healthy offset playlist = %q, want %q", resp.Body.Bytes(), playlist)
	}
	client.mu.Lock()
	metadataWasNotRead = len(client.statusErrs) == 1
	client.mu.Unlock()
	if !metadataWasNotRead {
		t.Fatal("joining an existing offset queried mirakc metadata")
	}
}

func TestChaseConcurrentJoinerUsesUpstreamStartupRetry(t *testing.T) {
	setShortLiveMirakcReleaseWait(t, 10*time.Millisecond)
	ffmpeg, _ := installEndlistMarkerFFmpeg(t)
	cfg := chaseTestConfig(t, ffmpeg)
	cfg.IdleTimeout = 30 * time.Second
	client := &retryingChaseStartupClient{
		firstFollowEntered: make(chan struct{}),
		releaseFirstFollow: make(chan struct{}),
	}
	ls := newLiveStreamer(client, cfg)
	t.Cleanup(ls.shutdown)

	// Make an idle victim available so the initial upstream rejection must use
	// the same recovery path as other live session startup failures.
	ready := make(chan struct{})
	close(ready)
	done := make(chan struct{})
	close(done)
	victim := &liveSession{
		serviceID:  999,
		key:        sessionKey{kind: liveSessionKind, id: 999},
		ready:      ready,
		done:       done,
		cancel:     func() {},
		lastAccess: time.Now().Add(-time.Hour),
	}
	ls.mu.Lock()
	ls.putSessionLocked(victim)
	ls.mu.Unlock()

	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() { responses <- requestHeadChasePlaylist(ls, 42, "recording") }()
	select {
	case <-client.firstFollowEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("initial chase upstream request did not start")
	}
	go func() { responses <- requestHeadChasePlaylist(ls, 42, "recording") }()
	// Let the second public request join the in-flight session and wait on ready
	// before releasing its upstream failure.
	time.Sleep(100 * time.Millisecond)
	close(client.releaseFirstFollow)

	for i := 0; i < 2; i++ {
		select {
		case resp := <-responses:
			if resp.Code != http.StatusOK {
				t.Errorf("concurrent chase playlist status = %d, want 200 (%s)", resp.Code, resp.Body.String())
			}
		case <-time.After(5 * time.Second):
			client.mu.Lock()
			followCalls := client.followCalls
			client.mu.Unlock()
			ls.mu.Lock()
			active := ls.chaseSessions[chaseSessionKeyFor(42, 0)]
			activeStartErr := error(nil)
			activeDir := ""
			if active != nil {
				select {
				case <-active.ready:
					activeStartErr = active.startErr
					activeDir = active.dir
				default:
				}
			}
			ls.mu.Unlock()
			entries, _ := os.ReadDir(activeDir)
			activeFiles := make([]string, 0, len(entries))
			for _, entry := range entries {
				activeFiles = append(activeFiles, entry.Name())
			}
			t.Fatalf("concurrent request %d did not finish after %d responses (follow calls=%d, active session=%v, dir=%q, start error=%v, files=%v)",
				i+1, i, followCalls, active != nil, activeDir, activeStartErr, activeFiles)
		}
	}
	client.mu.Lock()
	followCalls := client.followCalls
	client.mu.Unlock()
	if followCalls != 2 {
		t.Fatalf("chase upstream attempts = %d, want one initial failure and one shared retry", followCalls)
	}
}

// TestChasePurgeEndUsesCommittedOriginal は、record が 404 になったときの判定が DB の
// コミット済み原本のバイト数を ChasePlaylistForTarget から使うことを固定する。読んだ位置が
// 原本の終端なら ENDLIST を付けて保持し、手前なら ffmpeg を止める。
func TestChasePurgeEndUsesCommittedOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	tests := []struct {
		name        string
		eventID     int32
		committed   int64
		wantEndlist bool
	}{
		{name: "read to the committed end", eventID: 1001, committed: 8, wantEndlist: true},
		{name: "purged before the committed end", eventID: 1002, committed: 12, wantEndlist: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withFastChaseTimings(t)
			recordingID := seedRecordingWithEvent(t, pool, tt.eventID)
			seedAsset(t, pool, recordingID, fmt.Sprintf("recordings/chase-purge-%d.ts", recordingID), tt.committed)
			ffmpeg, marker := installEndlistMarkerFFmpeg(t)
			// 追従配信が 8 バイト渡して閉じ、その後の Range はもう 404（ingest が purge した）。
			client := &scriptedChaseRecord{
				content: "headtail", visible: 8, followBytes: 8, recording: true, followGate: make(chan struct{}),
				rangeErrs: []error{errNotFoundForTest},
			}
			ls := newLiveStreamer(client, chaseTestConfig(t, ffmpeg))
			ls.pool = pool
			t.Cleanup(ls.shutdown)
			startGatedHeadChase(t, ls, client, recordingID)
			_, statErr := os.Stat(marker)
			if gotEndlist := statErr == nil; gotEndlist != tt.wantEndlist {
				t.Fatalf("ENDLIST written = %v, want %v", gotEndlist, tt.wantEndlist)
			}
			ls.mu.Lock()
			_, retained := ls.chaseSessions[chaseSessionKeyFor(recordingID, 0)]
			ls.mu.Unlock()
			if retained != tt.wantEndlist {
				t.Fatalf("session retained = %v, want %v", retained, tt.wantEndlist)
			}
		})
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

// lockedLogBuffer は複数の goroutine から書かれるログを集める。
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureSlog(t *testing.T) *lockedLogBuffer {
	t.Helper()
	logs := &lockedLogBuffer{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return logs
}

// installChaseFFmpegScript は playlist を書いてから body を実行する偽 ffmpeg を作る。
func installChaseFFmpegScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg-chase")
	script := `#!/bin/sh
playlist=""
for a in "$@"; do case "$a" in *.m3u8) playlist="$a";; esac; done
printf '#EXTM3U\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nsegments/x.ts\n' > "$playlist"
` + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestChaseFFmpegCrashIsNotReportedAsInputFailure は、入力が待っている間に ffmpeg が自分で
// 異常終了したら、入力の失敗ではなく ffmpeg の異常終了として stderr ごと記録することを固定する。
// 終了後に入力を閉じて起きるエラーを入力の失敗と取り違えると、stderr が残らない。
func TestChaseFFmpegCrashIsNotReportedAsInputFailure(t *testing.T) {
	withFastChaseTimings(t)
	// 入力は追い付いたまま次の Range を 2 秒待っている。
	chaseRangePollMin, chaseRangePollMax = 2*time.Second, 2*time.Second
	logs := captureSlog(t)
	ffmpeg := installChaseFFmpegScript(t, `sleep 0.5
echo "fake ffmpeg crashed" >&2
exit 1
`)
	client := &scriptedChaseRecord{content: "head", visible: 4, followBytes: 4, recording: true}
	ls := newLiveStreamer(client, chaseTestConfig(t, ffmpeg))
	t.Cleanup(ls.shutdown)
	if resp := requestHeadChasePlaylist(ls, 42, "recording"); resp.Code != http.StatusOK {
		t.Fatalf("playlist status = %d, want 200 (%s)", resp.Code, resp.Body.String())
	}
	ls.mu.Lock()
	s := ls.chaseSessions[chaseSessionKeyFor(42, 0)]
	ls.mu.Unlock()
	if s == nil {
		t.Fatal("chase session disappeared before ffmpeg exited")
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("chase session did not end after ffmpeg exited")
	}
	ls.mu.Lock()
	_, retained := ls.chaseSessions[chaseSessionKeyFor(42, 0)]
	_, inputFailureRecorded := ls.failedChaseInputs[42]
	ls.mu.Unlock()
	if retained {
		t.Fatal("a chase session whose ffmpeg crashed was retained")
	}
	if inputFailureRecorded {
		t.Fatal("an ffmpeg crash was recorded as an input failure")
	}
	got := logs.String()
	if !strings.Contains(got, "ffmpeg exited unexpectedly") || !strings.Contains(got, "fake ffmpeg crashed") {
		t.Fatalf("logs = %q, want the ffmpeg crash with its stderr", got)
	}
	if strings.Contains(got, "chase input failed") {
		t.Fatalf("logs = %q, want no input failure for an ffmpeg crash", got)
	}
}

// stdinHolderScript は、ffmpeg の stdin（パイプの読み側）を握ったまま読まない孫を起こし、その
// pid を pidFile に書くシェル断片。非対話シェルの非同期リストは stdin が /dev/null になる
// （dash は `0<&0` を付けてもそうなる）ので、stdin を fd 3 に写してから孫の stdin に戻す。
func stdinHolderScript(pidFile string) string {
	return "{ sleep 5 <&3 3<&- >/dev/null 2>&1 & echo $! > '" + pidFile + "'; } 3<&0\n"
}

// TestChaseInputCopyFinishUnblocksStuckWrite は、ffmpeg の孫が stdin の読み側を握ったまま
// 読まず、パイプが埋まって写しの Write が止まっても、セッションの終了（ffmpeg が自分で
// 終わった場合）と stop（shutdown）が戻ることを固定する。
func TestChaseInputCopyFinishUnblocksStuckWrite(t *testing.T) {
	tests := []struct {
		name string
		// rest は孫を起こした後の偽 ffmpeg の振る舞い。
		rest string
		// stop が真なら、ffmpeg が生きている間に shutdown する。
		stop bool
	}{
		{name: "ffmpeg exits", rest: "sleep 0.3\nexit 0\n"},
		{name: "shutdown", rest: "exec sleep 5 </dev/null >/dev/null 2>&1\n", stop: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withFastChaseTimings(t)
			pidFile := filepath.Join(t.TempDir(), "holder.pid")
			ffmpeg := installChaseFFmpegScript(t, stdinHolderScript(pidFile)+tt.rest)
			// 1 MiB はパイプの容量より大きいので、読まれなければ Write が止まる。
			content := strings.Repeat("x", 1<<20)
			client := &scriptedChaseRecord{content: content, visible: len(content), followBytes: len(content), recording: true}
			ls := newLiveStreamer(client, chaseTestConfig(t, ffmpeg))
			ended := make(chan struct{})
			// 判定が落ちても、孫を止めて読み側を閉じれば止まった Write も抜けるので、後の
			// テストへセッションとパイプを持ち越さない。それでも終わらなければ待ちを打ち切る。
			t.Cleanup(func() {
				if data, err := os.ReadFile(pidFile); err == nil {
					if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
				select {
				case <-ended:
				case <-time.After(10 * time.Second):
					t.Error("cleanup: the chase session did not end even after the stdin holder was killed")
				}
			})
			if resp := requestHeadChasePlaylist(ls, 42, "recording"); resp.Code != http.StatusOK {
				close(ended)
				ls.shutdown()
				t.Fatalf("playlist status = %d, want 200 (%s)", resp.Code, resp.Body.String())
			}
			ls.mu.Lock()
			s := ls.chaseSessions[chaseSessionKeyFor(42, 0)]
			ls.mu.Unlock()
			go func() {
				if tt.stop {
					time.Sleep(300 * time.Millisecond)
				} else if s != nil {
					<-s.done
				}
				ls.shutdown()
				close(ended)
			}()
			if s == nil {
				t.Fatal("chase session disappeared")
			}
			for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
				if _, err := os.Stat(pidFile); err == nil {
					break
				} else if time.Now().After(deadline) {
					t.Fatalf("the stdin holder did not start: %v", err)
				}
			}
			select {
			case <-ended:
			case <-time.After(2 * time.Second):
				t.Fatal("the chase session (or shutdown) did not return while its stdin write was stuck")
			}
		})
	}
}

// TestFFmpegSessionCompletedKeepsCrashBesideInputFailure は、入力の失敗で kill したのと同じころに
// ffmpeg が自分で落ちていたら（終わり方が SIGKILL でない）、ffmpeg の異常終了も stderr ごと
// 記録し、こちらの kill で終わったなら記録しないことを固定する。2 つが同時に起きる窓は
// runSession を通して決定的に作れないので、終わり方を判定する関数に本物のプロセスの Wait の
// 結果を渡して見る。
func TestFFmpegSessionCompletedKeepsCrashBesideInputFailure(t *testing.T) {
	inputErr := errors.New("chase input broke")
	tests := []struct {
		name      string
		run       func(t *testing.T, cmd *exec.Cmd) error
		script    string
		wantCrash bool
	}{
		{
			name:   "ffmpeg exited by itself",
			script: "echo 'fake ffmpeg crashed' >&2; exit 3",
			run: func(_ *testing.T, cmd *exec.Cmd) error {
				return cmd.Run()
			},
			wantCrash: true,
		},
		{
			name:   "killed by us",
			script: "echo 'fake ffmpeg crashed' >&2; exec sleep 5",
			run: func(t *testing.T, cmd *exec.Cmd) error {
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				time.Sleep(200 * time.Millisecond)
				_ = cmd.Process.Kill()
				return cmd.Wait()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureSlog(t)
			stderr := newCappedWriter(stderrCap)
			cmd := exec.Command("/bin/sh", "-c", tt.script)
			cmd.Stderr = stderr
			waitErr := tt.run(t, cmd)
			if waitErr == nil {
				t.Fatal("the process exited 0, want a failure")
			}
			if ffmpegSessionCompleted(context.Background(), cmd, waitErr, inputErr, chaseSessionKind, 42, stderr) {
				t.Fatal("ffmpegSessionCompleted = true, want false after an input failure")
			}
			got := logs.String()
			if !strings.Contains(got, "chase input failed") {
				t.Fatalf("logs = %q, want the input failure", got)
			}
			// 否定側は stderr の有無に依らず判定する（echo が kill に間に合わないと素通りするため）。
			gotCrash := strings.Contains(got, "ffmpeg exited unexpectedly")
			if gotCrash != tt.wantCrash {
				t.Fatalf("crash logged = %v, want %v (logs = %q)", gotCrash, tt.wantCrash, got)
			}
			if tt.wantCrash && !strings.Contains(got, "fake ffmpeg crashed") {
				t.Fatalf("logs = %q, want the crash logged with its stderr", got)
			}
		})
	}
}

// TestBuildChaseFFmpegArgs_RealFFmpegEndlistOnlyAtStdinEOF は、本物の ffmpeg が追っかけの
// 引数で、stdin の EOF で ENDLIST を書き、stdin が開いている間と kill されたときは書かないことを
// 測る（docs/api/media.md の追っかけの節の前提）。CI は ROKUBAN_REQUIRE_FFMPEG で skip を禁じる。
func TestBuildChaseFFmpegArgs_RealFFmpegEndlistOnlyAtStdinEOF(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	in := filepath.Join(t.TempDir(), "in.ts")
	runFFmpeg(t, ffmpeg, nil,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=30",
		"-f", "lavfi", "-i", "sine=f=440:r=48000",
		"-t", "6", "-c:v", "mpeg2video", "-c:a", "aac", "-f", "mpegts", in)
	input, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	cfg := LiveConfig{Profiles: []LiveProfile{
		{Name: "hd", VideoCodec: "mpeg2video", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
	}}
	for _, tc := range []struct {
		name string
		eof  bool
	}{
		{name: "stdin EOF", eof: true},
		{name: "killed", eof: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "segments"), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(ffmpeg, BuildChaseFFmpegArgs(cfg, dir, false)...)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			go func() { _, _ = stdin.Write(input) }()

			playlist := filepath.Join(dir, "hd.m3u8")
			read := func() string {
				data, _ := os.ReadFile(playlist)
				return string(data)
			}
			// 入力を全部渡しても stdin が開いている間は、全 segment を書いても ENDLIST は無い。
			for deadline := time.Now().Add(20 * time.Second); strings.Count(read(), "#EXTINF") < 2; time.Sleep(50 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("ffmpeg wrote no segments within 20s (playlist %q)\n%s", read(), stderr.String())
				}
			}
			time.Sleep(500 * time.Millisecond)
			if strings.Contains(read(), "#EXT-X-ENDLIST") {
				t.Fatalf("playlist has ENDLIST while stdin is still open:\n%s", read())
			}
			if tc.eof {
				_ = stdin.Close()
				if err := cmd.Wait(); err != nil {
					t.Fatalf("ffmpeg after stdin EOF: %v\n%s", err, stderr.String())
				}
			} else {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
			if got := strings.Contains(read(), "#EXT-X-ENDLIST"); got != tc.eof {
				t.Fatalf("ENDLIST written = %v, want %v:\n%s", got, tc.eof, read())
			}
		})
	}
}
