package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// makeTSData は指定バイト数の 188 バイト境界に揃った TS データを生成する。
//
// **添字の純関数である**: パケット i の中身は packets（総数）に依存しない
// ので、makeTSData(30) は makeTSData(50) のバイト単位の前置になる。2 つの
// 転送を区別可能なバイト列で比較したいテストは makeTSData ではなく
// makeTSDataFill を使う（issue #281 のレビュー指摘: 長さ比較に退化した
// バイト比較が rename を commit 前に移す変異を見逃していた）。
func makeTSData(packets int) []byte {
	data := make([]byte, packets*188)
	for i := 0; i < packets; i++ {
		off := i * 188
		data[off] = 0x47
		data[off+1] = 0x01
		data[off+2] = 0x00
		data[off+3] = 0x10 | byte(i%16)
	}
	return data
}

// makeTSDataFill は makeTSData と同じ TS パケットヘッダ（sync byte 0x47 等、
// 先頭 4 バイト）を持ちつつ、ペイロード（各パケットの残り 184
// バイト）を fill で塗りつぶした TS データを生成する。fill には 0x10〜0x1F
// （ヘッダ 4 バイト目が取りうる範囲）と衝突しない値を渡すこと。
//
// 2 本の並行 ingest を区別可能なバイト列で比較するために使う ---
// makeTSData(30) は makeTSData(50) のバイト単位の前置なので、長さの異なる
// makeTSData だけでは「先行ファイルが後発のバイトで上書きされていないか」を
// 全バイト比較しても実は前置一致で通ってしまう変異を見逃す。
func makeTSDataFill(fill byte) []byte {
	const packets = 30
	data := makeTSData(packets)
	for i := 0; i < packets; i++ {
		off := i * 188
		for j := off + 4; j < off+188; j++ {
			data[j] = fill
		}
	}
	return data
}

// ingestTestFile は ingest.go の ingestFile を差し替えるテストダブル。
// Work は正常系でも Close を 2 回呼ぶ（明示 Close 1 回 + 早期 return 用 defer
// 1 回。*os.File の 2 回目の Close は ErrClosed を返すだけで無害）ので、
// onClose は 2 回目を鳴らさないよう closed で冪等にする。
type ingestTestFile struct {
	file     *os.File
	syncErr  error
	closeErr error
	onSync   func()
	onClose  func()
	closed   bool
}

func (f *ingestTestFile) Write(p []byte) (int, error) {
	return f.file.Write(p)
}

func (f *ingestTestFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	if err := f.file.Sync(); err != nil {
		return err
	}
	if f.onSync != nil {
		f.onSync()
	}
	return nil
}

func (f *ingestTestFile) Close() error {
	err := f.file.Close()
	if !f.closed {
		f.closed = true
		if f.onClose != nil {
			f.onClose()
		}
	}
	if err != nil {
		return err
	}
	return f.closeErr
}

func TestIngestWorker_FullTransfer(t *testing.T) {
	tsData := makeTSData(100)
	fake := newFullTransferFake(t, "rec-001", tsData, "test/recording.m2ts")

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Precheck = pgstoretest.Media(pool)
	w.Publisher = pgstoretest.Media(pool)
	w.RecordTarget = pgstoretest.Recordings(pool)
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-001")

	job := IngestJobArgs{Site: "default", RecordID: "rec-001"}

	err := runWorkerViaJobQFake(context.Background(), w, job)
	if err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	// rel_path には args.Site（ここでは "default"）が前置される（issue #186 M4-14）。
	fullPath := filepath.Join(mediaDir, "sites", "default", "test", "recording.m2ts")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if len(data) != len(tsData) {
		t.Errorf("file size = %d, want %d", len(data), len(tsData))
	}

	if fake.CallCount(http.MethodDelete, "/api/recording/records/rec-001") == 0 {
		t.Error("edge record was not deleted after successful commit")
	}
}

// TestIngestWorker_SiteMismatch は、args.Site がワーカー自身の site
// （w.Site、`--sites` で束縛された site）と一致しないジョブを、mirakc に一切
// 触れずに fail-fast することを確認する（issue #139）。
//
// モックは何を投げても 200 を返す --- 「args.Site を無視する実装でも mirakc が
// 404 を返せば落ちる」形の弱いテストにしないため（issue #139 のテスト規律）。
// record_sync / recordings は「site-b」側で実際に存在する体で用意する
// （lookupRecordingID がここで成功してしまうと、ガードを外したときに
// GetRecord まで進んで初めてモックへ到達する、という現実の壊れ方を再現できる）。
func TestIngestWorker_SiteMismatch(t *testing.T) {
	fake := newRecordPathFake(t, "rec-mismatch", makeTSData(1), strPtr("mismatch/recording.m2ts"), "/recording/mismatch/recording.m2ts")

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(fake.URL(), nil)

	// このワーカープロセスは site-a の mirakc を向いている。
	w := &IngestWorker{
		MirakcClients: singleSiteClients("site-a", mc),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Precheck = pgstoretest.Media(pool)
	w.Publisher = pgstoretest.Media(pool)
	w.RecordTarget = pgstoretest.Recordings(pool)

	// site-b 側では実在する record_sync / recordings（site-b の watcher が
	// 実際に作った行という想定）。
	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "site-b", 20240101
	})
	insertTestRecordSyncForSite(t, pool, "site-b", recordingID, "rec-mismatch", 327361024000099)

	job := IngestJobArgs{Site: "site-b", RecordID: "rec-mismatch"}

	err := runWorkerViaJobQFake(context.Background(), w, job)
	if err == nil {
		t.Fatal("Work() error = nil, want error for site mismatch (site-a worker handling a site-b job)")
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("mirakc received %d requests, want 0 (guard must fail before touching mirakc): err=%v", len(calls), err)
	}
}

// TestIngestWorker_SiteMatch は、args.Site がワーカー自身の site と一致する
// ジョブは従来どおり処理されることを確認する（TestIngestWorker_SiteMismatch と
// 対になる、両方向の確認。CLAUDE.md テスト規律「分岐を直したら両方向で確認する」）。
func TestIngestWorker_SiteMatch(t *testing.T) {
	tsData := makeTSData(20)
	fake := newFullTransferFake(t, "rec-match", tsData, "match/recording.m2ts")

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		MirakcClients: singleSiteClients("site-a", mc),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Precheck = pgstoretest.Media(pool)
	w.Publisher = pgstoretest.Media(pool)
	w.RecordTarget = pgstoretest.Recordings(pool)
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "site-a", 20240102
	})
	insertTestRecordSyncForSite(t, pool, "site-a", recordingID, "rec-match", 327361024000098)

	job := IngestJobArgs{Site: "site-a", RecordID: "rec-match"}

	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work() error = %v, want nil for matching site", err)
	}

	// rel_path には args.Site（ここでは "site-a"）が前置される（issue #186 M4-14）。
	fullPath := filepath.Join(mediaDir, "sites", "site-a", "match", "recording.m2ts")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if len(data) != len(tsData) {
		t.Errorf("file size = %d, want %d", len(data), len(tsData))
	}
}

func TestIngestWorker_WriteErrorDoesNotRetryMirakc(t *testing.T) {
	injectedErr := errors.New("injected local write failure")
	fake := mirakctest.New(t)
	fake.SetRecordStream("record", []byte("recorded"))

	worker := &IngestWorker{StallTimeout: time.Second}
	_, _, _, err := worker.transferIngestRecord(
		context.Background(),
		mirakc.NewClient(fake.URL(), nil),
		"record",
		partialErrorWriter{n: 0, err: injectedErr},
		nil,
	)
	if !errors.Is(err, injectedErr) {
		t.Fatalf("transferIngestRecord() error = %v, want local write error", err)
	}
	if got := fake.CallCount(http.MethodGet, "/api/recording/records/record/stream"); got != 1 {
		t.Errorf("Range requests = %d, want 1 after the destination failed", got)
	}
	if got := fake.CallCount(http.MethodGet, "/api/recording/records/record"); got != 0 {
		t.Errorf("GetRecord requests = %d, want 0 after the destination failed", got)
	}
}

func TestIngestWorker_CapturesSHA256OnlyAfterFinished(t *testing.T) {
	provisionalSHA256 := strings.Repeat("a", sha256.Size*2)
	var rangeRequests atomic.Int32
	var statusRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			rangeRequests.Add(1)
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			status := "recording"
			var contentSHA256 *string
			if statusRequests.Add(1) == 1 {
				contentSHA256 = &provisionalSHA256
			} else {
				status = "finished"
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(mirakc.Record{
				Recording: mirakc.RecordInfo{Status: status},
				Content:   mirakc.ContentInfo{Sha256: contentSHA256},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	// Keep progress observation in memory; this test only checks the status-to-hash rule.
	progress := &ingestProgressReporter{interval: time.Hour, lastAt: time.Now(), log: slog.Default()}
	worker := &IngestWorker{StallTimeout: time.Second}
	offset, gotSHA256, _, err := worker.transferIngestRecord(
		context.Background(), mirakc.NewClient(srv.URL, nil), "record", io.Discard, progress,
	)
	if err != nil {
		t.Fatalf("transferIngestRecord() error = %v", err)
	}
	if offset != 0 {
		t.Errorf("transferred offset = %d, want 0", offset)
	}
	if gotSHA256 != nil {
		t.Errorf("SHA-256 captured while recording = %q, want nil because finished had no hash", *gotSHA256)
	}
	if got, want := rangeRequests.Load(), int32(3); got != want {
		t.Errorf("Range requests = %d, want %d (recording catch-up, finished catch-up, final Range)", got, want)
	}
}

func TestIngestWorker_MidTransferDisconnect(t *testing.T) {
	tsData := makeTSData(100)
	cutoff := 50 * 188

	var attempt atomic.Int32
	// resumedAt は「書き込み済みオフセットから再開した」リクエストのオフセット。
	// attempt の回数だけでは足りない --- 追従ループは finished 後の drain でも
	// もう 1 回 /stream を叩くので、回数は再接続の証拠にならない。
	var resumedAt atomic.Int64
	resumedAt.Store(-1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			offset := mirakctest.RangeOffset(r)
			if offset == int64(cutoff) {
				resumedAt.Store(offset)
			}

			if attempt.Add(1) == 1 && offset == 0 {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(tsData[:cutoff])
				return
			}

			remaining := tsData[offset:]
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(remaining)))
			if offset > 0 {
				w.WriteHeader(http.StatusPartialContent)
			} else {
				w.WriteHeader(http.StatusPartialContent)
			}
			_, _ = w.Write(remaining)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Status:  "finished",
					Options: mirakc.Options{ContentPath: strPtr("test/partial.m2ts")},
				},
				Content: mirakc.ContentInfo{Path: "/recording/test/partial.m2ts", Sha256: strPtr(sha256Hex(tsData))},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		case r.Method == http.MethodDelete:
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Precheck = pgstoretest.Media(pool)
	w.Publisher = pgstoretest.Media(pool)
	w.RecordTarget = pgstoretest.Recordings(pool)
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-partial")

	job := IngestJobArgs{Site: "default", RecordID: "rec-partial"}

	err := runWorkerViaJobQFake(context.Background(), w, job)
	if err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	// rel_path には args.Site（ここでは "default"）が前置される（issue #186 M4-14）。
	fullPath := filepath.Join(mediaDir, "sites", "default", "test", "partial.m2ts")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if len(data) != len(tsData) {
		t.Errorf("file size = %d, want %d", len(data), len(tsData))
	}

	if got := resumedAt.Load(); got != int64(cutoff) {
		t.Errorf("Range 再開のオフセット = %d, want %d（切断後に書き込み済みオフセットから再開していない）", got, cutoff)
	}
	if attempt.Load() < 2 {
		t.Errorf("expected at least 2 stream attempts, got %d", attempt.Load())
	}
}

// TestIngestWorker_FollowsRecordingWithRangePolling は、Range 応答がリクエスト時点
// で有限である mirakc の録画中挙動を再現する。差分を読み切った時点では commit
// せず、recording.status を再取得して待ち、finished を観測してから最後の空 Range
// まで drain することを固定する。
//
// 固定するのは「差分を読み切っただけでは commit しない」ことである。**追い付き
// （416）が録画中に来て、その後に録画が伸びる**経路は
// TestIngestWorker_CatchUpWhileRecordingDoesNotCommit が持つ（このテストの 416 は
// finished を観測した後なので、416 を無条件に完了とみなす変異がここでは通る）。
// 204 と 206 の 0 バイトは
// TestIngestWorker_NotReadyAndEmptyBodyDoNotConsumeRetries が持つ。
// 「recording 中に待たない」は TestIngestWorker_FollowPollingIsRateLimited が持つ。
func TestIngestWorker_FollowsRecordingWithRangePolling(t *testing.T) {
	setFastIngestFollow(t)
	full := makeTSData(10)
	first := full[:5*188]
	second := full[5*188:]
	var rangeRequests atomic.Int32
	var statusRequests atomic.Int32
	var status atomic.Value
	status.Store("recording")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			rangeRequests.Add(1)
			offset := mirakctest.RangeOffset(r)
			switch offset {
			case 0:
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(first)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(first)
			case int64(len(first)):
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(second)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(second)
			default:
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			}
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			statusRequests.Add(1)
			currentStatus := status.Load().(string)
			var contentSHA256 *string
			if currentStatus == "finished" {
				contentSHA256 = strPtr(sha256Hex(full))
			}
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Status: currentStatus, Options: mirakc.Options{ContentPath: strPtr("test/follow.m2ts")}},
				Content:   mirakc.ContentInfo{Path: "/recording/test/follow.m2ts", Sha256: contentSHA256},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)
			// Work の初回 lookup の後、最初の catch-up 観測で finished に切り替える。
			if statusRequests.Load() >= 2 {
				status.Store("finished")
			}
		case r.Method == http.MethodDelete:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		MediaDir:      t.TempDir(),
		StallTimeout:  time.Second,
		RecordTarget:  pgstoretest.Recordings(pool),
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-follow")
	if err := runWorkerViaJobQFake(context.Background(), w, IngestJobArgs{Site: "default", RecordID: "rec-follow"}); err != nil {
		t.Fatalf("Work(): %v", err)
	}
	path := filepath.Join(w.MediaDir, "sites", "default", "test", "follow.m2ts")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading committed file: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("committed bytes differ from the two Range segments")
	}
	if rangeRequests.Load() != 5 {
		t.Errorf("Range requests = %d, want 5 (two data ranges + two status observations + final empty range)", rangeRequests.Load())
	}
	if statusRequests.Load() != 3 {
		t.Errorf("status requests = %d, want 3 (initial lookup + two catch-up observations)", statusRequests.Load())
	}
}

// TestIngestWorker_FollowPollingIsRateLimited は、ingest が放送より速いときでも
// mirakc へのリクエストがポーリング間隔で律速されることを固定する。
//
// 待ちを「差分が 0 バイトだったときだけ」にすると、放送が続いて毎回わずかに
// 差分が返る定常状態でリクエストが往復時間で律速され、1 秒に数十〜百回
// mirakc を叩く。recording のあいだは差分の有無に関わらず待つ。
func TestIngestWorker_FollowPollingIsRateLimited(t *testing.T) {
	data := makeTSData(200)
	const packetsPerPoll = 5

	var size atomic.Int64
	var rangeTimesMu sync.Mutex
	var rangeTimes []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			rangeTimesMu.Lock()
			rangeTimes = append(rangeTimes, time.Now())
			rangeTimesMu.Unlock()
			// 放送は進み続ける（毎ポーリングで差分が出る）。
			cur := size.Add(packetsPerPoll * 188)
			mirakctest.WriteRecordStream(w, r, data[:cur])
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", size.Load()))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Status: "recording", Options: mirakc.Options{ContentPath: strPtr("test/rate.m2ts")}},
				Content:   mirakc.ContentInfo{Path: "/recording/test/rate.m2ts"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		MediaDir:      t.TempDir(),
		StallTimeout:  time.Second,
		RecordTarget:  pgstoretest.Recordings(pool),
		Precheck:      pgstoretest.Media(pool),
	}
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-rate")

	const window = 1200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	err := runWorkerViaJobQFake(ctx, w, IngestJobArgs{Site: "default", RecordID: "rec-rate"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Work() error = %v, want context deadline error", err)
	}

	// At least two starts prove the reader stayed in its polling loop. Each start
	// must be separated by the shared 500ms minimum; without the wait this loop
	// can issue dozens of requests during the same window.
	rangeTimesMu.Lock()
	starts := append([]time.Time(nil), rangeTimes...)
	rangeTimesMu.Unlock()
	if len(starts) < 2 {
		t.Fatalf("Range requests = %d in %s, want at least 2 to exercise polling", len(starts), window)
	}
	if len(starts) > 4 {
		t.Errorf("Range requests = %d in %s, want at most 4 with the shared poll interval", len(starts), window)
	}
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(starts[i-1]); gap < 450*time.Millisecond {
			t.Errorf("Range request gap = %v, want at least 450ms", gap)
		}
	}

	// 録画中なので commit していない。
	var assets int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID).Scan(&assets); err != nil {
		t.Fatalf("counting media_assets: %v", err)
	}
	if assets != 0 {
		t.Errorf("media_assets rows = %d, want 0 (nothing may commit while recording)", assets)
	}
}

// TestIngestWorker_CatchUpWhileRecordingDoesNotCommit は、production の追い付き経路
// そのものを固定する: **録画中に 416（= 現在のサイズに追い付いた）が返り、その後
// さらに録画が伸びて、最後に finished になる**。
//
// これが無いと「416 を record の状態を見ずに完了とみなす」変異が通る（最初の 416 の
// 時点ではまだ 5 パケットしか無いので、HEAD 照合で size mismatch になり落ちる）。
func TestIngestWorker_CatchUpWhileRecordingDoesNotCommit(t *testing.T) {
	setFastIngestFollow(t)
	full := makeTSData(20)

	// /records/{id} の観測回数で「いまの content サイズ」と status を決める。
	// 同じサイズを 2 回続けて返すところが「追い付いた」状態になる。
	// 1 回目は determineRelPath の GetRecord である。
	sizes := []int{5, 5, 10, 10, 20}
	var recordGets atomic.Int32
	var rangeRequests atomic.Int32
	lastSize := func() int {
		n := int(recordGets.Load())
		if n < 1 {
			n = 1
		}
		if n > len(sizes) {
			n = len(sizes)
		}
		return sizes[n-1] * 188
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			rangeRequests.Add(1)
			mirakctest.WriteRecordStream(w, r, full[:lastSize()])
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			n := recordGets.Add(1)
			st := "recording"
			if int(n) >= len(sizes) {
				st = "finished"
			}
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Status: st, Options: mirakc.Options{ContentPath: strPtr("test/catchup.m2ts")}},
				Content:   mirakc.ContentInfo{Path: "/recording/test/catchup.m2ts", Sha256: strPtr(sha256Hex(full))},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)
		case r.Method == http.MethodDelete:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		MediaDir:      t.TempDir(),
		StallTimeout:  time.Second,
		RecordTarget:  pgstoretest.Recordings(pool),
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-catchup")
	if err := runWorkerViaJobQFake(context.Background(), w, IngestJobArgs{Site: "default", RecordID: "rec-catchup"}); err != nil {
		t.Fatalf("Work(): %v", err)
	}
	got, err := os.ReadFile(filepath.Join(w.MediaDir, "sites", "default", "test", "catchup.m2ts"))
	if err != nil {
		t.Fatalf("reading committed file: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("committed bytes = %d, want %d (追い付きの後で伸びたぶんが欠けている)", len(got), len(full))
	}
	// 5 パケットへの追い付きで止まっていれば Range は 3 回以下で終わる。
	if rangeRequests.Load() < 5 {
		t.Errorf("Range requests = %d, want >= 5 (追い付きのたびに打ち切っていない)", rangeRequests.Load())
	}
}

// TestIngestWorker_CanceledOrFailedRecordCancelsJobWithoutRetry は、録画中に
// observe した mirakc record が canceled / failed へ遷移したとき、Work が
// jobq.ErrDiscard でジョブを終端し、temp を破棄して再試行へ戻さないことを
// 固定する。
//
// jobq.ErrDiscard を返すこと・進捗行が消えること・原本 media_asset が
// 作られていない（部分ファイルを commit していない）こと・エッジの record を
// 削除しないことの 4 点を固定する。
// 単に「error が非 nil」だけを見るテストにはしない --- それでは
// jobq.ErrDiscard を外して素の error を返すだけの変異も通ってしまう。
func TestIngestWorker_CanceledOrFailedRecordCancelsJobWithoutRetry(t *testing.T) {
	for _, tt := range []struct {
		name        string
		status      string
		recordID    string
		contentPath string
		jobID       int64
	}{
		{name: "canceled", status: "canceled", recordID: "rec-canceled", contentPath: "test/canceled.m2ts", jobID: 425010},
		{name: "failed", status: "failed", recordID: "rec-failed", contentPath: "test/failed.m2ts", jobID: 425011},
	} {
		t.Run(tt.name, func(t *testing.T) {

			var recordGets, deletes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodDelete:
					// 成功を返す。404 だと DeleteRecord の失敗がログに落ちるだけで、
					// 呼んだこと自体が観測できない。
					deletes.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"recordRemoved":true,"contentRemoved":true}`))
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
					// 追い付いた状態のまま：このテストは status 遷移だけを見る。
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
					n := recordGets.Add(1)
					status := "recording"
					if n >= 2 {
						status = tt.status
					}
					record := mirakc.Record{
						Recording: mirakc.RecordInfo{Status: status, Options: mirakc.Options{ContentPath: strPtr(tt.contentPath)}},
						Content:   mirakc.ContentInfo{Path: "/recording/" + tt.contentPath},
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(record)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)

			pool := setupTestPool(t)
			if pool == nil {
				return
			}
			w := &IngestWorker{
				Publisher:     pgstoretest.Media(pool),
				MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
				MediaDir:      t.TempDir(),
				StallTimeout:  time.Second,
				RecordTarget:  pgstoretest.Recordings(pool),
				Precheck:      pgstoretest.Media(pool),
			}
			recordingID := dbtest.Recording(t, pool)
			insertTestRecordSync(t, pool, recordingID, tt.recordID)

			before := ingestJobResults()
			err := runWorkerViaJobQFake(context.Background(), w, IngestJobArgs{Site: "default", RecordID: tt.recordID})
			if err == nil {
				t.Fatal("Work() = nil, want jobq.ErrDiscard")
			}
			// 取り消された録画は canceled にだけ乗る。failure に混ぜると、利用者が
			// 止めた録画が失敗率に積まれて本物の失敗が埋もれる。
			assertIngestResultDeltas(t, before, map[string]float64{"canceled": 1}, tt.name)
			if !errors.Is(err, jobq.ErrDiscard) {
				t.Fatalf("Work() = %v (%T), want jobq.ErrDiscard (job must be terminated, not retried)", err, err)
			}

			var progressRows int
			if err := pool.QueryRow(context.Background(),
				"SELECT count(*) FROM recording_ingest_progress WHERE recording_id = $1", recordingID,
			).Scan(&progressRows); err != nil {
				t.Fatalf("counting recording_ingest_progress: %v", err)
			}
			if progressRows != 0 {
				t.Errorf("recording_ingest_progress rows = %d, want 0 (stale progress row must be cleared on cancel/fail)", progressRows)
			}

			var assetRows int
			if err := pool.QueryRow(context.Background(),
				"SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'original'", recordingID,
			).Scan(&assetRows); err != nil {
				t.Fatalf("counting media_assets: %v", err)
			}
			if assetRows != 0 {
				t.Errorf("media_assets rows = %d, want 0 (must not commit a partial recording)", assetRows)
			}
			if n := deletes.Load(); n != 0 {
				t.Errorf("DELETE requests = %d, want 0 (edge record must be kept: purging it can remove a successor's content file)", n)
			}
			tempPath := ingestTempFilePath(filepath.Join(w.MediaDir, "sites", "default", "test"), "default", tt.recordID)
			if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("ingest temp after %s record termination: err=%v, want os.ErrNotExist", tt.name, err)
			}
		})
	}
}

// TestIngestWorker_NotReadyAndEmptyBodyDoNotConsumeRetries は、録画開始直後に必ず出る
// 204（content file がまだ空）と、追い付いた状態の 206 + 0 バイトを、接続失敗として
// 数えないことを固定する。どちらも mirakc.MaxConsecutiveRetries（5）を超えて続けてから
// finished になる。
//
// 数えるように変異させると、正常な録画が 6 回目のポーリングで失敗する。
func TestIngestWorker_NotReadyAndEmptyBodyDoNotConsumeRetries(t *testing.T) {
	setFastIngestFollow(t)
	full := makeTSData(10)
	const notReadyPolls = 8
	const emptyBodyPolls = 8

	var streamRequests atomic.Int32
	var status atomic.Value
	status.Store("recording")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			n := streamRequests.Add(1)
			switch {
			case int(n) <= notReadyPolls:
				w.WriteHeader(http.StatusNoContent)
			case int(n) <= notReadyPolls+emptyBodyPolls:
				// 追い付いた状態の 206。本文は 0 バイト。
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(http.StatusPartialContent)
			default:
				mirakctest.WriteRecordStream(w, r, full)
			}
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			// 空本文のポーリングが終わったところで finished にする。
			if streamRequests.Load() > int32(notReadyPolls+emptyBodyPolls) {
				status.Store("finished")
			}
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Status: status.Load().(string), Options: mirakc.Options{ContentPath: strPtr("test/notready.m2ts")}},
				Content:   mirakc.ContentInfo{Path: "/recording/test/notready.m2ts", Sha256: strPtr(sha256Hex(full))},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)
		case r.Method == http.MethodDelete:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		MediaDir:      t.TempDir(),
		StallTimeout:  time.Second,
		RecordTarget:  pgstoretest.Recordings(pool),
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-notready")
	if err := runWorkerViaJobQFake(context.Background(), w, IngestJobArgs{Site: "default", RecordID: "rec-notready"}); err != nil {
		t.Fatalf("Work() = %v, want nil（204 と 206/0 は接続失敗ではない）", err)
	}
	got, err := os.ReadFile(filepath.Join(w.MediaDir, "sites", "default", "test", "notready.m2ts"))
	if err != nil {
		t.Fatalf("reading committed file: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("committed bytes = %d, want %d", len(got), len(full))
	}
	if got := streamRequests.Load(); int(got) <= notReadyPolls+emptyBodyPolls {
		t.Errorf("stream requests = %d, want > %d (変異で短絡していない)", got, notReadyPolls+emptyBodyPolls)
	}
}

// TestIngestWorker_FollowingCaughtUpKeepsProgressFresh は、追い付いた状態の
// ポーリングが進捗行を古びさせないことを固定する。
//
// 追い付き（416 / 206 の 0 バイト）は差分を書かないので、observed_at を
// 「バイトを書けたときだけ」進めると、**正常に追従できている録画ほど**
// UI の停滞判定（60 秒）に引っかかって「取り込み中（停滞）」と表示される。
//
// 分母も同時に更新する。Work 開始時の record_sync.content_length を固定すると、
// 録画が伸びて written が古い分母を追い越した時点で UI が「取り込み中 100%」を
// 出し続ける（Web 側は min(100, ...) で頭打ちにするので嘘が % として出る）。
func TestIngestWorker_FollowingCaughtUpKeepsProgressFresh(t *testing.T) {
	setFastIngestFollow(t)
	const staleDenominator = 100

	var statusGets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			// 常に追い付いた状態（読むものが無い）。
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			n := statusGets.Add(1)
			length := uint64(n) * 1000
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Status: "recording", Options: mirakc.Options{ContentPath: strPtr("test/fresh.m2ts")}},
				Content:   mirakc.ContentInfo{Path: "/recording/test/fresh.m2ts", Length: &length},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w := &IngestWorker{
		Publisher:        pgstoretest.Media(pool),
		MirakcClients:    singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		MediaDir:         t.TempDir(),
		StallTimeout:     time.Second,
		RecordTarget:     pgstoretest.Recordings(pool),
		ProgressInterval: 10 * time.Millisecond,
		Precheck:         pgstoretest.Media(pool),
	}
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-fresh")
	setRecordSyncContentLength(t, pool, "rec-fresh", staleDenominator)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWorkerViaJobQFake(ctx, w, IngestJobArgs{Site: "default", RecordID: "rec-fresh"})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("worker cleanup: got %v, want context.Canceled", err)
		}
	})

	// 進捗行ができ、いくつかポーリングが回るまで待つ。
	var firstObservedAt time.Time
	deadline := time.Now().Add(5 * time.Second)
	for {
		if statusGets.Load() >= 3 {
			if err := pool.QueryRow(context.Background(),
				`SELECT observed_at FROM recording_ingest_progress WHERE recording_id = $1`, recordingID,
			).Scan(&firstObservedAt); err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("進捗行が観測できなかった（statusGets=%d）", statusGets.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 追い付いたまま更にポーリングさせる。
	time.Sleep(1200 * time.Millisecond)

	var observedAt time.Time
	var expected *int64
	var written int64
	if err := pool.QueryRow(context.Background(),
		`SELECT observed_at, expected_bytes, written_bytes FROM recording_ingest_progress WHERE recording_id = $1`, recordingID,
	).Scan(&observedAt, &expected, &written); err != nil {
		t.Fatalf("reading progress row: %v", err)
	}
	if !observedAt.After(firstObservedAt) {
		t.Errorf("observed_at = %s, want > %s（追い付いたまま古びている。UI は 60 秒で「停滞」と読む）",
			observedAt, firstObservedAt)
	}
	if expected == nil {
		t.Fatal("expected_bytes が nil")
	}
	if *expected <= staleDenominator {
		t.Errorf("expected_bytes = %d, want > %d（分母が Work 開始時の値で凍結している）", *expected, staleDenominator)
	}
	if written != 0 {
		t.Errorf("written_bytes = %d, want 0（1 バイトも書いていない）", written)
	}
}

// TestIngestWorker_UnknownStatusRetriesInJobWithoutRestart は、未知の status を
// 観測したときに **River へ戻さず reader 内で再試行し、offset を捨てない**ことを
// 固定する。
//
// 共有 reader は未知の status を終端と誤認せず、同じ offset から Range を再試行する。
// 未知の status は一過性かもしれない --- mirakc が一時的に応答を欠いたとき status が空になりうる。
//
// 変異「未知の status で即 error を返す」は Work() が非 nil になって落ちる。
// 変異「未知の status を終端 sentinel に混ぜる」も同じく落ちる。
func TestIngestWorker_UnknownStatusRetriesInJobWithoutRestart(t *testing.T) {
	setFastIngestFollow(t)
	full := makeTSData(10)

	var recordGets atomic.Int32
	var zeroOffsetRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			offset := mirakctest.RangeOffset(r)
			if offset == 0 {
				zeroOffsetRequests.Add(1)
			}
			mirakctest.WriteRecordStream(w, r, full)
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			n := recordGets.Add(1)
			status := ""
			if n >= 4 {
				status = "finished"
			}
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Status: status, Options: mirakc.Options{ContentPath: strPtr("test/unknown.m2ts")}},
				Content:   mirakc.ContentInfo{Path: "/recording/test/unknown.m2ts", Sha256: strPtr(sha256Hex(full))},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)
		case r.Method == http.MethodDelete:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		MediaDir:      t.TempDir(),
		StallTimeout:  time.Second,
		RecordTarget:  pgstoretest.Recordings(pool),
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-unknown")
	if err := runWorkerViaJobQFake(context.Background(), w, IngestJobArgs{Site: "default", RecordID: "rec-unknown"}); err != nil {
		t.Fatalf("Work() = %v, want nil（未知の status はジョブ内で再試行する）", err)
	}
	got, err := os.ReadFile(filepath.Join(w.MediaDir, "sites", "default", "test", "unknown.m2ts"))
	if err != nil {
		t.Fatalf("reading committed file: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("committed bytes = %d, want %d", len(got), len(full))
	}
	if got := zeroOffsetRequests.Load(); got != 1 {
		t.Errorf("offset 0 の Range 要求 = %d, want 1（未知の status で再起動相当の再転送をしている）", got)
	}
}

// TestIngestWorker_PermanentlyUnknownStatusIsBounded は、未知の status が続いても
// ジョブ内の再試行が上限で止まり、無限にポーリングしないことを固定する。
//
// **連続失敗カウンタのリセット位置**を固定するテストである。リセットを
// RecordFollowReader が未知の status で失敗回数を戻すと、恒久的な異常応答でも
// 上限に達しないまま回り続ける。
func TestIngestWorker_PermanentlyUnknownStatusIsBounded(t *testing.T) {
	setFastIngestFollow(t)

	const recordID = "rec-forever-unknown"
	fake := newMirakcRecordFake(t, mirakc.Record{
		ID: recordID,
		Recording: mirakc.RecordInfo{
			Status: "", Options: mirakc.Options{ContentPath: strPtr("test/forever-unknown.m2ts")},
		},
		Content: mirakc.ContentInfo{Path: "/recording/test/forever-unknown.m2ts"},
	}, []byte{})

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
		MediaDir:      t.TempDir(),
		StallTimeout:  time.Second,
		RecordTarget:  pgstoretest.Recordings(pool),
		Precheck:      pgstoretest.Media(pool),
	}
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, recordID)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// 上限（mirakc.MaxConsecutiveRetries = 5）に達すれば数秒で error を返す。返らなければ
	// カウンタがリセットされ続けている。
	if err := runWorkerViaJobQFake(ctx, w, IngestJobArgs{Site: "default", RecordID: recordID}); err == nil {
		t.Fatal("Work() = nil, want an error（未知の status が続いたら上限で止まる）")
	}
	// determineRelPath の 1 回 + 上限までの 6 回。余裕を見て 10 で切る。
	if got := fake.CallCount(http.MethodGet, "/api/recording/records/"+recordID); got > 10 {
		t.Errorf("status の観測回数 = %d, want <= 10（連続失敗カウンタがリセットされ続けている）", got)
	}
}

func TestIngestWorker_SizeMismatch(t *testing.T) {
	tsData := makeTSData(50)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			mirakctest.WriteRecordStream(w, r, tsData)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			// HEAD が異なるサイズを返す → size mismatch
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)+1000))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Status:  "finished",
					Options: mirakc.Options{ContentPath: strPtr("test/mismatch.m2ts")},
				},
				Content: mirakc.ContentInfo{Path: "/recording/test/mismatch.m2ts"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Precheck = pgstoretest.Media(pool)
	w.Publisher = pgstoretest.Media(pool)
	w.RecordTarget = pgstoretest.Recordings(pool)

	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-mismatch")

	job := IngestJobArgs{Site: "default", RecordID: "rec-mismatch"}

	// 即座に失敗で終わる経路（再試行の予算を使い切るまで 500 を返す経路は約 6 秒
	// かかるので使わない）で、本当の失敗が failure として数えられることを固定する。
	before := ingestJobResults()
	err := runWorkerViaJobQFake(context.Background(), w, job)
	if err == nil {
		t.Fatal("expected error for size mismatch, got nil")
	}
	assertIngestResultDeltas(t, before, map[string]float64{"failure": 1}, "size mismatch")
	if !strings.Contains(err.Error(), "size mismatch") {
		t.Errorf("expected 'size mismatch' error, got: %v", err)
	}
	tempPath := ingestTempFilePath(filepath.Join(mediaDir, "sites", "default", "test"), "default", "rec-mismatch")
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ingest temp after size mismatch: stat error = %v, want not exist", err)
	}
}

func TestIngestWorker_HashMismatch(t *testing.T) {
	tsData := makeTSData(50)
	wrongData := bytes.Clone(tsData)
	wrongData[len(wrongData)-1] ^= 0xff
	const recordID = "rec-hash-mismatch"
	expectedSHA256 := sha256Hex(wrongData)
	fake := newSHA256IngestFake(t, recordID, tsData, "test/hash-mismatch.m2ts", &expectedSHA256)

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, recordID)

	mediaDir := t.TempDir()
	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
		RecordTarget:  pgstoretest.Recordings(pool),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	job := IngestJobArgs{Site: "default", RecordID: recordID}

	err := runWorkerViaJobQFake(context.Background(), w, job)
	if err == nil {
		t.Fatal("expected error for hash mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Errorf("expected 'hash mismatch' error, got: %v", err)
	}

	var assetCount int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID,
	).Scan(&assetCount); err != nil {
		t.Fatalf("counting media_assets after hash mismatch: %v", err)
	}
	if assetCount != 0 {
		t.Errorf("media_assets rows after hash mismatch = %d, want 0", assetCount)
	}
	if got := fake.CallCount(http.MethodDelete, "/api/recording/records/"+recordID); got != 0 {
		t.Errorf("DeleteRecord attempts after hash mismatch = %d, want 0", got)
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "sites", "default", "test", "hash-mismatch.m2ts")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("canonical file after hash mismatch: stat error = %v, want not exist", err)
	}
	tempPath := ingestTempFilePath(filepath.Join(mediaDir, "sites", "default", "test"), "default", "rec-hash-mismatch")
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ingest temp after hash mismatch: stat error = %v, want not exist", err)
	}
}

type lateSHA256IngestCase struct {
	name               string
	lateHash           func([]byte) string
	wantError          string
	wantAssets         int
	wantDeleteAttempts int32
	wantResult         string
	wantVerification   string
	// wantFinalSyncs は 3 本目の job が temp を Sync する回数（pending の 2 本は別に固定）。
	wantFinalSyncs int32
}

type lateSHA256IngestRun struct {
	worker       *IngestWorker
	pool         *pgxpool.Pool
	recordingID  int64
	fake         *mirakctest.Fake
	mediaDir     string
	recordID     string
	tsData       []byte
	contentPath  string
	setHash      func(*string)
	tempSyncs    *atomic.Int32
	logOutput    *bytes.Buffer
	tempModTime  time.Time
	firstOffsets []int64
}

func TestIngestWorker_CompletesUntilLateContentSHA256(t *testing.T) {
	setFastIngestFollow(t)
	for _, tc := range []lateSHA256IngestCase{
		{
			name:               "hash arrives and verifies",
			lateHash:           sha256Hex,
			wantAssets:         1,
			wantDeleteAttempts: 1,
			wantResult:         "success",
			wantVerification:   "sha256_verification=verified",
			wantFinalSyncs:     1, // commit 直前の Sync のみ
		},
		{
			name: "late hash mismatch",
			lateHash: func(data []byte) string {
				wrong := bytes.Clone(data)
				wrong[len(wrong)-1] ^= 0xff
				return sha256Hex(wrong)
			},
			wantError:  "hash mismatch",
			wantResult: "failure",
		},
		{
			name:               "timeout skips verification",
			wantAssets:         1,
			wantDeleteAttempts: 1,
			wantResult:         "success",
			wantVerification:   "sha256_verification=timeout_skipped",
			wantFinalSyncs:     1, // 期限切れなら待つ前の Sync はせず、commit 直前の 1 回だけ
		},
	} {
		t.Run(tc.name, func(t *testing.T) { runLateSHA256IngestCase(t, tc) })
	}
}

func runLateSHA256IngestCase(t *testing.T, tc lateSHA256IngestCase) {
	t.Helper()
	run := newLateSHA256IngestRun(t, tc)
	if run == nil {
		return
	}
	beforeResults := ingestJobResults()
	durationBefore := ingestDurationSamples(t)
	mismatchesBefore := promtestutil.ToFloat64(metrics.IngestHashMismatches)
	run.runPending(t, true, beforeResults, durationBefore)
	run.runPending(t, false, beforeResults, durationBefore)
	syncsBefore := run.tempSyncs.Load()
	thirdErr := run.complete(t, tc)
	if got := run.tempSyncs.Load() - syncsBefore; got != tc.wantFinalSyncs {
		t.Errorf("temp Sync calls in the final job = %d, want %d", got, tc.wantFinalSyncs)
	}
	run.assertCompletion(t, tc, beforeResults, durationBefore, mismatchesBefore, thirdErr)
}

type pacedIngestResponseWriter struct {
	http.ResponseWriter
	chunkSize int
	delay     time.Duration
}

func (w *pacedIngestResponseWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(len(p), w.chunkSize)
		written, err := w.ResponseWriter.Write(p[:n])
		total += written
		p = p[written:]
		if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
			flusher.Flush()
		}
		if err != nil {
			return total, err
		}
		if len(p) > 0 {
			time.Sleep(w.delay)
		}
	}
	return total, nil
}

func TestIngestWorker_SnoozesAndResumesSameJobFromCheckpoint(t *testing.T) {
	setFastIngestFollow(t)
	oldSlice := ingestTransferSlice
	ingestTransferSlice = 100 * time.Millisecond
	t.Cleanup(func() { ingestTransferSlice = oldSlice })

	data := makeTSData(700)
	contentPath := "test/sliced-transfer.m2ts"
	expectedHash := sha256Hex(data)
	var rangeMu sync.Mutex
	var offsets []int64
	var deleteAttempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			rangeMu.Lock()
			offsets = append(offsets, mirakctest.RangeOffset(r))
			rangeMu.Unlock()
			mirakctest.WriteRecordStream(&pacedIngestResponseWriter{ResponseWriter: w, chunkSize: 8 * 1024, delay: 10 * time.Millisecond}, r, data)
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Status: "finished", Options: mirakc.Options{ContentPath: strPtr(contentPath)}},
				Content:   mirakc.ContentInfo{Path: "/recording/" + contentPath, Sha256: &expectedHash},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(record)
		case r.Method == http.MethodDelete:
			deleteAttempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	recordingID := dbtest.Recording(t, pool)
	const recordID = "rec-sliced-transfer"
	insertTestRecordSync(t, pool, recordingID, recordID)
	mediaDir := t.TempDir()
	worker := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		RecordTarget:  pgstoretest.Recordings(pool),
		MediaDir:      mediaDir,
		StallTimeout:  time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	worker.CMDetectionStore = pgstoretest.Analysis(pool)
	job := IngestJobArgs{Site: "default", RecordID: recordID}

	firstErr := runWorkerViaJobQFake(context.Background(), worker, job)
	if _, ok := jobq.SnoozeDelay(firstErr); !ok {
		t.Fatalf("first Work() error = %v, want slice snooze", firstErr)
	}
	tempPath := ingestTempFilePath(filepath.Join(mediaDir, "sites", "default", "test"), "default", recordID)
	firstTemp, err := os.ReadFile(tempPath)
	if err != nil {
		t.Fatalf("reading temp after first slice: %v", err)
	}
	if len(firstTemp) == 0 || len(firstTemp) >= len(data) {
		t.Fatalf("first slice temp size = %d, want a nonempty partial transfer of %d bytes", len(firstTemp), len(data))
	}
	if _, err := os.Stat(ingestCheckpointPath(tempPath)); err != nil {
		t.Fatalf("checkpoint after first slice: %v", err)
	}

	completed := false
	for slice := 1; slice <= 5; slice++ {
		err := runWorkerViaJobQFake(context.Background(), worker, job)
		if err == nil {
			completed = true
			break
		}
		if _, ok := jobq.SnoozeDelay(err); !ok {
			t.Fatalf("resumed Work() error = %v, want another slice snooze or completion", err)
		}
	}
	if !completed {
		t.Fatal("same job did not complete within five resumed slices")
	}
	canonicalPath := filepath.Join(mediaDir, "sites", "default", contentPath)
	got, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("reading committed canonical file: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("committed file differs from the single source byte stream")
	}
	if gotHash := sha256Hex(got); gotHash != expectedHash {
		t.Fatalf("committed SHA-256 = %s, want %s", gotHash, expectedHash)
	}
	rangeMu.Lock()
	gotOffsets := slices.Clone(offsets)
	rangeMu.Unlock()
	if len(gotOffsets) < 2 || gotOffsets[0] != 0 || gotOffsets[1] != int64(len(firstTemp)) {
		t.Errorf("Range offsets across slice resume = %v, want first resume at %d", gotOffsets, len(firstTemp))
	}
	if got := deleteAttempts.Load(); got != 1 {
		t.Errorf("DeleteRecord attempts = %d, want 1 after final commit", got)
	}
	if _, err := os.Stat(ingestCheckpointPath(tempPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("checkpoint after commit: stat error = %v, want not exist", err)
	}
}

func newLateSHA256IngestRun(t *testing.T, tc lateSHA256IngestCase) *lateSHA256IngestRun {
	t.Helper()
	tsData := makeTSData(20)
	setIngestSHA256Rate(t, 47) // 3760 バイト / 47 B/s = 80 秒
	contentPath := "test/sha256-late.m2ts"
	run := &lateSHA256IngestRun{tsData: tsData, contentPath: contentPath}
	run.pool = setupTestPool(t)
	if run.pool == nil {
		return nil
	}
	run.recordingID = dbtest.Recording(t, run.pool)
	run.recordID = "rec-sha256-late-" + strings.ReplaceAll(tc.name, " ", "-")
	run.fake, run.setHash = newLateHashIngestFake(t, run.recordID, tsData, contentPath)
	insertTestRecordSync(t, run.pool, run.recordingID, run.recordID)
	run.mediaDir = t.TempDir()
	run.tempSyncs = &atomic.Int32{}
	originalOpenFile := openIngestFile
	t.Cleanup(func() { openIngestFile = originalOpenFile })
	openIngestFile = func(_ string, locked *os.File) (ingestFile, error) {
		file, err := duplicateIngestFile(locked)
		if err != nil {
			return nil, err
		}
		return &ingestTestFile{file: file, onSync: func() { run.tempSyncs.Add(1) }}, nil
	}
	run.worker = &IngestWorker{
		Publisher:     pgstoretest.Media(run.pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(run.fake.URL(), nil)),
		RecordTarget:  pgstoretest.Recordings(run.pool),
		MediaDir:      run.mediaDir,
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(run.pool),
	}
	run.worker.CMDetectionStore = pgstoretest.Analysis(run.pool)
	originalLogger := slog.Default()
	run.logOutput = &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(run.logOutput, nil)))
	t.Cleanup(func() { slog.SetDefault(originalLogger) })
	return run
}

func (run *lateSHA256IngestRun) job() IngestJobArgs {
	return IngestJobArgs{Site: "default", RecordID: run.recordID}
}

func (run *lateSHA256IngestRun) offsets() []int64 {
	var offsets []int64
	for _, call := range run.fake.Calls() {
		if call.Method != http.MethodGet || !strings.HasSuffix(call.Path, "/stream") {
			continue
		}
		offset := parseRangeOffset(call.Range)
		offsets = append(offsets, offset)
	}
	return offsets
}

func parseRangeOffset(header string) int64 {
	if header == "" {
		return 0
	}
	var offset int64
	if _, err := fmt.Sscanf(header, "bytes=%d-", &offset); err != nil {
		return -1
	}
	return offset
}

func (run *lateSHA256IngestRun) runPending(t *testing.T, first bool, beforeResults map[string]float64, durationBefore uint64) {
	t.Helper()
	err := runWorkerViaJobQFake(context.Background(), run.worker, run.job())
	if _, ok := jobq.SnoozeDelay(err); !ok {
		t.Fatalf("pending Work error = %v, want jobq snooze", err)
	}
	if !strings.Contains(run.logOutput.String(), "sha256_verification=pending") || strings.Contains(run.logOutput.String(), "sha256_verification=skipped") {
		t.Errorf("wait log = %q, want sha256_verification=pending only", run.logOutput.String())
	}
	assertIngestResultDeltas(t, beforeResults, nil, "SHA-256 pending snooze")
	if got := ingestDurationSamples(t); got != durationBefore {
		t.Errorf("IngestDuration samples while pending = %d, want %d", got, durationBefore)
	}
	wantSyncs := int32(2)
	if first {
		wantSyncs = 1
	}
	if got := run.tempSyncs.Load(); got != wantSyncs {
		t.Errorf("temp Sync calls while pending = %d, want %d", got, wantSyncs)
	}
	assertIngestSHA256WaitState(t, run.pool, run.recordingID, run.fake, run.mediaDir, run.recordID, int64(len(run.tsData)))
	tempPath := ingestTempFilePath(filepath.Join(run.mediaDir, "sites", "default", "test"), "default", run.recordID)
	info, err := os.Stat(tempPath)
	if err != nil {
		t.Fatalf("stating temp during SHA-256 wait: %v", err)
	}
	gotOffsets := run.offsets()
	if first {
		run.firstOffsets = gotOffsets
		run.tempModTime = info.ModTime()
	} else {
		if !slices.Equal(gotOffsets, run.firstOffsets) {
			t.Errorf("Range offsets after a repeated pending job = %v, want unchanged %v (no temp replay)", gotOffsets, run.firstOffsets)
		}
		if !info.ModTime().Equal(run.tempModTime) {
			t.Errorf("temp mtime after a repeated SHA-256 snooze = %s, want unchanged %s", info.ModTime(), run.tempModTime)
		}
	}
}

func (run *lateSHA256IngestRun) complete(t *testing.T, tc lateSHA256IngestCase) error {
	t.Helper()
	if tc.lateHash != nil {
		lateHash := tc.lateHash(run.tsData)
		run.setHash(&lateHash)
	} else {
		tempPath := ingestTempFilePath(filepath.Join(run.mediaDir, "sites", "default", "test"), "default", run.recordID)
		oldTime := time.Now().Add(-2 * time.Minute)
		if err := os.Chtimes(tempPath, oldTime, oldTime); err != nil {
			t.Fatalf("aging temp mtime past SHA-256 deadline: %v", err)
		}
	}
	return runWorkerViaJobQFake(context.Background(), run.worker, run.job())
}

func (run *lateSHA256IngestRun) assertCompletion(t *testing.T, tc lateSHA256IngestCase, beforeResults map[string]float64, durationBefore uint64, mismatchesBefore float64, workErr error) {
	t.Helper()
	if tc.wantError != "" {
		if workErr == nil || !strings.Contains(workErr.Error(), tc.wantError) {
			t.Fatalf("late-hash Work error = %v, want %q", workErr, tc.wantError)
		}
	} else if workErr != nil {
		t.Fatalf("late-hash Work: %v", workErr)
	}
	assertIngestResultDeltas(t, beforeResults, map[string]float64{tc.wantResult: 1}, "late SHA-256 decision")
	if got, want := ingestDurationSamples(t), durationBefore+1; got != want {
		t.Errorf("IngestDuration samples after late SHA-256 decision = %d, want %d", got, want)
	}
	wantMismatchDelta := 0.0
	if tc.wantError != "" {
		wantMismatchDelta = 1
	}
	if got := promtestutil.ToFloat64(metrics.IngestHashMismatches); got != mismatchesBefore+wantMismatchDelta {
		t.Errorf("hash mismatch counter after late SHA-256 decision = %v, want %v", got, mismatchesBefore+wantMismatchDelta)
	}
	var assetCount int
	if err := run.pool.QueryRow(context.Background(), "SELECT count(*) FROM media_assets WHERE recording_id = $1", run.recordingID).Scan(&assetCount); err != nil {
		t.Fatalf("counting media_assets after resume: %v", err)
	}
	if assetCount != tc.wantAssets {
		t.Errorf("media_assets rows after resume = %d, want %d", assetCount, tc.wantAssets)
	}
	if got := int32(run.fake.CallCount(http.MethodDelete, "/api/recording/records/"+run.recordID)); got != tc.wantDeleteAttempts {
		t.Errorf("DeleteRecord attempts after resume = %d, want %d", got, tc.wantDeleteAttempts)
	}
	if want := []int64{0, int64(len(run.tsData)), int64(len(run.tsData)), int64(len(run.tsData)), int64(len(run.tsData))}; !slices.Equal(run.offsets(), want) {
		t.Errorf("Range offsets across completed jobs and replay = %v, want %v", run.offsets(), want)
	}
	if tc.wantVerification != "" && !strings.Contains(run.logOutput.String(), tc.wantVerification) {
		t.Errorf("logs = %q, want %q", run.logOutput.String(), tc.wantVerification)
	}
	if tc.wantAssets == 1 {
		canonicalPath := filepath.Join(run.mediaDir, "sites", "default", run.contentPath)
		committed, err := os.ReadFile(canonicalPath)
		if err != nil {
			t.Fatalf("reading committed canonical file: %v", err)
		}
		if !bytes.Equal(committed, run.tsData) {
			t.Error("committed canonical file differs from the transfer bytes")
		}
	}
}

func assertIngestSHA256WaitState(t *testing.T, pool *pgxpool.Pool, recordingID int64, fake *mirakctest.Fake, mediaDir, recordID string, wantTempSize int64) {
	t.Helper()
	var assetCount int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID).Scan(&assetCount); err != nil {
		t.Fatalf("counting media_assets while waiting: %v", err)
	}
	if assetCount != 0 {
		t.Errorf("media_assets rows while waiting = %d, want 0", assetCount)
	}
	if got := fake.CallCount(http.MethodDelete, "/api/recording/records/"+recordID); got != 0 {
		t.Errorf("DeleteRecord attempts while waiting = %d, want 0", got)
	}
	var progressRows int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM recording_ingest_progress WHERE recording_id = $1", recordingID).Scan(&progressRows); err != nil {
		t.Fatalf("counting progress rows while waiting: %v", err)
	}
	if progressRows != 0 {
		t.Errorf("recording_ingest_progress rows while waiting = %d, want 0", progressRows)
	}
	tempPath := ingestTempFilePath(filepath.Join(mediaDir, "sites", "default", "test"), "default", recordID)
	if info, err := os.Stat(tempPath); err != nil || info.Size() != wantTempSize {
		t.Errorf("ingest temp while waiting: stat=(%v, %v), want size %d", info, err, wantTempSize)
	}
}

func TestIngestWorker_OptionalContentSHA256(t *testing.T) {
	setFastIngestFollow(t)
	for _, tt := range []struct {
		name      string
		shaValue  func([]byte) string
		wantError bool
	}{
		{name: "missing after timeout"},
		{name: "uppercase-and-space", shaValue: func(data []byte) string {
			return "  " + strings.ToUpper(sha256Hex(data)) + "  "
		}},
		{name: "empty", shaValue: func([]byte) string { return "" }},
		{name: "uppercase-and-space-mismatch", wantError: true, shaValue: func(data []byte) string {
			wrongData := bytes.Clone(data)
			wrongData[len(wrongData)-1] ^= 0xff
			return "  " + strings.ToUpper(sha256Hex(wrongData)) + "  "
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tsData := makeTSData(20)
			recordID := "rec-optional-hash-" + tt.name
			var contentSHA256 *string
			if tt.shaValue != nil {
				value := tt.shaValue(tsData)
				contentSHA256 = &value
			}
			fake := newSHA256IngestFake(t, recordID, tsData, "test/optional-"+tt.name+".m2ts", contentSHA256)

			pool := setupTestPool(t)
			if pool == nil {
				return
			}
			recordingID := dbtest.Recording(t, pool)
			insertTestRecordSync(t, pool, recordingID, recordID)

			w := &IngestWorker{
				Publisher:     pgstoretest.Media(pool),
				MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
				RecordTarget:  pgstoretest.Recordings(pool),
				MediaDir:      t.TempDir(),
				StallTimeout:  5 * time.Second,
				Precheck:      pgstoretest.Media(pool),
			}
			w.CMDetectionStore = pgstoretest.Analysis(pool)
			job := IngestJobArgs{Site: "default", RecordID: recordID}

			err := runWorkerViaJobQFake(context.Background(), w, job)
			if tt.wantError {
				if err == nil {
					t.Fatal("Work() error = nil, want hash mismatch")
				}
				if !strings.Contains(err.Error(), "hash mismatch") {
					t.Fatalf("Work() error = %v, want hash mismatch", err)
				}
				var assetCount int
				if err := pool.QueryRow(context.Background(),
					"SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID,
				).Scan(&assetCount); err != nil {
					t.Fatalf("counting media_assets after hash mismatch: %v", err)
				}
				if assetCount != 0 {
					t.Errorf("media_assets rows after hash mismatch = %d, want 0", assetCount)
				}
				if got := fake.CallCount(http.MethodDelete, "/api/recording/records/"+recordID); got != 0 {
					t.Errorf("DeleteRecord attempts after hash mismatch = %d, want 0", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Work() error: %v", err)
			}
			var assetCount int
			if err := pool.QueryRow(context.Background(),
				"SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID,
			).Scan(&assetCount); err != nil {
				t.Fatalf("counting media_assets: %v", err)
			}
			if assetCount != 1 {
				t.Errorf("media_assets rows = %d, want 1", assetCount)
			}
			if got := fake.CallCount(http.MethodDelete, "/api/recording/records/"+recordID); got != 1 {
				t.Errorf("DeleteRecord attempts = %d, want 1", got)
			}
		})
	}
}

func TestIngestWorker_UsesSHA256ThatArrivesDuringFinalSync(t *testing.T) {
	tsData := makeTSData(20)
	setIngestSHA256Rate(t, 47)
	const recordID = "rec-hash-during-sync"
	fake, setHash := newLateHashIngestFake(t, recordID, tsData, "test/hash-during-sync.m2ts")
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, recordID)
	mediaDir := t.TempDir()
	originalOpenFile := openIngestFile
	t.Cleanup(func() { openIngestFile = originalOpenFile })
	var published atomic.Bool
	openIngestFile = func(_ string, locked *os.File) (ingestFile, error) {
		file, err := duplicateIngestFile(locked)
		if err != nil {
			return nil, err
		}
		return &ingestTestFile{file: file, onSync: func() {
			if published.CompareAndSwap(false, true) {
				hash := sha256Hex(tsData)
				setHash(&hash)
			}
		}}, nil
	}
	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
		RecordTarget:  pgstoretest.Recordings(pool),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)
	originalLogger := slog.Default()
	var logOutput bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logOutput, nil)))
	t.Cleanup(func() { slog.SetDefault(originalLogger) })
	job := IngestJobArgs{Site: "default", RecordID: recordID}
	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if !strings.Contains(logOutput.String(), "sha256_verification=verified") {
		t.Errorf("logs = %q, want late hash verified in the same job", logOutput.String())
	}
	if strings.Contains(logOutput.String(), "sha256_verification=pending") {
		t.Errorf("logs = %q, unexpected pending state after hash arrived during sync", logOutput.String())
	}
	var assetCount int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID).Scan(&assetCount); err != nil {
		t.Fatalf("counting media_assets: %v", err)
	}
	if assetCount != 1 {
		t.Errorf("media_assets rows = %d, want 1", assetCount)
	}
	if got := fake.CallCount(http.MethodDelete, "/api/recording/records/"+recordID); got != 1 {
		t.Errorf("DeleteRecord attempts = %d, want 1", got)
	}
}

func TestIngestWorker_DoesNotWaitWhenHEADLengthIsUnknown(t *testing.T) {
	tsData := makeTSData(20)
	contentPath := "test/unknown-head-length.m2ts"
	length := uint64(len(tsData))
	endTime := mirakc.Milliseconds(time.Now())
	var deleteAttempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			mirakctest.WriteRecordStream(rw, r, tsData)
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			// Deliberately omit Content-Length; record.content.length is present but is not
			// an authority for deciding that the local temp is complete.
			rw.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Status: "finished", EndTime: &endTime, Options: mirakc.Options{ContentPath: &contentPath}},
				Content:   mirakc.ContentInfo{Path: "/recording/" + contentPath, Length: &length},
			}
			rw.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(rw).Encode(record)
		case r.Method == http.MethodDelete:
			deleteAttempts.Add(1)
			rw.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(rw).Encode(mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true})
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(srv.Close)

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-unknown-head-length")
	mediaDir := t.TempDir()
	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		RecordTarget:  pgstoretest.Recordings(pool),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)
	originalLogger := slog.Default()
	var logOutput bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logOutput, nil)))
	t.Cleanup(func() { slog.SetDefault(originalLogger) })
	job := IngestJobArgs{Site: "default", RecordID: "rec-unknown-head-length"}
	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work with unknown HEAD length: %v", err)
	}
	if !strings.Contains(logOutput.String(), "sha256_verification=timeout_skipped") {
		t.Errorf("logs = %q, want timeout_skipped without waiting", logOutput.String())
	}
	if strings.Contains(logOutput.String(), "sha256_verification=pending") {
		t.Errorf("logs = %q, unexpected SHA-256 wait with unknown HEAD length", logOutput.String())
	}
	var assetCount int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID).Scan(&assetCount); err != nil {
		t.Fatalf("counting media_assets: %v", err)
	}
	if assetCount != 1 {
		t.Errorf("media_assets rows = %d, want 1", assetCount)
	}
	if got := deleteAttempts.Load(); got != 1 {
		t.Errorf("DeleteRecord attempts = %d, want 1", got)
	}
}

type partialErrorWriter struct {
	n   int
	err error
}

func (w partialErrorWriter) Write(p []byte) (int, error) {
	return w.n, w.err
}

func TestHashingWriterHashesAcceptedBytes(t *testing.T) {
	input := []byte("accepted prefix plus rejected suffix")
	injectedErr := errors.New("injected partial write")
	hasher := sha256.New()
	w := &hashingWriter{
		w: partialErrorWriter{n: len("accepted prefix"), err: injectedErr},
		h: hasher,
	}

	n, err := w.Write(input)
	if !errors.Is(err, injectedErr) {
		t.Fatalf("Write() error = %v, want injected partial write", err)
	}
	if n != len("accepted prefix") {
		t.Fatalf("Write() bytes = %d, want %d", n, len("accepted prefix"))
	}
	if got, want := hex.EncodeToString(hasher.Sum(nil)), sha256Hex(input[:n]); got != want {
		t.Errorf("hash of accepted bytes = %s, want %s", got, want)
	}
}

// TestIngestWorker_PersistsBeforeCommitAndDelete は原本ファイルの Sync/Close 前は
// media_assets 行がなく、mirakc の DeleteRecord 時点では行があることをイベント列で検証する。
// 転送途中の定期 fsync を入れる変異も、イベント列が変わるため検出する。
func TestIngestWorker_PersistsBeforeCommitAndDelete(t *testing.T) {
	tsData := makeTSData(20)
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-durable-order")

	var eventsMu sync.Mutex
	var events []string
	// snapshot は「呼ばれた時点の media_assets 行数」を name=count の形で
	// events へ append するクロージャを返す。fsync / Close / DeleteRecord の
	// 各フックから同じ形で呼び、コミット前後の可視性を 1 実装で検査する。
	snapshot := func(name string) func() {
		return func() {
			var count int
			if err := pool.QueryRow(context.Background(),
				"SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID,
			).Scan(&count); err != nil {
				t.Errorf("counting media_assets (%s): %v", name, err)
				return
			}
			eventsMu.Lock()
			events = append(events, fmt.Sprintf("%s=%d", name, count))
			eventsMu.Unlock()
		}
	}

	const recordID = "rec-durable-order"
	fake := newFullTransferFake(t, recordID, tsData, "test/durable-order.m2ts")
	client := &http.Client{Transport: mirakcRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			snapshot("delete-assets")()
		}
		return http.DefaultTransport.RoundTrip(req)
	})}

	mediaDir := t.TempDir()
	originalOpenFile := openIngestFile
	t.Cleanup(func() { openIngestFile = originalOpenFile })

	openIngestFile = func(_ string, locked *os.File) (ingestFile, error) {
		file, err := duplicateIngestFile(locked)
		if err != nil {
			return nil, err
		}
		return &ingestTestFile{
			file:    file,
			onSync:  snapshot("file-sync-assets"),
			onClose: snapshot("file-close-assets"),
		}, nil
	}

	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), client)),
		RecordTarget:  pgstoretest.Recordings(pool),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)
	job := IngestJobArgs{Site: "default", RecordID: recordID}

	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	eventsMu.Lock()
	gotEvents := slices.Clone(events)
	eventsMu.Unlock()
	wantEvents := []string{
		"file-sync-assets=0",
		"file-close-assets=0",
		"delete-assets=1",
	}
	if !slices.Equal(gotEvents, wantEvents) {
		t.Errorf("operation events = %v, want %v", gotEvents, wantEvents)
	}
}

// TestIngestWorker_DurabilityFailuresKeepEdgeRecord は、原本ファイルの
// Sync/Close が失敗した場合に media_assets へ登録されず mirakc の
// DeleteRecord も呼ばれないこと、その後実装本来の opener に戻して再試行すると
// 正しい原本が公開されることを確認する。
func TestIngestWorker_DurabilityFailuresKeepEdgeRecord(t *testing.T) {
	setFastIngestFollow(t)
	injectedErr := errors.New("injected durability failure")
	tests := []struct {
		name      string
		fileSync  bool
		fileClose bool
	}{
		{name: "file Sync", fileSync: true},
		{name: "file Close", fileClose: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tsData := makeTSData(20)
			pool := setupTestPool(t)
			if pool == nil {
				return
			}
			recordingID := dbtest.Recording(t, pool)
			const recordID = "rec-durable-failure"
			insertTestRecordSync(t, pool, recordingID, recordID)
			fake := newFullTransferFake(t, recordID, tsData, "test/durable-failure.m2ts")

			mediaDir := t.TempDir()
			originalOpenFile := openIngestFile
			t.Cleanup(func() { openIngestFile = originalOpenFile })

			openIngestFile = func(_ string, locked *os.File) (ingestFile, error) {
				file, err := duplicateIngestFile(locked)
				if err != nil {
					return nil, err
				}
				var syncErr, closeErr error
				if tt.fileSync {
					syncErr = injectedErr
				}
				if tt.fileClose {
					closeErr = injectedErr
				}
				return &ingestTestFile{file: file, syncErr: syncErr, closeErr: closeErr}, nil
			}

			w := &IngestWorker{
				Publisher:     pgstoretest.Media(pool),
				MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
				RecordTarget:  pgstoretest.Recordings(pool),
				MediaDir:      mediaDir,
				StallTimeout:  5 * time.Second,
				Precheck:      pgstoretest.Media(pool),
			}
			w.CMDetectionStore = pgstoretest.Analysis(pool)
			job := IngestJobArgs{Site: "default", RecordID: recordID}

			if err := runWorkerViaJobQFake(context.Background(), w, job); err == nil {
				t.Fatal("first Work() error = nil, want injected durability failure")
			}
			var assetCount int
			if err := pool.QueryRow(context.Background(),
				"SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID,
			).Scan(&assetCount); err != nil {
				t.Fatalf("counting media_assets after failed Work(): %v", err)
			}
			if assetCount != 0 {
				t.Errorf("media_assets rows after failed Work() = %d, want 0", assetCount)
			}
			if got := fake.CallCount(http.MethodDelete, "/api/recording/records/"+recordID); got != 0 {
				t.Errorf("DeleteRecord attempts after failed Work() = %d, want 0", got)
			}

			// 再試行では実装本来の opener に戻す。失敗した試行の temp を replay して、
			// 正しい原本を続きから公開できることを確認する。
			openIngestFile = originalOpenFile
			if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
				t.Fatalf("retry Work() error: %v", err)
			}

			fullPath := filepath.Join(mediaDir, "sites", "default", "test", "durable-failure.m2ts")
			gotData, err := os.ReadFile(fullPath)
			if err != nil {
				t.Fatalf("reading retried output file: %v", err)
			}
			if !bytes.Equal(gotData, tsData) {
				t.Errorf("retried file content differs from the edge original")
			}
			if err := pool.QueryRow(context.Background(),
				"SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID,
			).Scan(&assetCount); err != nil {
				t.Fatalf("counting media_assets after retry: %v", err)
			}
			if assetCount != 1 {
				t.Errorf("media_assets rows after retry = %d, want 1", assetCount)
			}
			if got := fake.CallCount(http.MethodDelete, "/api/recording/records/"+recordID); got != 1 {
				t.Errorf("DeleteRecord attempts after retry = %d, want 1", got)
			}
		})
	}
}

func TestIngestWorker_StallDetection(t *testing.T) {
	tsData := makeTSData(10)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			rangeHeader := r.Header.Get("Range")
			var offset int
			if rangeHeader != "" {
				_, _ = fmt.Sscanf(rangeHeader, "bytes=%d-", &offset)
			}

			if offset == 0 {
				flusher, ok := w.(http.Flusher)
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(tsData[:188])
				if ok {
					flusher.Flush()
				}
				// stall: don't send more data, wait for context cancellation
				<-r.Context().Done()
				return
			}

			remaining := tsData[offset:]
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(remaining)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(remaining)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Status:  "finished",
					Options: mirakc.Options{ContentPath: strPtr("test/stall.m2ts")},
				},
				Content: mirakc.ContentInfo{Path: "/recording/test/stall.m2ts", Sha256: strPtr(sha256Hex(tsData))},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		case r.Method == http.MethodDelete:
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(srv.URL, nil)

	w := &IngestWorker{
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      mediaDir,
		StallTimeout:  200 * time.Millisecond,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Precheck = pgstoretest.Media(pool)
	w.Publisher = pgstoretest.Media(pool)
	w.RecordTarget = pgstoretest.Recordings(pool)
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-stall")

	job := IngestJobArgs{Site: "default", RecordID: "rec-stall"}

	err := runWorkerViaJobQFake(context.Background(), w, job)
	if err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	// rel_path には args.Site（ここでは "default"）が前置される（issue #186 M4-14）。
	fullPath := filepath.Join(mediaDir, "sites", "default", "test", "stall.m2ts")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if len(data) != len(tsData) {
		t.Errorf("file size = %d, want %d", len(data), len(tsData))
	}
}

func strPtr(s string) *string { return &s }

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// setupTestPool はマイグレーション済みのテスト用プールを返す。
func setupTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testutil.SetupDB(t)
}

// noProgramRecordSyncProgramID は insertTestRecordSync が使う固定の program_id。
// 呼び出し側は dbtest.Recording（program_snapshots を作らない）で録画だけを
// 用意していることを前提にしており、この値は意図的にどの program_snapshots
// 行とも対応しない。テストが扱っている番組（insertProgramSnapshotAndReservation /
// insertTestRecordSyncForSite に渡した programID）と record_sync を
// 対応させたい場合は insertTestRecordSyncForSite を使うこと（対応させないまま
// insertTestRecordSync を使うと、record_sync.program_id を読む将来の実装に対する
// テストが黙って空虚な PASS になる）。
const noProgramRecordSyncProgramID = 327361024000001

// recordingBroadcastEventKey は recordings 行の site と放送イベントキー
// (network_id, service_id, event_id) を返す。
// assertRecordSyncProgramMatchesRecording が record_sync.program_id と
// recordings の対応を検査するのに使う。site も返すのは、検査側が
// program_snapshots を引く site を固定値で書かないため（録画が "tokyo" など
// default 以外の site に作られていると、'default' 固定の検査はその番組を見落とす）。
func recordingBroadcastEventKey(t *testing.T, pool *pgxpool.Pool, recordingID int64) (site string, networkID, serviceID, eventID int32) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		"SELECT site, network_id, service_id, event_id FROM recordings WHERE id = $1", recordingID,
	).Scan(&site, &networkID, &serviceID, &eventID); err != nil {
		t.Fatalf("querying recording broadcast event key (recording_id=%d): %v", recordingID, err)
	}
	return site, networkID, serviceID, eventID
}

// assertRecordSyncProgramMatchesRecording は「record_sync.program_id は
// recordingID の録画が対象にしている番組を指している」という record_sync
// フィクスチャの不変条件を**両方向**で検査する。helperName はエラーメッセージに
// 出す呼び出し元ヘルパ名。
//
// 検査する 2 方向:
//   - (site, programID) が program_snapshots に解決する場合、その放送イベント
//     キーが recordings 行と一致すること（別の番組を指していないこと）
//   - 解決しない場合、録画側の放送イベントにも program_snapshots が無いこと
//     （テストが番組を scope に持っているのに record_sync がどの番組も
//     指していない = issue #285 が報告した状態そのもの）
//
// どちらの方向も欠かすと、record_sync.program_id を読む実装のテストが黙って
// 空虚な PASS になる。落ちることの確認は、いずれかの呼び出しを
// insertTestRecordSync に差し戻す / 古い programID を渡す変異で行う。
func assertRecordSyncProgramMatchesRecording(t *testing.T, pool *pgxpool.Pool, helperName, site string, recordingID, programID int64) {
	t.Helper()
	recSite, recNetworkID, recServiceID, recEventID := recordingBroadcastEventKey(t, pool, recordingID)

	var snapNetworkID, snapServiceID, snapEventID int32
	err := pool.QueryRow(context.Background(),
		"SELECT network_id, service_id, event_id FROM program_snapshots WHERE site = $1 AND program_id = $2",
		site, programID,
	).Scan(&snapNetworkID, &snapServiceID, &snapEventID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		var hasSnapshot bool
		if err := pool.QueryRow(context.Background(),
			`SELECT EXISTS(
				SELECT 1 FROM program_snapshots
				WHERE site = $1 AND network_id = $2 AND service_id = $3 AND event_id = $4
			)`, recSite, recNetworkID, recServiceID, recEventID,
		).Scan(&hasSnapshot); err != nil {
			t.Fatalf("checking program_snapshots for recording %d's broadcast event: %v", recordingID, err)
		}
		if hasSnapshot {
			t.Fatalf("%s: programID %d (site=%s) does not resolve to any program_snapshots row, but recording %d "+
				"has a program_snapshots row for its broadcast event (site=%s, network_id=%d, service_id=%d, event_id=%d); "+
				"this test has a program in scope, pass that program's programID to insertTestRecordSyncForSite",
				helperName, programID, site, recordingID, recSite, recNetworkID, recServiceID, recEventID)
		}
	case err != nil:
		t.Fatalf("querying program_snapshots (site=%s, program_id=%d): %v", site, programID, err)
	default:
		if recNetworkID != snapNetworkID || recServiceID != snapServiceID || recEventID != snapEventID {
			t.Fatalf("%s: programID %d (site=%s) resolves to program_snapshots broadcast event (%d,%d,%d), "+
				"but recording %d has (%d,%d,%d) -- record_sync would point at the wrong program",
				helperName, programID, site, snapNetworkID, snapServiceID, snapEventID,
				recordingID, recNetworkID, recServiceID, recEventID)
		}
	}
}

// insertTestRecordSync は番組と対応しない record_sync 行を 1 件作る
// （noProgramRecordSyncProgramID 参照）。呼び出し側の録画がたまたま
// program_snapshots に番組を持っている（= insertProgramSnapshotAndReservation
// で番組がテストの scope にある）場合は、対応させないという前提が崩れている
// ので t.Fatalf する。対応させたい場合は insertTestRecordSyncForSite を使うこと。
func insertTestRecordSync(t *testing.T, pool *pgxpool.Pool, recordingID int64, recordID string) {
	t.Helper()
	q := sqlcgen.New(pool)
	if err := q.UpsertRecordSync(context.Background(), sqlcgen.UpsertRecordSyncParams{
		Site:        "default",
		RecordID:    recordID,
		RecordingID: &recordingID,
		ProgramID:   noProgramRecordSyncProgramID,
		Status:      "finished",
		Tags:        []string{},
	}); err != nil {
		t.Fatalf("inserting test record_sync: %v", err)
	}

	assertRecordSyncProgramMatchesRecording(t, pool, "insertTestRecordSync", "default", recordingID, noProgramRecordSyncProgramID)
}

// insertTestRecordSyncForSite は insertTestRecordSync の site / programID 指定版。
// dbtest.Recording で作った録画との programID と recordingID の対応は
// assertRecordSyncProgramMatchesRecording が両方向で検査する ——
// programID が実在する program_snapshots 行を指しているならその放送イベントキーが
// recordings 行と一致すること、指していないなら録画側の放送イベントにも
// program_snapshots が無いこと（番組を持たないテストが独自の数値を渡す場合は
// 後者で通る）。
func insertTestRecordSyncForSite(t *testing.T, pool *pgxpool.Pool, site string, recordingID int64, recordID string, programID int64) {
	t.Helper()
	q := sqlcgen.New(pool)
	if err := q.UpsertRecordSync(context.Background(), sqlcgen.UpsertRecordSyncParams{
		Site:        site,
		RecordID:    recordID,
		RecordingID: &recordingID,
		ProgramID:   programID,
		Status:      "finished",
		Tags:        []string{},
	}); err != nil {
		t.Fatalf("inserting test record_sync (site=%s): %v", site, err)
	}

	assertRecordSyncProgramMatchesRecording(t, pool, "insertTestRecordSyncForSite", site, recordingID, programID)
}

func TestIngestWorker_JobReexecution(t *testing.T) {
	tsData := makeTSData(50)
	const recordID = "rec-reexec"
	fake := newFullTransferFake(t, recordID, tsData, "test/reexec.m2ts")

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(fake.URL(), nil)

	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, recordID)

	// rel_path には args.Site（ここでは "default"）が前置される（issue #186 M4-14）。
	fullPath := filepath.Join(mediaDir, "sites", "default", "test", "reexec.m2ts")
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	garbage := []byte("this is leftover data from a crashed previous attempt that is longer than the real file")
	if err := os.WriteFile(fullPath, garbage, 0o644); err != nil {
		t.Fatal(err)
	}

	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mc),
		RecordTarget:  pgstoretest.Recordings(pool),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	job := IngestJobArgs{Site: "default", RecordID: recordID}

	err := runWorkerViaJobQFake(context.Background(), w, job)
	if err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if len(data) != len(tsData) {
		t.Errorf("file size = %d, want %d (stale canonical data must not survive commit)", len(data), len(tsData))
	}
	if string(data[:4]) != string(tsData[:4]) {
		t.Error("file content does not match expected TS data (atomic rename may have failed)")
	}
}

// TestIngestWorker_SkipsTransferWhenAlreadyCommitted は、エッジ record の削除
// （DeleteRecord）が失敗して mirakc 側に record が残り、同じジョブ引数で
// Work が再実行された（record_sweep 経由の再投入を模している）場合の挙動を
// 確認する。修正前は 2 回目の Work が canonical file を
// 0 バイトに切り詰めて全量を再転送し、最後に media_assets の unique 制約
// 違反でエラーになっていた。
func TestIngestWorker_SkipsTransferWhenAlreadyCommitted(t *testing.T) {
	tsData := makeTSData(50)
	const recordID = "rec-reingest"
	fake := newFullTransferFake(t, recordID, tsData, "test/reingest.m2ts")
	// 1 回目の DELETE は失敗させ、mirakc 側に record が残る状況を再現する。
	fake.FailNext(http.MethodDelete, "/api/recording/records/"+recordID, 1, http.StatusInternalServerError)

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Precheck = pgstoretest.Media(pool)
	w.Publisher = pgstoretest.Media(pool)
	w.RecordTarget = pgstoretest.Recordings(pool)
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, recordID)

	job := IngestJobArgs{Site: "default", RecordID: recordID}

	// 1 回目: 転送・コミットは成功するが、DeleteRecord は失敗する。
	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("first Work() error: %v", err)
	}

	// rel_path には args.Site（ここでは "default"）が前置される（issue #186 M4-14）。
	fullPath := filepath.Join(mediaDir, "sites", "default", "test", "reingest.m2ts")
	firstData, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file after first Work(): %v", err)
	}
	if len(firstData) != len(tsData) {
		t.Fatalf("file size after first Work() = %d, want %d", len(firstData), len(tsData))
	}

	// 別経路で残った stale progress を再投入前に作り、既にコミット済みの場合も
	// 再実行時に掃除されることを検証する。最初の commit 自身が progress を消すため、
	// ここで作らないと handleAlreadyCommittedIngest の ClearIngestProgress を通らない。
	media := pgstoretest.Media(pool)
	if err := media.UpsertIngestProgress(context.Background(), store.IngestProgress{
		RecordingID:  recordingID,
		WrittenBytes: 10,
	}); err != nil {
		t.Fatalf("seeding stale ingest progress: %v", err)
	}
	if _, ok := readIngestProgress(t, pool, recordingID); !ok {
		t.Fatal("stale ingest progress row was not seeded")
	}

	// record_sweep が「まだ mirakc に record がある」ことを検知して同じジョブ引数を
	// 再投入したのと同じ状況を、同じ job で Work をもう一度呼ぶことで再現する。
	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("second Work() error: %v", err) // (a)
	}
	if _, ok := readIngestProgress(t, pool, recordingID); ok {
		t.Error("stale ingest progress row remains after already-committed retry")
	}

	secondData, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file after second Work(): %v", err)
	}
	if len(secondData) != len(tsData) {
		// (c) ファイルが切り詰められていない
		t.Errorf("file size after second Work() = %d, want %d (file must not be truncated)",
			len(secondData), len(tsData))
	}
	if !bytes.Equal(secondData, firstData) {
		t.Error("file content changed after second Work() (transfer should have been skipped)")
	}

	// 1 回目の Work は最初の Range、追い付き確認、finished 観測後の最終 Range で 3。2 回目は
	// 原本コミット済みなので転送に入らない（この数が 3 のままであることが、
	// 「転送をやり直していない」の判定になる）。
	if got := fake.CallCount(http.MethodGet, "/api/recording/records/"+recordID+"/stream"); got != 3 {
		t.Errorf("stream requests = %d, want 3 (first Work reads initial, catch-up, and terminal ranges; second Work must skip the transfer)", got)
	}

	var mediaAssetCount int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID,
	).Scan(&mediaAssetCount); err != nil {
		t.Fatalf("counting media_assets: %v", err)
	}
	if mediaAssetCount != 1 {
		// (b) media_assets が 1 行のまま
		t.Errorf("media_assets rows = %d, want 1", mediaAssetCount)
	}

	if got := fake.CallCount(http.MethodDelete, "/api/recording/records/"+recordID); got != 2 {
		// (d) 2 回目でも DeleteRecord が再試行されている
		t.Errorf("delete attempts = %d, want 2 (edge record deletion must be retried)", got)
	}
}

// --- M3-14 (issue #103): encode policy (keep_original / encode_profiles) の
// スナップショット。resolveAndSnapshotEncodePolicy（internal/worker/ingest.go）が
// ingest コミット tx 内で recordings に焼くことをエンドツーエンドで確認する。

// newMirakcRecordFake は 1 件の record と固定 stream を mirakc.Client 経由で返す。
func newMirakcRecordFake(t *testing.T, record mirakc.Record, stream []byte) *mirakctest.Fake {
	t.Helper()
	fake := mirakctest.New(t)
	fake.SetRecords([]mirakc.Record{record})
	if stream != nil {
		fake.SetRecordStream(record.ID, stream)
	}
	return fake
}

func newRecordPathFake(t *testing.T, recordID string, tsData []byte, contentPath *string, contentFilePath string) *mirakctest.Fake {
	var contentSHA256 *string
	if tsData != nil {
		contentSHA256 = strPtr(sha256Hex(tsData))
	}
	return newMirakcRecordFake(t, mirakc.Record{
		ID: recordID,
		Recording: mirakc.RecordInfo{
			Status: "finished", Options: mirakc.Options{ContentPath: contentPath},
		},
		Content: mirakc.ContentInfo{Path: contentFilePath, Sha256: contentSHA256},
	}, tsData)
}

func newFullTransferFake(t *testing.T, recordID string, tsData []byte, contentPath string) *mirakctest.Fake {
	t.Helper()
	endTime := mirakc.Milliseconds(time.Now().Add(-time.Hour))
	contentSHA256 := sha256Hex(tsData)
	contentPathPtr := strPtr(contentPath)
	return newMirakcRecordFake(t, mirakc.Record{
		ID: recordID,
		Recording: mirakc.RecordInfo{
			Status: "finished", EndTime: &endTime,
			Options: mirakc.Options{ContentPath: contentPathPtr},
		},
		Content: mirakc.ContentInfo{Path: "/recording/" + contentPath, Sha256: &contentSHA256},
	}, tsData)
}

func newSHA256IngestFake(t *testing.T, recordID string, tsData []byte, contentPath string, contentSHA256 *string) *mirakctest.Fake {
	t.Helper()
	endTime := mirakc.Milliseconds(time.Now().Add(-time.Hour))
	contentPathPtr := strPtr(contentPath)
	record := mirakc.Record{
		ID: recordID,
		Recording: mirakc.RecordInfo{
			Status: "finished", EndTime: &endTime,
			Options: mirakc.Options{ContentPath: contentPathPtr},
		},
		Content: mirakc.ContentInfo{Path: "/recording/" + contentPath, Sha256: contentSHA256},
	}
	return newMirakcRecordFake(t, record, tsData)
}

func newLateHashIngestFake(t *testing.T, recordID string, tsData []byte, contentPath string) (*mirakctest.Fake, func(*string)) {
	t.Helper()
	record := mirakc.Record{
		ID: recordID,
		Recording: mirakc.RecordInfo{
			Status: "finished", Options: mirakc.Options{ContentPath: strPtr(contentPath)},
		},
		Content: mirakc.ContentInfo{Path: "/recording/" + contentPath},
	}
	fake := newMirakcRecordFake(t, record, tsData)
	var mu sync.Mutex
	setHash := func(hash *string) {
		mu.Lock()
		defer mu.Unlock()
		var copyHash *string
		if hash != nil {
			value := *hash
			copyHash = &value
		}
		record.Content.Sha256 = copyHash
		fake.SetRecords([]mirakc.Record{record})
	}
	return fake, setHash
}

// insertProgramSnapshotAndReservation は programID の program_snapshots 行と、
// base の無い手動予約を作る。base だけは setReservationBase で後から模す
// （reservations の実運用上の書き手は ruler の 1 パス UPSERT だけで、
// internal/ruler 内の unexported 関数はテストから直接呼べないため）。
func insertProgramSnapshotAndReservation(t *testing.T, pool *pgxpool.Pool, programID int64, title string) sqlcgen.Reservation {
	t.Helper()
	ctx := context.Background()
	q := sqlcgen.New(pool)
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site:        "default",
		ProgramID:   programID,
		Title:       title,
		StartAt:     time.Now(),
		DurationMs:  1800000,
		NetworkID:   32736,
		ServiceID:   1024,
		ChannelType: "GR",
		Channel:     "27",
		EventID:     int32(programID % 100000),
		ServiceName: "テスト局",
	}); err != nil {
		t.Fatalf("upserting program snapshot: %v", err)
	}
	res, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:      "default",
		ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("creating reservation: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM program_snapshots WHERE site = $1 AND program_id = $2", "default", programID)
	})
	return res
}

// setReservationBase は ruler が書く reservations.base を模したテスト用フィクスチャ。
// internal/worker/encode_test.go が recording_encode_policy.encode_profiles を raw SQL で
// 直接作るのと同じ規律（reservations.sql は #52 並走中につき、この目的のためだけの
// 書き込みクエリを新設しない）。
func setReservationBase(t *testing.T, pool *pgxpool.Pool, programID int64, base string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"UPDATE reservations SET base = $2 WHERE site = 'default' AND program_id = $1", programID, base); err != nil {
		t.Fatalf("setting reservation base: %v", err)
	}
}

func TestAlreadyCommittedIngestDoesNotRebuildStaleCut(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "already-committed/stale-cut.m2ts",
		[]string{"cut"}, []byte("payload"))
	if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, recordingID); err != nil {
		t.Fatalf("adopting chapters: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(600000, 900000), 'CM', true)`, recordingID); err != nil {
		t.Fatalf("inserting initial chapter span: %v", err)
	}
	assetID := seedEncodedAsset(t, pool, recordingID, "cut", "already-committed/stale-cut.g1.mp4")
	frozen, _, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, recordingID)
	if err != nil {
		t.Fatalf("deriving initial keep ranges: %v", err)
	}
	if err := setFrozenCuts(t, pool, assetID, frozen); err != nil {
		t.Fatalf("seeding frozen ranges: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(1200000, 1300000), 'CM2', true)`, recordingID); err != nil {
		t.Fatalf("editing chapter spans: %v", err)
	}
	current, _, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, recordingID)
	if err != nil {
		t.Fatalf("deriving edited keep ranges: %v", err)
	}
	if slices.Equal(frozen, current) {
		t.Fatal("test setup did not make the active cut asset stale")
	}

	const recordID = "already-committed"
	fake := mirakctest.New(t)
	fake.SetRecords([]mirakc.Record{{ID: recordID}})

	enqueuer := newRecordingEnqueuer()
	w := &IngestWorker{Publisher: pgstoretest.Media(pool), Enqueuer: enqueuer, Precheck: pgstoretest.Media(pool)}
	w.handleAlreadyCommittedIngest(context.Background(), mirakc.NewClient(fake.URL(), nil),
		jobs.IngestJobArgs{Site: "default", RecordID: recordID}, recordingID, slog.Default())
	if got := countEncodeJobs(t, enqueuer, recordingID, "cut"); got != 0 {
		t.Fatalf("cut encode jobs after already-committed ingest = %d, want 0", got)
	}
	targetedPasses := countEnqueuedForRecording(t, enqueuer, "encode_reconcile", recordingID)
	if targetedPasses != 1 {
		t.Fatalf("targeted encode_reconcile jobs = %d, want 1", targetedPasses)
	}
	passEnqueuer := runEncodeReconcilePassArgs(t, &EncodeReconcileWorker{Profiles: config.EncodeConfig{
		Profiles: []config.EncodeProfile{cutFFmpegProfile()},
	}, Media: pgstoretest.Media(pool),
	}, EncodeReconcileArgs{RecordingID: recordingID})
	if got := countEncodeJobs(t, passEnqueuer, recordingID, "cut"); got != 0 {
		t.Errorf("cut encode jobs after targeted pass = %d, want 0", got)
	}
}

// encodePolicyOfRecording は recording_encode_policy 衛星表（issue #159）を読む。
// 行が無い（未凍結）場合は既定値 "always" / [] を返す --- resolveAndSnapshotEncodePolicy
// が解決に失敗した場合や、予約が無く一度も呼ばれない場合はこの表に行自体が
// 作られない（不変条件 10「意味を持たない行を作らない」）。既存テストの
// アサーションはいずれも「凍結されなかった＝既定値のまま」を確認する意図なので、
// この関数がその意図をそのまま表す。
func encodePolicyOfRecording(t *testing.T, pool *pgxpool.Pool, recordingID int64) (keepOriginal string, profiles []string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		"SELECT keep_original, encode_profiles FROM recording_encode_policy WHERE recording_id = $1", recordingID,
	).Scan(&keepOriginal, &profiles)
	if err == nil {
		return keepOriginal, profiles
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "always", []string{}
	}
	t.Fatalf("querying recording encode policy: %v", err)
	return "", nil
}

// encodePolicyRowExists は recording_encode_policy に行そのものがあるかを見る。
// encodePolicyOfRecording は ErrNoRows を既定値に潰すため「行がある + 既定値」と
// 「行が無い」を区別できない --- resolveAndSnapshotEncodePolicy が解決に失敗
// しても凍結する（issue #159 の中心的な設計判断。doc コメント「解決に失敗しても
// 凍結する」参照）ことを確認するテストは、この関数で行の有無そのものを見る
// 必要がある。
func encodePolicyRowExists(t *testing.T, pool *pgxpool.Pool, recordingID int64) bool {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM recording_encode_policy WHERE recording_id = $1", recordingID,
	).Scan(&count); err != nil {
		t.Fatalf("counting recording encode policy rows: %v", err)
	}
	return count == 1
}

func countEncodeJobs(t *testing.T, enqueuer *recordingEnqueuer, recordingID int64, profile string) int {
	t.Helper()
	count := 0
	for _, args := range enqueuedArgs[jobs.EncodeJobArgs](enqueuer, jobs.EncodeJobArgs{}.Kind()) {
		if args.RecordingID == recordingID && args.Profile == profile {
			count++
		}
	}
	return count
}

func countEncodeJobsForRecording(t *testing.T, enqueuer *recordingEnqueuer, recordingID int64) int {
	return countEnqueuedForRecording(t, enqueuer, jobs.EncodeJobArgs{}.Kind(), recordingID)
}

// TestIngestWorker_SnapshotsEncodePolicyFromRuleBase は issue #103 の受け入れ基準
// 「ルールに encodeProfiles を設定して録画 → ingest 完了後に
// recording_encode_policy.encode_profiles が一致し、対象 reconcile が投入されて
// reconcile 実行後に encode ジョブが投入される」を確認する。
func TestIngestWorker_SnapshotsEncodePolicyFromRuleBase(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	programID := int64(900000000000001)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "ルール予約番組")
	setReservationBase(t, pool, res.ProgramID, `{"keepOriginal":"until_encoded","encodeProfiles":["h265"]}`)

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Source, fixtureParams.NetworkID, fixtureParams.ServiceID, fixtureParams.EventID = "rule", 32736, 1024, int32(programID%100000)
	})
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-base", programID)

	tsData := makeTSData(20)
	fake := newFullTransferFake(t, "rec-policy-base", tsData, "test/policy-base.m2ts")
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      t.TempDir(),
		RecordTarget:  pgstoretest.Recordings(pool),
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	job := IngestJobArgs{Site: "default", RecordID: "rec-policy-base"}

	w.Enqueuer = newRecordingEnqueuer()
	ctx := context.Background()
	if err := runWorkerViaJobQFake(ctx, w, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "until_encoded" {
		t.Errorf("keep_original = %q, want until_encoded", keepOriginal)
	}
	if !slices.Equal(profiles, []string{"h265"}) {
		t.Errorf("encode_profiles = %v, want [h265]", profiles)
	}
	if got := countEnqueuedForRecording(t, w.Enqueuer.(*recordingEnqueuer), "encode_reconcile", recordingID); got != 1 {
		t.Fatalf("targeted encode_reconcile jobs = %d, want 1", got)
	}
	passEnqueuer := runEncodeReconcilePassArgs(t, &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool)},
		jobs.EncodeReconcileArgs{RecordingID: recordingID})

	if got := countEncodeJobs(t, passEnqueuer, recordingID, "h265"); got != 1 {
		t.Errorf("encode jobs for h265 = %d, want 1", got)
	}
}

// TestIngestWorker_SnapshotsEncodePolicyFromOverride は「PATCH .../overrides で
// encodeProfiles を上書きした予約が、ingest 後の recordings にその値で焼かれる」を
// 確認する。base（ルール由来: h264 / always）と overrides（ユーザー上書き: h265 /
// until_encoded）が競合するとき、reservation.EffectiveOptions を通して overrides が勝つ
// ことも合わせて確認する。
func TestIngestWorker_SnapshotsEncodePolicyFromOverride(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	programID := int64(900000000000002)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "上書き予約番組")
	setReservationBase(t, pool, res.ProgramID, `{"keepOriginal":"always","encodeProfiles":["h264"]}`)

	overrides, err := json.Marshal(map[string]any{
		"keepOriginal":   "until_encoded",
		"encodeProfiles": []string{"h265"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site:      "default",
		ProgramID: programID,
		Overrides: overrides,
	}); err != nil {
		t.Fatalf("setting override: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM program_overrides WHERE site = $1 AND program_id = $2", "default", programID)
	})

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Source, fixtureParams.NetworkID, fixtureParams.ServiceID, fixtureParams.EventID = "rule", 32736, 1024, int32(programID%100000)
	})
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-override", programID)

	tsData := makeTSData(20)
	fake := newFullTransferFake(t, "rec-policy-override", tsData, "test/policy-override.m2ts")
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      t.TempDir(),
		RecordTarget:  pgstoretest.Recordings(pool),
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	job := IngestJobArgs{Site: "default", RecordID: "rec-policy-override"}

	w.Enqueuer = newRecordingEnqueuer()
	workCtx := context.Background()
	if err := runWorkerViaJobQFake(workCtx, w, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "until_encoded" {
		t.Errorf("keep_original = %q, want until_encoded (override should win over base's always)", keepOriginal)
	}
	if !slices.Equal(profiles, []string{"h265"}) {
		t.Errorf("encode_profiles = %v, want [h265] (override should win over base's [h264])", profiles)
	}
	if got := countEnqueuedForRecording(t, w.Enqueuer.(*recordingEnqueuer), "encode_reconcile", recordingID); got != 1 {
		t.Fatalf("targeted encode_reconcile jobs = %d, want 1", got)
	}
	passEnqueuer := runEncodeReconcilePassArgs(t, &EncodeReconcileWorker{Profiles: encodeConfigWith("h264", "h265"), Media: pgstoretest.Media(pool)},
		jobs.EncodeReconcileArgs{RecordingID: recordingID})

	if got := countEncodeJobs(t, passEnqueuer, recordingID, "h265"); got != 1 {
		t.Errorf("encode jobs for h265 = %d, want 1", got)
	}
	if got := countEncodeJobs(t, passEnqueuer, recordingID, "h264"); got != 0 {
		t.Errorf("encode jobs for h264 = %d, want 0 (base profile must be fully replaced by override)", got)
	}
}

// TestIngestWorker_ClampsUntilEncodedWithEmptyProfiles は issue #104 との相互作用の
// 回帰テスト: override が keepOriginal=until_encoded だけを立て、encodeProfiles は
// （ルール側の h264 を明示的に打ち消して）空にした場合、実効値としては
// until_encoded × 空プロファイルというドリフトが生成される。この組み合わせは
// recordings.keep_original / encode_profiles への CHECK 制約
// （cardinality(encode_profiles) > 0、issue #104）に違反するため、そのまま書くと
// 原本 media_asset の INSERT と同一 tx がロールバックし録画が消失する
// （不変条件 3「コミット = DB 行」）。resolveAndSnapshotEncodePolicy のクランプで
// keepOriginal を 'always' に倒して書くことを確認する。
func TestIngestWorker_ClampsUntilEncodedWithEmptyProfiles(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	programID := int64(900000000000003)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "ドリフト予約番組")
	setReservationBase(t, pool, res.ProgramID, `{"keepOriginal":"always","encodeProfiles":["h264"]}`)

	overrides, err := json.Marshal(map[string]any{
		"keepOriginal":   "until_encoded",
		"encodeProfiles": []string{}, // ルールの h264 を明示的に打ち消す override
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site:      "default",
		ProgramID: programID,
		Overrides: overrides,
	}); err != nil {
		t.Fatalf("setting override: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM program_overrides WHERE site = $1 AND program_id = $2", "default", programID)
	})

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Source, fixtureParams.NetworkID, fixtureParams.ServiceID, fixtureParams.EventID = "rule", 32736, 1024, int32(programID%100000)
	})
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-drift", programID)

	tsData := makeTSData(20)
	fake := newFullTransferFake(t, "rec-policy-drift", tsData, "test/policy-drift.m2ts")
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      t.TempDir(),
		RecordTarget:  pgstoretest.Recordings(pool),
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	job := IngestJobArgs{Site: "default", RecordID: "rec-policy-drift"}

	w.Enqueuer = newRecordingEnqueuer()
	workCtx := context.Background()
	if err := runWorkerViaJobQFake(workCtx, w, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "always" {
		t.Errorf("keep_original = %q, want always (clamped: effective encodeProfiles is empty)", keepOriginal)
	}
	if len(profiles) != 0 {
		t.Errorf("encode_profiles = %v, want empty", profiles)
	}

	jobCount := countEncodeJobsForRecording(t, w.Enqueuer.(*recordingEnqueuer), recordingID)
	if jobCount != 0 {
		t.Errorf("encode jobs = %d, want 0", jobCount)
	}
}

// TestIngestWorker_NoReservation_LeavesEncodePolicyDefault は「予約行が無い録画では
// 既定値のまま・encode ジョブも入らない」を確認する（手動で mirakc に起こされた
// 録画等、放送イベントキーで program_snapshots → reservations を引いても
// 何も見つからないケース。dbtest.Recording が作る録画にはそもそも対応する
// program_snapshots / reservations 行が無い）。
func TestIngestWorker_NoReservation_LeavesEncodePolicyDefault(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-policy-none")

	tsData := makeTSData(20)
	fake := newFullTransferFake(t, "rec-policy-none", tsData, "test/policy-none.m2ts")
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      t.TempDir(),
		RecordTarget:  pgstoretest.Recordings(pool),
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	job := IngestJobArgs{Site: "default", RecordID: "rec-policy-none"}

	w.Enqueuer = newRecordingEnqueuer()
	ctx := context.Background()
	if err := runWorkerViaJobQFake(ctx, w, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "always" {
		t.Errorf("keep_original = %q, want always (default, unchanged)", keepOriginal)
	}
	if len(profiles) != 0 {
		t.Errorf("encode_profiles = %v, want empty (default, unchanged)", profiles)
	}

	jobCount := countEncodeJobsForRecording(t, w.Enqueuer.(*recordingEnqueuer), recordingID)
	if jobCount != 0 {
		t.Errorf("encode jobs = %d, want 0", jobCount)
	}
}

// TestIngestWorker_SnapshotGCedBeyondGrace_FreezesDefaults は issue #214 の決定
// 「encode 意図が生き残る滞留の上限は epg.retention_grace であって、エッジの
// リングバッファの N 日ではない」を固定する。
//
// エッジのリングバッファは「回線断・クラウド側障害で未 ingest の record が
// N 日分溜まる」ことを前提にサイジングする（docs/operations.md §4）が、凍結の
// JOIN 先 program_snapshots の寿命は放送終了 + epg.retention_grace（既定 24h）で
// 決まる（docs/storage.md §6）。**2 つの時計の間に制約が無い**ので、猶予を
// 超えた滞留から復帰した ingest は予約を引けず、既定値で凍結される。
//
// このテストは「そうなる」ことを固定する ---
// TestIngestWorker_NoReservation_LeavesEncodePolicyDefault が「予約が最初から
// 無い」を模すのに対し、こちらは**予約と意図が確かに存在したうえで GC に刈られた**
// 経路を、実際の GC クエリ（DeleteEndedProgramSnapshots）を通して模す。
//
// **record_sync の program_id を programID に一致させるのが要点。** GC を未 ingest の
// record_sync と連動させる案（issue #214 の案 1）は、record_sync が持つ唯一の
// 番組キー (site, program_id) でスナップショットを引いて留め置く形になる。
// 汎用ヘルパ insertTestRecordSync は program_id を固定値でハードコードしている
// ので、それを使うとこの record_sync 行は GC 対象のスナップショットを指さず、
// **案 1 を実装してもこのテストは通り続ける**（実際に通ってしまっていた。
// PR #270 のレビューで発覚）。programID を渡せる insertTestRecordSyncForSite を
// 使い、案 1 の鍵付けを実際に成立させる。
//
// 案 1 を (site, program_id) で実装すると、このテストは
// 「DeleteEndedProgramSnapshots deleted 0 rows, want 1」で落ちる（確認済み）。
// 決定を変えるならこのテストも一緒に変える。
func TestIngestWorker_SnapshotGCedBeyondGrace_FreezesDefaults(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	programID := int64(900000000000010)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "滞留番組")
	setReservationBase(t, pool, res.ProgramID, `{"keepOriginal":"until_encoded","encodeProfiles":["h265"]}`)

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Source, fixtureParams.NetworkID, fixtureParams.ServiceID, fixtureParams.EventID = "rule", 32736, 1024, int32(programID%100000)
	})
	// program_id を programID に一致させる（doc コメント参照）。status='finished' /
	// 原本 media_asset なしなので、案 1 から見て「未 ingest の record」に該当する。
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-gced", programID)

	// 放送は 48 時間前に終わったことにする（insertProgramSnapshotAndReservation は
	// start_at = now() で作る）。GC の cutoff は epg.retention_grace = 24h 相当。
	if _, err := pool.Exec(ctx,
		"UPDATE program_snapshots SET start_at = now() - interval '48 hours' WHERE site = $1 AND program_id = $2",
		"default", programID,
	); err != nil {
		t.Fatalf("aging program snapshot: %v", err)
	}

	// ruler の GC 本体（internal/ruler.runGC が呼ぶのと同じクエリ）。FK CASCADE で
	// reservations / program_intents / program_overrides も一緒に落ちる。
	deleted, err := q.DeleteEndedProgramSnapshots(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("running GC: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("DeleteEndedProgramSnapshots deleted %d rows, want 1 "+
			"（未 ingest の record_sync（program_id=%d）があるスナップショットも刈るのが issue #214 の決定。"+
			"GC を record_sync と連動させる案 1 を実装したならここで落ちる —— 決定を変えるならこのテストも変える）",
			deleted, programID)
	}
	var reservations int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM reservations WHERE site = $1 AND program_id = $2", "default", programID,
	).Scan(&reservations); err != nil {
		t.Fatalf("counting reservations: %v", err)
	}
	if reservations != 0 {
		t.Fatalf("reservations after GC = %d, want 0 (FK CASCADE で一緒に落ちるはず)", reservations)
	}

	tsData := makeTSData(20)
	fake := newFullTransferFake(t, "rec-policy-gced", tsData, "test/policy-gced.m2ts")
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      t.TempDir(),
		RecordTarget:  pgstoretest.Recordings(pool),
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	job := IngestJobArgs{Site: "default", RecordID: "rec-policy-gced"}

	w.Enqueuer = newRecordingEnqueuer()
	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	// 凍結そのものはスキップされない（issue #159）。行はあり、値が既定値になる。
	if !encodePolicyRowExists(t, pool, recordingID) {
		t.Error("recording_encode_policy に行が無い。解決に失敗しても既定値で凍結する契約（issue #159）が破れている")
	}
	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "always" {
		t.Errorf("keep_original = %q, want always "+
			"（猶予超過の滞留では予約を引けないので既定値で凍結される。issue #214）", keepOriginal)
	}
	if len(profiles) != 0 {
		t.Errorf("encode_profiles = %v, want empty （予約の h265 は GC で失われている。issue #214）", profiles)
	}
	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID, "h265"); got != 0 {
		t.Errorf("encode jobs for h265 = %d, want 0 "+
			"（原本は keep_original='always' で残るがエンコードは投入されない。issue #214）", got)
	}
}

// TestIngestWorker_SnapshotsEncodePolicy_SurvivesReservationRematerialization は
// issue #149 の受け入れ基準「予約の再実体化を跨いでもエンコード方針が焼き込まれる」
// を確認する。
//
// ruler は EPG フリッカー・ルール編集・dedup で予約を導出削除・再実体化し、
// 予約行が再実体化される（#53 / #98 / #99 と同じ族。CLAUDE.md 不変条件 9
// 「identity」の 5 例目）。旧実装は録画開始時に watcher が焼いた
// recordings.reservation_id（当時 FK の ON DELETE SET NULL。issue #158 で
// 列自体を削除済み）を宛先に GetReservationEncodePolicy を引いていたため、
// 再実体化で NULL に落ちて「予約が無い」と誤認し、encode policy を凍結し
// 損なっていた。
//
// internal/reconciler/never_scheduled_identity_test.go の
// TestReconciler_NeverScheduledExclusionSurvivesRematerialization と同じ模し方
// （DELETE してから CreateManualReservation で作り直す）。
func TestIngestWorker_SnapshotsEncodePolicy_SurvivesReservationRematerialization(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	programID := int64(900000000000004)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "再実体化予約番組")
	setReservationBase(t, pool, res.ProgramID, `{"keepOriginal":"until_encoded","encodeProfiles":["h265"]}`)

	// watcher.createRecording が録画開始時に放送イベントキーを焼いた状態を模す。
	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Source, fixtureParams.NetworkID, fixtureParams.ServiceID, fixtureParams.EventID = "rule", 32736, 1024, int32(programID%100000)
	})
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-rematerialized", programID)

	// ruler の導出削除 → 再実体化を模す（同じキーで DELETE → 再 INSERT する。
	// 理由はこの関数の doc コメント）。
	if _, err := pool.Exec(ctx, `DELETE FROM reservations WHERE site = $1 AND program_id = $2`, res.Site, res.ProgramID); err != nil {
		t.Fatalf("deleting reservation: %v", err)
	}
	res2, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: "default", ProgramID: programID,
	})
	if err != nil {
		t.Fatalf("re-materializing reservation: %v", err)
	}
	// ruler は再実体化のたびに射影から base を書き直す。テストではその 1 パスを
	// 模して同じ base を新しい行に立て直す（setReservationBase 参照）。
	setReservationBase(t, pool, res2.ProgramID, `{"keepOriginal":"until_encoded","encodeProfiles":["h265"]}`)

	tsData := makeTSData(20)
	fake := newFullTransferFake(t, "rec-policy-rematerialized", tsData, "test/policy-rematerialized.m2ts")
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      t.TempDir(),
		RecordTarget:  pgstoretest.Recordings(pool),
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	job := IngestJobArgs{Site: "default", RecordID: "rec-policy-rematerialized"}

	w.Enqueuer = newRecordingEnqueuer()
	workCtx := context.Background()
	if err := runWorkerViaJobQFake(workCtx, w, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "until_encoded" {
		t.Errorf("keep_original = %q, want until_encoded (予約の再実体化を跨いで解決できているはず)", keepOriginal)
	}
	if !slices.Equal(profiles, []string{"h265"}) {
		t.Errorf("encode_profiles = %v, want [h265] (予約の再実体化を跨いで解決できているはず)", profiles)
	}
	if got := countEnqueuedForRecording(t, w.Enqueuer.(*recordingEnqueuer), "encode_reconcile", recordingID); got != 1 {
		t.Fatalf("targeted encode_reconcile jobs = %d, want 1", got)
	}
	passEnqueuer := runEncodeReconcilePassArgs(t, &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool)},
		jobs.EncodeReconcileArgs{RecordingID: recordingID})

	if got := countEncodeJobs(t, passEnqueuer, recordingID, "h265"); got != 1 {
		t.Errorf("encode jobs for h265 = %d, want 1", got)
	}
}

// TestIngestWorker_LogsWarnWhenRuleSourceReservationUnresolvable は
// source='rule'（DeriveRecordingSource が「作成時点で予約があり、intent
// action='record' の行は無かった」ことを保証する）の録画で放送イベントキーの
// JOIN が失敗する異常系を再現し、slog.Warn に識別子が残ることを確認する
// （レビュー指摘: internal/worker/ingest.go:539-547 の「引けなかった」を黙って
// return nil にしない、の rule 側）。
func TestIngestWorker_LogsWarnWhenRuleSourceReservationUnresolvable(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()

	programID := int64(900000000000005)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "恒久削除予約番組")
	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Source, fixtureParams.NetworkID, fixtureParams.ServiceID, fixtureParams.EventID = "rule", 32736, 1024, int32(programID%100000)
	})
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-rule-gone", programID)

	// 予約が恒久的に削除された（または GC が想定より早く走った）場合を模す
	// （再実体化しない。TestIngestWorker_SnapshotsEncodePolicy_SurvivesReservationRematerialization
	// と異なり、これが正常経路には無い異常系であることが本テストの前提）。
	// JOIN 失敗の 3 つ目の原因（GC は設計どおり走ったが ingest が猶予を跨いで
	// 遅れた。issue #214）は TestIngestWorker_SnapshotGCedBeyondGrace_FreezesDefaults
	// が別に持つ —— そちらは異常系ではなく設計が許容するシナリオ。
	if _, err := pool.Exec(ctx, `DELETE FROM reservations WHERE site = $1 AND program_id = $2`, res.Site, res.ProgramID); err != nil {
		t.Fatalf("deleting reservation: %v", err)
	}

	tsData := makeTSData(20)
	fake := newFullTransferFake(t, "rec-policy-rule-gone", tsData, "test/policy-rule-gone.m2ts")
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      t.TempDir(),
		RecordTarget:  pgstoretest.Recordings(pool),
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	job := IngestJobArgs{Site: "default", RecordID: "rec-policy-rule-gone"}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	workCtx := context.Background()
	if err := runWorkerViaJobQFake(workCtx, w, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	logText := logBuf.String()
	if !strings.Contains(logText, "level=WARN") {
		t.Errorf("expected a WARN log for source=rule reservation unresolvable, got:\n%s", logText)
	}
	if !strings.Contains(logText, "reservation not found via broadcast event key") {
		t.Errorf("expected log message to mention the unresolved lookup, got:\n%s", logText)
	}
	for _, want := range []string{
		fmt.Sprintf("recording_id=%d", recordingID),
		"network_id=32736",
		"service_id=1024",
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("expected log to contain identifier %q, got:\n%s", want, logText)
		}
	}

	// 解決に失敗しても凍結自体はスキップしない（issue #159。resolveAndSnapshotEncodePolicy
	// の doc コメント「解決に失敗しても凍結する」参照）ので既定値で凍結されるはず
	// （この関数の主張の対象ではないが、「ログは出たが desired が誤って書かれた」を
	// 排除するために確認する）。
	//
	// encodePolicyRowExists で行そのものの有無を確認する --- encodePolicyOfRecording
	// は ErrNoRows を既定値に潰すため、旧実装（ErrNoRows で return nil して凍結を
	// スキップする）に戻しても値の比較だけでは検出できない。行が無いと
	// 「原本 media_asset を持つ録画は凍結済み」という判定基準と issue #133 の
	// 事後追加（AppendRecordingEncodeProfiles が「行が既にある」前提で書けること）が破れる。
	if !encodePolicyRowExists(t, pool, recordingID) {
		t.Fatalf("recording_encode_policy row missing for recording %d; resolveAndSnapshotEncodePolicy must freeze defaults even when the lookup fails", recordingID)
	}
	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "always" || len(profiles) != 0 {
		t.Errorf("keep_original/encode_profiles = %q/%v, want always/[] (unresolved, frozen to defaults)", keepOriginal, profiles)
	}
}

// TestIngestWorker_LogsInfoWhenUnattributedSourceReservationUnresolvable は
// source='unattributed' の録画で JOIN が失敗した場合でも「引けなかった」が必ず
// Info ログに残ることを確認する。予約も intent も無い日常的な録画では、解決失敗は
// 異常ではないが、encode policy が既定値に凍結された事実は運用上追跡できる必要が
// ある。
func TestIngestWorker_LogsInfoWhenUnattributedSourceReservationUnresolvable(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Source = "unattributed"
	})
	insertTestRecordSync(t, pool, recordingID, "rec-policy-manual-unresolved")

	tsData := makeTSData(20)
	fake := newFullTransferFake(t, "rec-policy-manual-unresolved", tsData, "test/policy-manual-unresolved.m2ts")
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      t.TempDir(),
		RecordTarget:  pgstoretest.Recordings(pool),
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	job := IngestJobArgs{Site: "default", RecordID: "rec-policy-manual-unresolved"}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	workCtx := context.Background()
	if err := runWorkerViaJobQFake(workCtx, w, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	logText := logBuf.String()
	if !strings.Contains(logText, "level=INFO") {
		t.Errorf("expected an INFO log for source=unattributed reservation unresolvable, got:\n%s", logText)
	}
	if !strings.Contains(logText, "reservation not found via broadcast event key") {
		t.Errorf("expected log message to mention the unresolved lookup, got:\n%s", logText)
	}
	if !strings.Contains(logText, fmt.Sprintf("recording_id=%d", recordingID)) {
		t.Errorf("expected log to contain recording_id=%d, got:\n%s", recordingID, logText)
	}
	if strings.Contains(logText, "level=WARN") {
		t.Errorf("source=unattributed unresolved reservation must not be logged at WARN, got:\n%s", logText)
	}

	// source='rule' の対応テストと同じ理由で、行の有無そのものを確認する
	// （encodePolicyOfRecording は ErrNoRows を既定値に潰すため使えない）。
	if !encodePolicyRowExists(t, pool, recordingID) {
		t.Fatalf("recording_encode_policy row missing for recording %d; resolveAndSnapshotEncodePolicy must freeze defaults even when the lookup fails", recordingID)
	}
	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "always" || len(profiles) != 0 {
		t.Errorf("keep_original/encode_profiles = %q/%v, want always/[] (unresolved, frozen to defaults)", keepOriginal, profiles)
	}
}

// TestIngestWorker_LogsWarnWhenManualSourceReservationUnresolvable は、録画作成時に
// program_intents.action=record があった録画を先に作り、その後に snapshot ごと GC
// して予約解決を失敗させる。recordings.source は作成時の manual snapshot を保持する
// ので、unattributed と混同せず Warn になることを確認する。
func TestIngestWorker_LogsWarnWhenManualSourceReservationUnresolvable(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	programID := int64(900000000000011)
	insertProgramSnapshotAndReservation(t, pool, programID, "手動意図の GC 番組")
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: "default", ProgramID: programID, Action: reservation.IntentRecord,
	}); err != nil {
		t.Fatalf("creating record intent: %v", err)
	}
	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Source, fixtureParams.NetworkID, fixtureParams.ServiceID, fixtureParams.EventID = "rule", 32736, 1024, int32(programID%100000)
	})
	if _, err := pool.Exec(ctx, "UPDATE recordings SET source = $2 WHERE id = $1", recordingID, reservation.SourceManual); err != nil {
		t.Fatalf("marking recording manual: %v", err)
	}
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-manual-gced", programID)

	// program_snapshots の削除は reservations / program_intents を CASCADE する。
	if _, err := pool.Exec(ctx,
		"DELETE FROM program_snapshots WHERE site = $1 AND program_id = $2", "default", programID); err != nil {
		t.Fatalf("running snapshot GC: %v", err)
	}
	var intents, reservations int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM program_intents WHERE site = $1 AND program_id = $2", "default", programID,
	).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM reservations WHERE site = $1 AND program_id = $2", "default", programID,
	).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if intents != 0 || reservations != 0 {
		t.Fatalf("GC left intent/reservation rows: intents=%d reservations=%d", intents, reservations)
	}

	tsData := makeTSData(20)
	fake := newFullTransferFake(t, "rec-policy-manual-gced", tsData, "test/policy-manual-gced.m2ts")
	mc := mirakc.NewClient(fake.URL(), nil)
	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      t.TempDir(),
		RecordTarget:  pgstoretest.Recordings(pool),
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)
	job := IngestJobArgs{Site: "default", RecordID: "rec-policy-manual-gced"}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	logText := logBuf.String()
	if !strings.Contains(logText, "level=WARN") {
		t.Errorf("expected a WARN log for source=manual reservation unresolvable, got:\n%s", logText)
	}
	if !strings.Contains(logText, "reservation not found via broadcast event key") {
		t.Errorf("expected log message to mention the unresolved lookup, got:\n%s", logText)
	}
	if !strings.Contains(logText, fmt.Sprintf("recording_id=%d", recordingID)) {
		t.Errorf("expected log to contain recording_id=%d, got:\n%s", recordingID, logText)
	}
}

// TestIngestWorker_RelPathPrefixedWithSite は、issue #186 (M4-14) 受け入れの
// 1 項目目を固定する: site "tokyo" の worker が ingest した原本の
// media_assets.rel_path が "sites/tokyo/" で始まり、実ファイルがその下に置かれる
// （contentPath に階層があってもその下に入る）。
//
// catalog.SiteRelPath が site 前置をしなくなると rel_path が
// "20240101/prog.m2ts" のままになり、このテストは失敗する。
func TestIngestWorker_RelPathPrefixedWithSite(t *testing.T) {
	tsData := makeTSData(10)
	const recordID = "rec-tokyo"
	fake := newRecordPathFake(t, recordID, tsData, strPtr("20240101/prog.m2ts"), "/recording/20240101/prog.m2ts")

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		MirakcClients: singleSiteClients("tokyo", mc),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Precheck = pgstoretest.Media(pool)
	w.Publisher = pgstoretest.Media(pool)
	w.RecordTarget = pgstoretest.Recordings(pool)
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "tokyo", 101
	})
	insertTestRecordSyncForSite(t, pool, "tokyo", recordingID, recordID, 327361024000101)

	job := IngestJobArgs{Site: "tokyo", RecordID: recordID}
	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	q := sqlcgen.New(pool)
	asset, err := q.GetActiveOriginalMediaAsset(context.Background(), recordingID)
	if err != nil {
		t.Fatalf("GetActiveOriginalMediaAsset: %v", err)
	}
	const wantRelPath = "sites/tokyo/20240101/prog.m2ts"
	if asset.RelPath != wantRelPath {
		t.Errorf("media_assets.rel_path = %q, want %q (site prefix missing)", asset.RelPath, wantRelPath)
	}

	fullPath := filepath.Join(mediaDir, "sites", "tokyo", "20240101", "prog.m2ts")
	if _, err := os.Stat(fullPath); err != nil {
		t.Errorf("expected file at %s (site-prefixed dir): %v", fullPath, err)
	}
}

// TestIngestWorker_RelPathPrefixedWithSite_FallbackContentPath は受け入れの
// 2 項目目を固定する: mirakc の contentPath が空で filepath.Base(record.Content.Path)
// にフォールバックする経路でも "sites/{site}/" が前置される。
func TestIngestWorker_RelPathPrefixedWithSite_FallbackContentPath(t *testing.T) {
	tsData := makeTSData(10)
	// ContentPath を nil にしてフォールバック経路を通す。
	const recordID = "rec-tokyo-fallback"
	fake := newRecordPathFake(t, recordID, tsData, nil, "/recording/fallback/plain.m2ts")

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		MirakcClients: singleSiteClients("tokyo", mc),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Precheck = pgstoretest.Media(pool)
	w.Publisher = pgstoretest.Media(pool)
	w.RecordTarget = pgstoretest.Recordings(pool)
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "tokyo", 102
	})
	insertTestRecordSyncForSite(t, pool, "tokyo", recordingID, recordID, 327361024000102)

	job := IngestJobArgs{Site: "tokyo", RecordID: recordID}
	if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	q := sqlcgen.New(pool)
	asset, err := q.GetActiveOriginalMediaAsset(context.Background(), recordingID)
	if err != nil {
		t.Fatalf("GetActiveOriginalMediaAsset: %v", err)
	}
	// フォールバックは filepath.Base なので階層は失われ "plain.m2ts" だけが残る。
	// そこに "sites/tokyo/" が前置される。
	const wantRelPath = "sites/tokyo/plain.m2ts"
	if asset.RelPath != wantRelPath {
		t.Errorf("media_assets.rel_path = %q, want %q (site prefix missing on fallback path)", asset.RelPath, wantRelPath)
	}
}

// TestIngestWorker_DegenerateContentPath_Rejected は、mirakc の contentPath /
// Content.Path がどちらも空という縮退したレスポンスを拒否し、誤った保存先を
// 作らないことを固定する。
//
// 空パスを前置してから解決すると "sites/{site}/." が site directory になり、
// ファイルを指す rel_path として通ってしまう。catalog.SiteRelPath の直接テストで
// 検査を固定し、この経路では不正な保存先を作らないことも確認する。
func TestIngestWorker_DegenerateContentPath_Rejected(t *testing.T) {
	tsData := makeTSData(10)
	// ContentPath も Content.Path も空。
	const recordID = "rec-tokyo-degenerate"
	fake := newRecordPathFake(t, recordID, tsData, nil, "")

	mediaDir := t.TempDir()
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		MirakcClients: singleSiteClients("tokyo", mc),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
	}

	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w.Precheck = pgstoretest.Media(pool)
	w.Publisher = pgstoretest.Media(pool)
	w.RecordTarget = pgstoretest.Recordings(pool)

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "tokyo", 103
	})
	insertTestRecordSyncForSite(t, pool, "tokyo", recordingID, recordID, 327361024000103)

	job := IngestJobArgs{Site: "tokyo", RecordID: recordID}
	if err := runWorkerViaJobQFake(context.Background(), w, job); err == nil {
		t.Fatal("Work() error = nil, want non-nil for a record with no usable content path")
	}

	// 副作用（sites/tokyo をファイルとして作る等）が残っていないことを確認する。
	sitesDir := filepath.Join(mediaDir, "sites")
	entries, statErr := os.ReadDir(sitesDir)
	if statErr == nil {
		for _, e := range entries {
			if e.Name() == "tokyo" && !e.IsDir() {
				t.Errorf("sites/tokyo was created as a regular file (would break all future ingests for this site): %+v", e)
			}
		}
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("reading %s: %v", sitesDir, statErr)
	}
}

func TestIngestWorker_ReservedStorageBasenameRejected(t *testing.T) {
	for _, contentPath := range []string{
		".rokuban-ingest-user.m2ts",
		".rokuban-rel-path-lock-user.m2ts",
		".rokuban-encode-user.m2ts",
		".rokuban-media-asset-user.m2ts",
	} {
		t.Run(contentPath, func(t *testing.T) {
			fake := newRecordPathFake(t, "reserved-basename", nil, strPtr(contentPath), "/recording/"+contentPath)
			w := &IngestWorker{MediaDir: t.TempDir()}
			client := mirakc.NewClient(fake.URL(), nil)
			_, _, _, err := w.determineRelPath(context.Background(), IngestJobArgs{
				Site: "default", RecordID: "reserved-basename",
			}, client)
			if err == nil {
				t.Fatal("determineRelPath returned nil for a reserved storage basename")
			}
			if !strings.Contains(err.Error(), "reserved storage filename") {
				t.Fatalf("determineRelPath error = %v, want reserved storage filename", err)
			}
		})
	}
}

// TestIngestWorker_TwoSitesSameContentPath_DoNotCollide は受け入れの 3 項目目
// （罠の核心）を固定する: 同じ contentPath を持つ 2 サイトの record を ingest
// しても、rel_path の site 前置で実ファイルが別々になり両方が commit できる。
//
// 反転確認: determineRelPath の前置（"relPath = "sites/" + args.Site + "/" +
// relPath"）を削ると、2 回目の Work() が media_assets の一意索引違反
// （CREATE UNIQUE INDEX ON media_assets (rel_path) WHERE state <> 'deleted'）で
// 失敗し、このテストは "second Work() error" で落ちる。実際に前置行を削って
// 確認済み（PR 本文参照）。
func TestIngestWorker_TwoSitesSameContentPath_DoNotCollide(t *testing.T) {
	setFastIngestFollow(t)
	tsDataA := makeTSData(10)
	tsDataB := makeTSData(20)
	const sharedContentPath = "shared/recording.m2ts"

	fakeA := newRecordPathFake(t, "rec-collide-a", tsDataA, strPtr(sharedContentPath), "/recording/"+sharedContentPath)
	fakeB := newRecordPathFake(t, "rec-collide-b", tsDataB, strPtr(sharedContentPath), "/recording/"+sharedContentPath)

	mediaDir := t.TempDir()

	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	recordingIDA := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "site-a", 201
	})
	insertTestRecordSyncForSite(t, pool, "site-a", recordingIDA, "rec-collide-a", 327361024000201)
	recordingIDB := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "site-b", 202
	})
	insertTestRecordSyncForSite(t, pool, "site-b", recordingIDB, "rec-collide-b", 327361024000202)

	wA := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("site-a", mirakc.NewClient(fakeA.URL(), nil)),
		RecordTarget:  pgstoretest.Recordings(pool),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	wA.CMDetectionStore = pgstoretest.Analysis(pool)
	wB := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("site-b", mirakc.NewClient(fakeB.URL(), nil)),
		RecordTarget:  pgstoretest.Recordings(pool),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	wB.CMDetectionStore = pgstoretest.Analysis(pool)

	jobA := IngestJobArgs{Site: "site-a", RecordID: "rec-collide-a"}
	jobB := IngestJobArgs{Site: "site-b", RecordID: "rec-collide-b"}

	if err := runWorkerViaJobQFake(context.Background(), wA, jobA); err != nil {
		t.Fatalf("first Work() (site-a) error: %v", err)
	}
	if err := runWorkerViaJobQFake(context.Background(), wB, jobB); err != nil {
		t.Fatalf("second Work() (site-b) error: %v (site prefix likely missing, colliding on rel_path %q)", err, sharedContentPath)
	}

	pathA := filepath.Join(mediaDir, "sites", "site-a", "shared", "recording.m2ts")
	pathB := filepath.Join(mediaDir, "sites", "site-b", "shared", "recording.m2ts")

	dataA, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatalf("reading site-a output file: %v", err)
	}
	if len(dataA) != len(tsDataA) {
		t.Errorf("site-a file size = %d, want %d", len(dataA), len(tsDataA))
	}

	dataB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatalf("reading site-b output file: %v", err)
	}
	if len(dataB) != len(tsDataB) {
		t.Errorf("site-b file size = %d, want %d", len(dataB), len(tsDataB))
	}

	q := sqlcgen.New(pool)
	assetA, err := q.GetActiveOriginalMediaAsset(context.Background(), recordingIDA)
	if err != nil {
		t.Fatalf("GetActiveOriginalMediaAsset(site-a): %v", err)
	}
	assetB, err := q.GetActiveOriginalMediaAsset(context.Background(), recordingIDB)
	if err != nil {
		t.Fatalf("GetActiveOriginalMediaAsset(site-b): %v", err)
	}
	if assetA.RelPath == assetB.RelPath {
		t.Errorf("both sites committed the same rel_path %q; site prefix must make them distinct", assetA.RelPath)
	}
	if assetA.RelPath != "sites/site-a/"+sharedContentPath {
		t.Errorf("site-a rel_path = %q, want %q", assetA.RelPath, "sites/site-a/"+sharedContentPath)
	}
	if assetB.RelPath != "sites/site-b/"+sharedContentPath {
		t.Errorf("site-b rel_path = %q, want %q", assetB.RelPath, "sites/site-b/"+sharedContentPath)
	}
}

// --- issue #197: 一意性を知る前に canonical file を truncate して
// （既存の active な media_asset を壊して 23505 で落ちる）の回帰テスト。

// TestIngestWorker_RelPathConflict_RefusesWithoutCorruptingExistingFile は
// issue #197 の受け入れ基準 1 項目目を固定する: 同じ rel_path になる 2 つ目の
// ingest が、先行の実ファイルを壊さずに失敗する。
//
// checkRelPathConflict の述語は `state <> 'deleted'`（一意索引の述語と同じ）
// であり、'active' と 'deleting' の両方を「衝突」として塞ぐ。これを
// `state = 'active'` に狭める変異（'deleting' を見逃す）は worker パッケージ
// 全緑のまま通ってしまう穴だった（PR #267 のレビューで指摘・実測）。
// 'deleting' 行のファイルは delete_reconcile の 3 段階
// （active → deleting → unlink → deleted、delete_execute.go の
// deleteMediaAsset）のうち unlink 前・unlink 失敗中はまだ実在し、かつ
// resolveUnqualifiedDeletingAsset がファイルの現存を確認した上でその行を
// active に戻しうる（issue #105）。したがって 'deleting' 行のファイルを
// ingest が上書きすると、それが後から active として復元され、issue #197
// が問題にした「DB は active、実体は別番組」がここでも再生産される。
// そのためテーブル駆動で 'active' / 'deleting' の両方を確認する
// （CLAUDE.md テスト規律「分岐を直したら両方向で確認する」）。
func TestIngestWorker_RelPathConflict_RefusesWithoutCorruptingExistingFile(t *testing.T) {
	for _, existingState := range []string{"active", "deleting"} {
		t.Run(existingState, func(t *testing.T) {
			testRelPathConflictRefusesWithoutCorruptingExistingFile(t, existingState)
		})
	}
}

// testRelPathConflictRefusesWithoutCorruptingExistingFile は
// TestIngestWorker_RelPathConflict_RefusesWithoutCorruptingExistingFile の
// 本体。existingState は先行の media_asset に与える state（"active" /
// "deleting"）。
//
// このテストが実際に守っているもの（一時ファイル方式では checkRelPathConflict
// を無効化しても先行ファイルは壊れない --- commit は canonical path を temp から
// の rename でしか触らないので truncate も上書きも起きない。実測: 事前チェックを
// 無効化する変異では先行ファイルはバイト一致のまま残り、落ちるのは下記の
// エラー文言アサーションと streamRequests == 0 のアサーションだけだった）:
// 事前チェックが無いと、2 つ目の Work() は衝突に気付かないまま無駄な全量転送を
// 完走し、commit 内の media_assets 一意索引違反（23505）でようやく失敗する
// --- エラーメッセージは "refusing to overwrite" ではなく生の unique-violation
// になり、streamRequests も 0 のままにはならない。事前チェックはこの無駄な
// 転送とエラー文言の劣化を防ぐためのものであり、先行ファイルの保護そのものは
// 一時ファイル方式が事前チェック無しでも既に満たしている。
func testRelPathConflictRefusesWithoutCorruptingExistingFile(t *testing.T, existingState string) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()

	// 先行: 既に existingState の media_asset として commit 済みの録画
	// （別の recording_id）。実ファイルは既存の「正しい」中身を持つ。
	existingRecordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 301
	})
	const conflictRelPath = "sites/default/shared/conflict.m2ts"
	existingAssetID := dbtest.MediaAsset(t, pool, existingRecordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.RelPath = conflictRelPath
		fixtureParams.SizeBytes = 21
	})

	if existingState != "active" {
		// MediaDelete.MarkDeleting（delete_execute.go）は deleted_at を
		// 立てない --- 'deleting' は unlink 前後の中間状態であり、不可逆な
		// 削除確定（deleted_at）はまだ起きていない。ここでもその形を模す。
		if _, err := pool.Exec(ctx,
			"UPDATE media_assets SET state = $2 WHERE id = $1", existingAssetID, existingState,
		); err != nil {
			t.Fatalf("setting existing media_asset state to %s: %v", existingState, err)
		}
	}

	existingContent := []byte("existing-untouched-01") // 21 バイト
	if len(existingContent) != 21 {
		t.Fatalf("test fixture bug: existingContent must be 21 bytes, got %d", len(existingContent))
	}
	conflictFullPath := filepath.Join(mediaDir, "sites", "default", "shared", "conflict.m2ts")
	if err := os.MkdirAll(filepath.Dir(conflictFullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conflictFullPath, existingContent, 0o644); err != nil {
		t.Fatal(err)
	}

	// 新規: 同じ contentPath（→ 同じ site 前置後の rel_path）を持つ別の録画。
	newRecordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 302
	})
	const recordID = "rec-conflict-new"
	insertTestRecordSyncForSite(t, pool, "default", newRecordingID, recordID, 327361024000302)

	tsDataNew := makeTSData(20)
	fake := newFullTransferFake(t, recordID, tsDataNew, "shared/conflict.m2ts")

	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
		RecordTarget:  pgstoretest.Recordings(pool),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}

	job := IngestJobArgs{Site: "default", RecordID: recordID}

	err := runWorkerViaJobQFake(ctx, w, job)
	if err == nil {
		t.Fatal("Work() error = nil, want an explicit failure for a conflicting rel_path")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("Work() error = %v, want it to mention refusing to overwrite (distinct from a bare 23505 unique-violation message)", err)
	}

	// 核心のアサーション: 先行の実ファイルは中身まで元のままである
	// （サイズが同じだけでは truncate → 途中まで書いた場合を見逃す）。
	gotContent, err := os.ReadFile(conflictFullPath)
	if err != nil {
		t.Fatalf("reading pre-existing file after failed Work(): %v", err)
	}
	if !bytes.Equal(gotContent, existingContent) {
		t.Errorf("pre-existing file was modified: got %q, want unchanged %q", gotContent, existingContent)
	}

	// 転送そのものが始まっていないこと（「開始してから失敗」ではなく
	// 「開始前に失敗」であることの確認）。
	if got := fake.CallCount(http.MethodGet, "/api/recording/records/"+recordID+"/stream"); got != 0 {
		t.Errorf("stream requests = %d, want 0 (transfer must not start when rel_path already conflicts)", got)
	}

	// 新規録画側は media_asset がコミットされていない。
	var newAssetCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM media_assets WHERE recording_id = $1", newRecordingID,
	).Scan(&newAssetCount); err != nil {
		t.Fatalf("counting media_assets for new recording: %v", err)
	}
	if newAssetCount != 0 {
		t.Errorf("media_assets rows for new recording = %d, want 0 (must not commit on conflict)", newAssetCount)
	}

	// 先行録画側の行は変わらず 1 のまま。
	var existingAssetCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM media_assets WHERE recording_id = $1", existingRecordingID,
	).Scan(&existingAssetCount); err != nil {
		t.Fatalf("counting media_assets for existing recording: %v", err)
	}
	if existingAssetCount != 1 {
		t.Errorf("media_assets rows for existing recording = %d, want 1 (untouched)", existingAssetCount)
	}
}

// TestIngestWorker_RelPathConflict_AllowsReuseAfterDeleted は issue #197 の
// 受け入れ基準 2 項目目（罠: state <> 'deleted' の条件を落とさない）を固定する:
// 同じ rel_path を持つ既存行が state='deleted'（tombstone）の場合は、その
// rel_path を新しい ingest が正当に再利用できる --- 既存の削除済み行を
// 「active な衝突」として誤って弾いてはいけない。
//
// checkRelPathConflict の `AND state <> 'deleted'` を落とす（例えば無条件で
// rel_path 一致を見る）と、このテストは "Work() error" で失敗する
// （tombstone を衝突と誤認して転送を始めずに落ちる）。
func TestIngestWorker_RelPathConflict_AllowsReuseAfterDeleted(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()

	// 先行: 同じ rel_path を使っていたが、既に削除済み（tombstone）の行。
	deletedRecordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 311
	})
	const reusedRelPath = "sites/default/shared/reused.m2ts"
	q := sqlcgen.New(pool)
	deletedAssetID := dbtest.MediaAsset(t, pool, deletedRecordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.RelPath = reusedRelPath
		fixtureParams.SizeBytes = 9
	})

	if _, err := pool.Exec(ctx,
		"UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE id = $1", deletedAssetID,
	); err != nil {
		t.Fatalf("marking media_asset deleted: %v", err)
	}

	// 新規: 同じ rel_path になる録画。
	newRecordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 312
	})
	insertTestRecordSyncForSite(t, pool, "default", newRecordingID, "rec-reuse-new", 327361024000312)

	tsData := makeTSData(15)
	fake := newRecordPathFake(t, "rec-reuse-new", tsData, strPtr("shared/reused.m2ts"), "/recording/shared/reused.m2ts")

	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
		RecordTarget:  pgstoretest.Recordings(pool),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	job := IngestJobArgs{Site: "default", RecordID: "rec-reuse-new"}

	if err := runWorkerViaJobQFake(ctx, w, job); err != nil {
		t.Fatalf("Work() error = %v, want nil (a tombstoned rel_path must be reusable)", err)
	}

	fullPath := filepath.Join(mediaDir, "sites", "default", "shared", "reused.m2ts")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if len(data) != len(tsData) {
		t.Errorf("file size = %d, want %d", len(data), len(tsData))
	}

	asset, err := q.GetActiveOriginalMediaAsset(ctx, newRecordingID)
	if err != nil {
		t.Fatalf("GetActiveOriginalMediaAsset: %v", err)
	}
	if asset.RelPath != reusedRelPath {
		t.Errorf("new media_asset rel_path = %q, want %q", asset.RelPath, reusedRelPath)
	}
}

func TestIngestSHA256WaitFor(t *testing.T) {
	for _, tt := range []struct {
		name string
		size int64
		want time.Duration
	}{
		{"unknown or empty uses the floor", 0, 10 * time.Second},
		{"small file uses the floor", 7_800_000 * 5, 10 * time.Second},
		{"just above the floor", 7_800_000 * 11, 11 * time.Second},
		{"rounds up", 7_800_000*11 + 1, 12 * time.Second},
		{"terrestrial 30 min", 3_800_000_000, 488 * time.Second},
		{"BS 2 h", 20_000_000_000, 2565 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ingestSHA256WaitFor(tt.size); got != tt.want {
				t.Errorf("ingestSHA256WaitFor(%d) = %s, want %s", tt.size, got, tt.want)
			}
		})
	}
}

func TestIngestSHA256WaitDeadline(t *testing.T) {
	setIngestSHA256Rate(t, 100)
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	tempModTime := now.Add(-2 * time.Hour)
	pastEndTime := mirakc.Milliseconds(now.Add(-time.Minute))
	futureEndTime := mirakc.Milliseconds(now.Add(time.Minute))
	zeroEndTime := mirakc.Milliseconds(time.Time{})
	for _, tt := range []struct {
		name    string
		endTime *mirakc.Milliseconds
		want    time.Time
	}{
		{name: "uses recording end time", endTime: &pastEndTime, want: pastEndTime.Time().Add(12 * time.Second)},
		{name: "missing end time uses temp mtime", want: tempModTime.Add(12 * time.Second)},
		{name: "future end time uses temp mtime", endTime: &futureEndTime, want: tempModTime.Add(12 * time.Second)},
		{name: "zero end time uses temp mtime", endTime: &zeroEndTime, want: tempModTime.Add(12 * time.Second)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ingestSHA256WaitDeadline(1200, tt.endTime, tempModTime, now); !got.Equal(tt.want) {
				t.Errorf("ingestSHA256WaitDeadline() = %s, want %s", got, tt.want)
			}
		})
	}
}

// setIngestSHA256Rate は SHA-256 待ちの速度見積もりをテストの間だけ差し替える。
func setIngestSHA256Rate(t *testing.T, rate int64) {
	t.Helper()
	old := ingestSHA256BytesPerSecond
	ingestSHA256BytesPerSecond = rate
	t.Cleanup(func() { ingestSHA256BytesPerSecond = old })
}

// setFastIngestFollow は録画追従の「追い付き時のポーリング間隔」と「再試行バック
// オフ」を数 ms に差し替え、t.Cleanup で戻す。待ちの長さ自体を検証していない
// ingest テストが、本番値（0.5〜1 秒のポーリング、最長 3.2 秒のバックオフ）を
// 実時間で払わないための差し替え。再試行の回数・順序は変わらない。
func setFastIngestFollow(t *testing.T) {
	t.Helper()
	prev := ingestFollowPace
	ingestFollowPace.pollMin = 5 * time.Millisecond
	ingestFollowPace.pollMax = 10 * time.Millisecond
	ingestFollowPace.retryDelay = func(int) time.Duration { return 5 * time.Millisecond }
	t.Cleanup(func() { ingestFollowPace = prev })
}
