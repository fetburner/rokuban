/* Chase request handling and record-following input. */

package streamer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
)

// chaseInputFailureCooldown は追っかけ入力の失敗後、同じ録画の全 offset で新しい
// セッションを作らない期間。再生位置の offset で張り直されても、入力障害中に
// ffmpeg と mirakc の要求を作り直し続けないようにする。
//
// 判定基準: フロントの再生中の失敗は、hls.js のプレイリスト再取得が規定の再試行
// （playlistLoadPolicy の errorRetry: maxNumRetry 2、1 秒→2 秒の backoff で計 3 秒前後）を
// 使い切って fatal になり、再生元を選び直した probe が 502 を受ける順で進む。この全体が
// cooldown に収まらないと、hls.js の再取得自体が先頭から作り直しを起こす。
// そのため再試行が尽きるまでの時間より長くとる。10 秒はその 3 倍強の余裕で、実測値ではない。
var chaseInputFailureCooldown = 10 * time.Second

// ChaseTarget is the durable-to-mirakc mapping needed by a chase session.
// recordingID is the canonical recordings.id from the URL; RecordID is the
// opaque id accepted by mirakc and is never exposed in the public URL.
type ChaseTarget struct {
	RecordingID      int64
	Site             string
	RecordID         string
	Status           string
	RecordingStatus  string
	HasOriginalAsset bool
}

// LookupChaseTarget resolves a recording id without starting a session. It
// deliberately returns finished recordings too: a completed chase session
// keeps its EVENT playlist until idle GC, and the browser still needs to fetch
// that playlist after the recording status changes. ChasePlaylistForTarget
// decides whether a missing session may be started from the returned statuses.
func (ls *LiveStreamer) LookupChaseTarget(ctx context.Context, recordingID int64) (ChaseTarget, error) {
	if ls.pool == nil {
		return ChaseTarget{}, errors.New("chase target database is unavailable")
	}
	row, err := sqlcgen.New(ls.pool).GetChaseTarget(ctx, recordingID)
	if err != nil {
		return ChaseTarget{}, err
	}
	if row.DeletedAt != nil || row.Site == "" || row.RecordID == "" {
		return ChaseTarget{}, pgx.ErrNoRows
	}
	return ChaseTarget{
		RecordingID:      recordingID,
		Site:             row.Site,
		RecordID:         row.RecordID,
		Status:           row.Status,
		RecordingStatus:  row.RecordingStatus,
		HasOriginalAsset: row.HasOriginalAsset,
	}, nil
}

// canStartChaseSession reports whether mirakc still owns an uncommitted original
// that a new chase session may follow. A media_assets row blocks a new session
// regardless of state because even a deleted row proves ingest already committed.
func (target ChaseTarget) canStartChaseSession() bool {
	canFollowRecord := target.Status == "recording" || target.Status == "finished"
	canFollowRecording := target.RecordingStatus == "recording" || target.RecordingStatus == "finished"
	return canFollowRecord && canFollowRecording && !target.HasOriginalAsset
}

func chaseSessionKeyFor(recordingID, offsetSeconds int64) sessionKey {
	return sessionKey{
		kind:          chaseSessionKind,
		id:            recordingID,
		offsetSeconds: offsetSeconds,
	}
}

// parseCanonicalChaseOffset accepts the decimal seconds used in the chase URL.
// Keeping the spelling canonical prevents one requested position from creating
// multiple sessions or segment directories.
func parseCanonicalChaseOffset(raw string) (int64, bool) {
	if raw == "" {
		return 0, true
	}
	if len(raw) > 1 && raw[0] == '0' {
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 || strconv.FormatInt(v, 10) != raw {
		return 0, false
	}
	return v, true
}

func chaseOffsetFromRequest(r *http.Request) (int64, bool) {
	return parseCanonicalChaseOffset(chi.URLParam(r, "offset"))
}

// ChasePlaylist handles the site-local form of the chase route. Only the
// playlist lookup touches the database; segments use the in-memory session.
func (ls *LiveStreamer) ChasePlaylist(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "site") != ls.site {
		http.NotFound(w, r)
		return
	}
	recordingID, ok := parseCanonicalRecordingID(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	target, err := ls.LookupChaseTarget(r.Context(), recordingID)
	if err != nil {
		writeChaseTargetError(w, r, err)
		return
	}
	if target.Site != ls.site {
		http.NotFound(w, r)
		return
	}
	ls.ChasePlaylistForTarget(w, r, target)
}

// ChasePlaylistForTarget starts or joins the one shared ffmpeg session for a
// recording and serves an EVENT playlist. The omitted offset starts at the
// recording head; a non-zero offset starts from the requested recording-relative
// second. Viewers with the same recording and offset share one session.
func (ls *LiveStreamer) ChasePlaylistForTarget(w http.ResponseWriter, r *http.Request, target ChaseTarget) {
	if target.Site != ls.site {
		http.NotFound(w, r)
		return
	}
	offsetSeconds, ok := chaseOffsetFromRequest(r)
	if !ok {
		http.Error(w, "invalid chase offset", http.StatusBadRequest)
		return
	}
	profile, ok := ls.cfg.profile(r.URL.Query().Get("profile"))
	if !ok {
		http.Error(w, "unknown chase profile", http.StatusBadRequest)
		return
	}
	key := chaseSessionKeyFor(target.RecordingID, offsetSeconds)
	var s *liveSession
	if target.canStartChaseSession() {
		var err error
		s, err = ls.existingChaseSessionOrCooldown(r.Context(), key)
		if err != nil {
			if _, retryable := liveEvictionReason(err); !retryable {
				writeSessionError(w, err)
				return
			}
			// 起動に失敗した進行中のセッションは、共通の退避・再試行経路（下の
			// recoverSessionStartup。s.done の待ちもそこで行う）に通す。健全なセッションと
			// cooldown 応答は、offset のメタデータ要求より前に処理する。
			s, err = ls.recoverSessionStartup(r.Context(), key, s.source, s, err)
			if err != nil {
				writeSessionError(w, err)
				return
			}
		}
		if s == nil {
			committedSize := ls.committedOriginalSize(target.RecordingID)
			var source sessionSource
			if offsetSeconds == 0 {
				// 先頭からでも、追従配信が閉じた後は Range で続きを読む（followChaseRecord）。
				client, ok := ls.mirakc.(mirakcSeekRecordClient)
				if !ok {
					http.Error(w, "chase stream unavailable", http.StatusServiceUnavailable)
					return
				}
				source = func(ctx context.Context) (io.ReadCloser, error) {
					return followChaseRecord(ctx, client, target.RecordID, committedSize)
				}
			} else {
				client, ok := ls.mirakc.(mirakcSeekRecordClient)
				if !ok {
					http.Error(w, "chase offset stream unavailable", http.StatusServiceUnavailable)
					return
				}
				record, err := client.GetRecord(r.Context(), target.RecordID)
				if err != nil {
					slog.Error("streamer: getting chase record metadata", "record_id", target.RecordID, "err", err)
					http.Error(w, "chase offset stream unavailable", http.StatusServiceUnavailable)
					return
				}
				startByte, err := chaseStartByteOffset(record, offsetSeconds)
				if err != nil {
					if errors.Is(err, errChaseOffsetUnavailable) {
						http.Error(w, "chase offset is outside the available recording range", http.StatusRequestedRangeNotSatisfiable)
						return
					}
					http.Error(w, "chase offset stream is not ready", http.StatusServiceUnavailable)
					return
				}
				source = func(ctx context.Context) (io.ReadCloser, error) {
					return waitForChaseRecordAtOffset(ctx, client, target.RecordID, startByte, committedSize)
				}
			}
			s, err = ls.getOrCreateSessionFor(r.Context(), key, source)
			if err != nil {
				writeSessionError(w, err)
				return
			}
		}
	} else {
		// The recording finished after the session was created. Keep serving the
		// retained EVENT playlist, but never start a second mirakc follow session.
		ls.mu.Lock()
		var ok bool
		s, ok = ls.chaseSessions[key]
		ls.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if err := waitReadyTouching(r.Context(), s, playlistStartupTimeout); err != nil {
			if errors.Is(err, errStartupTimeout) {
				http.Error(w, "chase stream did not start in time", http.StatusGatewayTimeout)
			}
			return
		}
		if s.startErr != nil {
			http.NotFound(w, r)
			return
		}
	}
	s.touch()

	// captions 無効時も音声レンディションを含む per-profile master を返す。
	playlistName := profile.Name + ".m3u8"
	readyMarker := "#EXT-X-STREAM-INF"
	if ls.cfg.Captions {
		playlistName = "playlist.m3u8"
	}
	playlistPath := filepath.Join(s.dir, playlistName)
	content, ok := waitForPlaylist(r.Context(), s, playlistPath, playlistStartupTimeout, readyMarker)
	if !ok {
		slog.Error("streamer: chase playlist did not appear in time",
			"recording_id", target.RecordingID, "profile", profile.Name, "dir", s.dir)
		http.Error(w, "chase stream did not start in time", http.StatusGatewayTimeout)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(content)
}

// ChaseSegment serves segments and variant playlists from the recording-keyed
// session. The site in the URL selects the LiveStreamer, so the hot segment path
// does not query Postgres repeatedly.
func (ls *LiveStreamer) ChaseSegment(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "site") != ls.site {
		http.NotFound(w, r)
		return
	}
	recordingID, ok := parseCanonicalRecordingID(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	name := chi.URLParam(r, "name")
	if !ls.cfg.servesFile(name) {
		http.Error(w, "invalid segment name", http.StatusBadRequest)
		return
	}

	offsetSeconds, ok := chaseOffsetFromRequest(r)
	if !ok {
		http.Error(w, "invalid chase offset", http.StatusBadRequest)
		return
	}
	ls.mu.Lock()
	s, ok := ls.chaseSessions[chaseSessionKeyFor(recordingID, offsetSeconds)]
	ls.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := waitReadyTouching(r.Context(), s, playlistStartupTimeout); err != nil {
		if errors.Is(err, errStartupTimeout) {
			http.Error(w, "chase stream did not start in time", http.StatusGatewayTimeout)
		}
		return
	}
	if s.startErr != nil {
		http.NotFound(w, r)
		return
	}
	s.touch()

	path := sessionFilePath(s.dir, name)
	if strings.HasSuffix(name, ".m3u8") {
		content, ok := waitForPlaylist(r.Context(), s, path, playlistStartupTimeout, "#EXTINF")
		if !ok {
			http.Error(w, "chase stream did not start in time", http.StatusGatewayTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(content)
		return
	}
	if filepath.Ext(name) == ".vtt" {
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "video/mp2t")
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

// ChaseLeave is a leave hint, not a stop command, with the same shared-session
// semantics as live Leave. A missing session is intentionally still 204.
func (ls *LiveStreamer) ChaseLeave(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "site") != ls.site {
		http.NotFound(w, r)
		return
	}
	recordingID, ok := parseCanonicalRecordingID(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	offsetSeconds, ok := chaseOffsetFromRequest(r)
	if !ok {
		http.Error(w, "invalid chase offset", http.StatusBadRequest)
		return
	}
	ls.mu.Lock()
	s, ok := ls.chaseSessions[chaseSessionKeyFor(recordingID, offsetSeconds)]
	ls.mu.Unlock()
	if !ok {
		metrics.LiveLeaveHints.WithLabelValues("no_session").Inc()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.hintLeave(time.Now(), ls.cfg.leaveGrace(), ls.cfg.IdleTimeout) {
		metrics.LiveLeaveHints.WithLabelValues("no_effect").Inc()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	metrics.LiveLeaveHints.WithLabelValues("deadline_shortened").Inc()
	slog.Info("streamer: chase leave hint received, shortening idle deadline",
		"recording_id", recordingID, "grace", ls.cfg.leaveGrace())
	w.WriteHeader(http.StatusNoContent)
}

func writeChaseTargetError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	slog.Error("streamer: looking up chase target", "err", err)
	http.Error(w, "chase stream unavailable", http.StatusInternalServerError)
}

// followChaseRecord は先頭からの追っかけの入力を返す。mirakc の追従配信は無入力
// タイムアウトで録画中にも閉じうる（実際に閉じる頻度は未検証）。そのまま ffmpeg の EOF に
// すると、録画が続いているのに playlist に ENDLIST が付く。閉じた後（正常に閉じても、
// 途中で切れても）は読んだバイトの続きから chaseRangeFollowReader で追い、mirakc が録画の
// 終了を返すまで EOF にしない（TestFollowChaseRecordContinuesWithRangeAfterFollowCloses /
// TestChaseRangeFollowReaderResumesAfterUncleanBodyClose）。
func followChaseRecord(ctx context.Context, client mirakcSeekRecordClient, recordID string, committedSize chaseCommittedSize) (io.ReadCloser, error) {
	body, err := waitForChaseRecord(ctx, client, recordID)
	if err != nil {
		return nil, err
	}
	return newChaseRangeFollowReader(ctx, client, recordID, 0, body, committedSize), nil
}

func waitForChaseRecord(ctx context.Context, client mirakcRecordClient, recordID string) (io.ReadCloser, error) {
	deadline := time.Now().Add(playlistStartupTimeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, errChaseRecordNotReadyTimeout
		}
		body, err := streamRecordFollowWithin(ctx, client, recordID, remaining)
		if err == nil {
			return body, nil
		}
		if errors.Is(err, errChaseRecordNotReadyTimeout) {
			return nil, err
		}
		if !errors.Is(err, mirakc.ErrRecordNotReady) {
			return nil, err
		}

		// 204 means “the recording file is still empty”, not a broken connection.
		// Retry until the same 15s startup budget used by playlist readiness expires;
		// this keeps 204 retries out of connection-failure accounting while still
		// guaranteeing that a request eventually returns 503.
		remaining = time.Until(deadline)
		if remaining <= 0 {
			return nil, errChaseRecordNotReadyTimeout
		}
		wait := playlistPollInterval
		if remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

const mpegTSPacketSize = 188

// chaseStartByteOffset maps the user-facing recording-file-relative second to a
// byte position in the current mirakc content snapshot. The file starts at the
// later of the tuner-open time and Program.StartAt: filter-program waits for
// the scheduled programme when mirakc opens the tuner early. MPEG-TS packet
// alignment avoids asking ffmpeg to begin halfway through a packet. The map is
// intentionally approximate: bitrate changes and encoder buffering mean that
// PTS is the final authority, so the UI exposes normal HLS seeking after the
// initial start.
func chaseStartByteOffset(record *mirakc.Record, offsetSeconds int64) (int64, error) {
	if offsetSeconds == 0 {
		return 0, nil
	}
	if record == nil || record.Content.Length == nil || *record.Content.Length == 0 {
		return 0, mirakc.ErrRecordNotReady
	}

	recordingStart := record.Recording.StartTime.Time()
	fileStart := recordingStart
	if record.Program.StartAt != nil && record.Program.StartAt.Time().After(fileStart) {
		fileStart = record.Program.StartAt.Time()
	}
	available := time.Since(fileStart)
	if record.Recording.Status != "recording" {
		switch {
		case record.Recording.Duration != nil:
			available = time.Duration(*record.Recording.Duration)*time.Millisecond - fileStart.Sub(recordingStart)
		case record.Recording.EndTime != nil:
			available = record.Recording.EndTime.Time().Sub(fileStart)
		}
	}
	availableSeconds := int64(available / time.Second)
	if availableSeconds <= offsetSeconds || availableSeconds <= 0 {
		return 0, errChaseOffsetUnavailable
	}

	length := *record.Content.Length
	if length < mpegTSPacketSize {
		return 0, mirakc.ErrRecordNotReady
	}
	// This form avoids overflowing length*offset for long recordings while
	// retaining integer arithmetic for the byte position.
	denominator := uint64(availableSeconds)
	requested := uint64(offsetSeconds)
	byteOffset := (length/denominator)*requested + (length%denominator)*requested/denominator
	byteOffset -= byteOffset % mpegTSPacketSize
	if byteOffset >= length {
		byteOffset = length - mpegTSPacketSize
		byteOffset -= byteOffset % mpegTSPacketSize
	}
	return int64(byteOffset), nil
}

// waitForChaseRecordAtOffset gets the first finite Range response within the
// normal startup budget. Once the first bytes are available, the returned
// reader follows the recording by requesting the next Range after each finite
// response reaches EOF.
func waitForChaseRecordAtOffset(ctx context.Context, client mirakcSeekRecordClient, recordID string, startByte int64, committedSize chaseCommittedSize) (io.ReadCloser, error) {
	deadline := time.Now().Add(playlistStartupTimeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, errChaseRecordNotReadyTimeout
		}
		body, length, err := streamRecordRangeWithin(ctx, client, recordID, startByte, remaining)
		// length は 206 の Content-Length。分からない（-1）本文も読み、空かどうかは
		// 読んだ結果で reader が判断する。
		if err == nil && body != nil && length != 0 {
			return newChaseRangeFollowReader(ctx, client, recordID, startByte, body, committedSize), nil
		}
		if body != nil {
			_ = body.Close()
		}
		if errors.Is(err, errChaseRecordNotReadyTimeout) {
			return nil, err
		}
		if err != nil && !errors.Is(err, mirakc.ErrRecordNotReady) && !errors.Is(err, mirakc.ErrRangeNotSatisfiable) {
			return nil, err
		}
		if err := waitForChaseRecordPoll(ctx, deadline); err != nil {
			return nil, err
		}
	}
}

func waitForChaseRecordPoll(ctx context.Context, deadline time.Time) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return errChaseRecordNotReadyTimeout
	}
	wait := playlistPollInterval
	if remaining < wait {
		wait = remaining
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

// errChaseRecordPurged は、追っかけが録画ファイルの終端まで読む前に mirakc の record が
// 消えた（404）ことを表す。
var errChaseRecordPurged = errors.New("mirakc record is gone before the chase read it to the end")

// chaseCommittedSize は録画のコミット済み原本のバイト数を返す（無ければ ok=false）。
type chaseCommittedSize func(ctx context.Context) (size int64, ok bool, err error)

// chaseRangeFollowReader turns mirakc's finite Range responses into one
// long-lived reader. It never reads and discards the recording head: every
// request begins at the byte position already consumed by ffmpeg.
//
// EOF を返すのは、次のどちらかで録画ファイルの終端まで渡し終えたときだけである。
//   - GetRecord が録画の終了（recording 以外）を返し、同じ offset への最後の Range も空だった
//     （状態遷移と最後の追記の競合を吸収する）
//   - record が 404 になり、かつコミット済み原本のバイト数が読んだ位置と一致した。ingest は
//     mirakc が録画の終了を返し、最後の Range が空になるまで読んでからコミットするので
//     （internal/worker/ingest.go の transferIngestRecord）、コミットされたバイト数は終了時点の
//     ファイル長である。一致すれば終端まで読んでいる
//
// 404 で原本と一致しない（ffmpeg が先端より遅れていて purge が先に来た、別経路の原本、
// mirakc が record を失った等）ならエラーを返す。終端が分からないまま EOF にすると、
// 欠けた終端に ENDLIST が付く。一過性の失敗は mirakc.MaxConsecutiveRetries 回まで再試行する。
//
// Close は別の goroutine から呼んでよい。進行中の Read と待ちを打ち切り、以後の要求を止める。
type chaseRangeFollowReader struct {
	ctx           context.Context
	cancel        context.CancelFunc
	client        mirakcSeekRecordClient
	recordID      string
	committedSize chaseCommittedSize

	mu     sync.Mutex
	body   io.ReadCloser // mu で守る
	closed bool          // mu で守る

	// 以下は Read を呼ぶ goroutine だけが触る。
	nextOffset int64
	// bodyBytes は今の本文から読んだバイト数。Content-Length の無い（-1）本文が何も返さずに
	// 終わったら、追い付いた（空の応答）として扱う。
	bodyBytes     int64
	finished      bool
	done          bool
	err           error
	nextRequestAt time.Time
	idleWait      time.Duration
	failures      int
}

// newChaseRangeFollowReader は offset から追う reader を作る。body は offset から始まる
// 読みかけの本文（追従配信か最初の Range 応答）で、nil なら最初の Read で Range を要求する。
// committedSize が nil なら、404 は常にエラーになる。
func newChaseRangeFollowReader(ctx context.Context, client mirakcSeekRecordClient, recordID string, offset int64, body io.ReadCloser, committedSize chaseCommittedSize) *chaseRangeFollowReader {
	ctx, cancel := context.WithCancel(ctx)
	return &chaseRangeFollowReader{
		ctx:           ctx,
		cancel:        cancel,
		client:        client,
		recordID:      recordID,
		committedSize: committedSize,
		body:          body,
		nextOffset:    offset,
	}
}

func (r *chaseRangeFollowReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		body, closed := r.current()
		switch {
		case closed:
			return 0, io.ErrClosedPipe
		case r.err != nil:
			return 0, r.err
		case r.done:
			return 0, io.EOF
		case body == nil:
			if err := r.requestNext(); err != nil {
				r.err = err
			}
			continue
		}

		n, err := body.Read(p)
		r.nextOffset += int64(n)
		r.bodyBytes += int64(n)
		if n > 0 {
			r.failures = 0
			r.idleWait = 0
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			empty := r.bodyBytes == 0
			r.closeBody(body)
			if empty {
				if caughtErr := r.caughtUp(); caughtErr != nil {
					r.err = caughtErr
				}
			}
		default:
			// 追従配信や Range の本文が途中で切れた（ErrUnexpectedEOF・接続リセット等）。
			// 読んだ位置は分かっているので、同じ続きを Range で取り直す。
			r.closeBody(body)
			if failErr := r.failure(fmt.Errorf("reading chase record body at offset %d: %w", r.nextOffset, err)); failErr != nil {
				r.err = failErr
			}
		}
		if n > 0 {
			return n, nil
		}
	}
}

// requestNext は nextOffset からの Range を 1 回要求する。本文があれば r.body に置き、
// 空なら caughtUp、失敗なら failure / gone に回す。
func (r *chaseRangeFollowReader) requestNext() error {
	if err := sleepUntil(r.ctx, r.nextRequestAt); err != nil {
		return err
	}
	r.nextRequestAt = time.Now().Add(chaseRangePollMin)
	body, length, err := r.client.StreamRecord(r.ctx, r.recordID, r.nextOffset)
	if err == nil && body != nil && length != 0 {
		r.bodyBytes = 0
		return r.setBody(body)
	}
	if body != nil {
		_ = body.Close()
	}
	switch {
	case err == nil, errors.Is(err, mirakc.ErrRecordNotReady), errors.Is(err, mirakc.ErrRangeNotSatisfiable):
		r.failures = 0
		return r.caughtUp()
	case chaseRecordNotFound(err):
		return r.gone(err)
	default:
		return r.failure(fmt.Errorf("requesting chase record range at offset %d: %w", r.nextOffset, err))
	}
}

// caughtUp は空の応答の後に呼ぶ。終了済みなら EOF、まだ録画中ならバックオフして待つ。
func (r *chaseRangeFollowReader) caughtUp() error {
	if r.finished {
		r.done = true
		return nil
	}
	finished, err := chaseRecordFinished(r.ctx, r.client, r.recordID)
	if err != nil {
		if chaseRecordNotFound(err) {
			return r.gone(err)
		}
		return r.failure(fmt.Errorf("checking chase record status: %w", err))
	}
	r.failures = 0
	if finished {
		// 状態遷移と最後の追記の競合を吸収するため、同じ offset をすぐにもう 1 回読む。
		r.finished = true
		r.nextRequestAt = time.Time{}
		return nil
	}
	if r.idleWait == 0 {
		r.idleWait = chaseRangePollMin
	} else {
		r.idleWait = min(r.idleWait*2, chaseRangePollMax)
	}
	r.nextRequestAt = time.Now().Add(r.idleWait)
	return nil
}

// gone は record が 404 になったときに呼ぶ。読んだ位置がコミット済み原本の終端なら EOF、
// それ以外はエラーにする（chaseRangeFollowReader の doc を参照）。
func (r *chaseRangeFollowReader) gone(cause error) error {
	if r.committedSize != nil {
		size, ok, err := r.committedSize(r.ctx)
		if err != nil {
			return fmt.Errorf("%w at offset %d (reading the committed original: %w): %w", errChaseRecordPurged, r.nextOffset, err, cause)
		}
		if ok && size == r.nextOffset {
			r.done = true
			return nil
		}
		if ok {
			return fmt.Errorf("%w at offset %d of %d committed bytes: %w", errChaseRecordPurged, r.nextOffset, size, cause)
		}
	}
	return fmt.Errorf("%w at offset %d (no committed original): %w", errChaseRecordPurged, r.nextOffset, cause)
}

// failure は一過性の失敗なら上限つきでバックオフを予約し、そうでなければエラーを返す。
func (r *chaseRangeFollowReader) failure(err error) error {
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if !mirakc.IsRetryable(err) {
		return err
	}
	r.failures++
	if r.failures > mirakc.MaxConsecutiveRetries {
		return fmt.Errorf("chase record %s failed %d consecutive times: %w", r.recordID, r.failures, err)
	}
	delay := chaseRetryDelay(r.failures - 1)
	slog.Warn("streamer: transient chase record failure, retrying",
		"record_id", r.recordID, "offset", r.nextOffset, "consecutive_failures", r.failures, "delay", delay, "err", err)
	r.nextRequestAt = time.Now().Add(delay)
	return nil
}

func (r *chaseRangeFollowReader) current() (io.ReadCloser, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body, r.closed
}

func (r *chaseRangeFollowReader) setBody(body io.ReadCloser) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = body.Close()
		return io.ErrClosedPipe
	}
	r.body = body
	r.mu.Unlock()
	return nil
}

// closeBody は body がまだ今の本文なら外して閉じる（Close が先に外していたら何もしない）。
func (r *chaseRangeFollowReader) closeBody(body io.ReadCloser) {
	r.mu.Lock()
	if r.body != body {
		r.mu.Unlock()
		return
	}
	r.body = nil
	r.mu.Unlock()
	_ = body.Close()
}

// Close は進行中の Read と待ちを打ち切り、以後の要求を止める（TestChaseRangeFollowReaderCloseStopsConcurrentRead）。
func (r *chaseRangeFollowReader) Close() error {
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
	return body.Close()
}

// chaseRecordNotFound は mirakc の record が無い（404）か。意味は chaseRangeFollowReader を参照。
func chaseRecordNotFound(err error) bool {
	var apiErr *mirakc.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// sleepUntil は at まで待つ（過去なら待たない）。
func sleepUntil(ctx context.Context, at time.Time) error {
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

func chaseRecordFinished(ctx context.Context, client mirakcSeekRecordClient, recordID string) (bool, error) {
	record, err := client.GetRecord(ctx, recordID)
	if err != nil {
		return false, err
	}
	return record.Recording.Status != "recording", nil
}

type rangedRecordResult struct {
	body   io.ReadCloser
	length int64
	err    error
}

// streamRecordRangeWithin bounds only the response-header wait. The returned
// body remains valid beyond timeout because its request context is cancelled
// only when the body is closed or the session ends.
func streamRecordRangeWithin(ctx context.Context, client mirakcSeekRecordClient, recordID string, offset int64, timeout time.Duration) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	attemptCtx, cancel := context.WithCancel(ctx)
	resultCh := make(chan rangedRecordResult)
	go func() {
		body, length, err := client.StreamRecord(attemptCtx, recordID, offset)
		select {
		case resultCh <- rangedRecordResult{body: body, length: length, err: err}:
		case <-attemptCtx.Done():
			if body != nil {
				_ = body.Close()
			}
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-resultCh:
		if result.err != nil {
			cancel()
			return nil, 0, result.err
		}
		if result.body == nil {
			cancel()
			return nil, 0, mirakc.ErrRecordNotReady
		}
		return &cancelOnCloseReadCloser{ReadCloser: result.body, cancel: cancel}, result.length, nil
	case <-timer.C:
		cancel()
		return nil, 0, errChaseRecordNotReadyTimeout
	case <-ctx.Done():
		cancel()
		return nil, 0, ctx.Err()
	}
}

// streamRecordFollowWithin bounds the response-header wait without imposing the
// same deadline on the returned long-lived body. A context deadline passed
// directly to StreamRecordFollow would also cancel a successful body after the
// chase startup budget, which would truncate the ffmpeg input. On timeout we
// cancel the request context; on success the request context remains attached to
// the body and is released with the session context.
func streamRecordFollowWithin(ctx context.Context, client mirakcRecordClient, recordID string, timeout time.Duration) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	attemptCtx, cancel := context.WithCancel(ctx)
	type result struct {
		body io.ReadCloser
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		body, err := client.StreamRecordFollow(attemptCtx, recordID)
		resultCh <- result{body: body, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-resultCh:
		if result.err != nil {
			cancel()
			return nil, result.err
		}
		return &cancelOnCloseReadCloser{ReadCloser: result.body, cancel: cancel}, nil
	case <-timer.C:
		cancel()
		return nil, errChaseRecordNotReadyTimeout
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

type cancelOnCloseReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *cancelOnCloseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.cancel()
	return err
}

const chaseInputCooldownMessage = "追っかけ入力のエラーが続いているため、再作成を一時停止しています。しばらく待ってから再読み込みしてください。"

type chaseInputCooldownError struct {
	retryAfter time.Duration
}

func (e *chaseInputCooldownError) Error() string { return errChaseInputCoolingDown.Error() }

func (e *chaseInputCooldownError) Unwrap() error { return errChaseInputCoolingDown }

// chaseSessionDir gives every recording-relative start position its own leaf
// directory. In particular, offset 0 must not use the recording directory as
// the parent of other offsets: cleanup of one session is allowed to remove only
// that session's HLS files.
func chaseSessionDir(segmentDir, site string, recordingID, offsetSeconds int64) string {
	return filepath.Join(
		segmentDir,
		site,
		"chase",
		strconv.FormatInt(recordingID, 10),
		"offset",
		strconv.FormatInt(offsetSeconds, 10),
	)
}

// existingChaseSessionOrCooldown は、mirakc のメタデータを引く前に既存 offset のセッションへ
// 相乗りさせ、録画単位の入力失敗 cooldown 中は新規作成を拒む。この事前確認の後に別セッションが
// 失敗する競合は、getOrCreateSessionOnceFor 内の最終確認が塞ぐ。
func (ls *LiveStreamer) existingChaseSessionOrCooldown(ctx context.Context, key sessionKey) (*liveSession, error) {
	ls.mu.Lock()
	s, exists := ls.getSessionLocked(key)
	if !exists && key.kind == chaseSessionKind {
		if err := ls.chaseInputCooldownLocked(key.id); err != nil {
			ls.mu.Unlock()
			return nil, err
		}
	}
	ls.mu.Unlock()
	if !exists {
		return nil, nil
	}
	if err := waitReadyTouching(ctx, s, playlistStartupTimeout); err != nil {
		return nil, err
	}
	if s.startErr != nil {
		return s, s.startErr
	}
	return s, nil
}

// chaseInputCooldownLocked は録画単位の cooldown エラーを返す。呼び出し側は ls.mu を保持すること。
// 失敗の記録・map からの削除・作成の可否判定を 1 つのロック区間で行うためである。
func (ls *LiveStreamer) chaseInputCooldownLocked(recordingID int64) error {
	if retryAt, failed := ls.failedChaseInputs[recordingID]; failed {
		remaining := time.Until(retryAt)
		if remaining > 0 {
			return &chaseInputCooldownError{retryAfter: remaining}
		}
		delete(ls.failedChaseInputs, recordingID)
	}
	return nil
}

// recordFailedChaseInputLocked は ls.mu を保持した状態で追っかけ入力の cooldown を記録する。
func (ls *LiveStreamer) recordFailedChaseInputLocked(recordingID int64) {
	if ls.failedChaseInputs == nil {
		ls.failedChaseInputs = make(map[int64]time.Time)
	}
	ls.failedChaseInputs[recordingID] = time.Now().Add(chaseInputFailureCooldown)
}

// committedOriginalSize は録画のコミット済み原本（active）のバイト数を返す関数を作る。
// DB が無い構成（テスト）では nil で、404 は常にエラーになる。
func (ls *LiveStreamer) committedOriginalSize(recordingID int64) chaseCommittedSize {
	if ls.pool == nil {
		return nil
	}
	return func(ctx context.Context) (int64, bool, error) {
		row, err := sqlcgen.New(ls.pool).GetActiveOriginalMediaAsset(ctx, recordingID)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		return row.SizeBytes, true, nil
	}
}

// chaseInputCopy は追っかけの ffmpeg の stdin のパイプと、そこへの写し（copyChaseInput）を持つ。
// 入力のエラーで stdin を閉じる前に ffmpeg を kill するためで、os/exec の Stdin（io.Reader）に
// 任せると入力のエラーでもパイプが閉じられ、StdinPipe では Wait も書き込み側を閉じる。
// nil のメソッドは何もしない（追っかけ以外のセッション）。
type chaseInputCopy struct {
	read, write *os.File
	done        chan struct{}
	err         error
	// finishing は finish が入力と stdin を閉じ始めたこと。その後の入力のエラーは finish が
	// 起こしたもので、入力の失敗ではない（ffmpeg が自分で終わった）。
	finishing atomic.Bool
}

// attachChaseInput は cmd の stdin にパイプの読み側を付ける。Start の前に呼ぶ。
func attachChaseInput(cmd *exec.Cmd) (*chaseInputCopy, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("opening chase ffmpeg stdin: %w", err)
	}
	cmd.Stdin = pr
	return &chaseInputCopy{read: pr, write: pw}, nil
}

// started は Start の直後に呼ぶ。読み側は子に渡したので親の分を閉じる（持ち続けると、
// 書き側を閉じても子に EOF が届かない）。Start が失敗したら書き側も閉じる。
func (c *chaseInputCopy) started(startErr error) {
	if c == nil {
		return
	}
	_ = c.read.Close()
	if startErr != nil {
		_ = c.write.Close()
	}
}

// copy は写しを goroutine で始める。
func (c *chaseInputCopy) copy(input io.Reader, kill func() error) {
	if c == nil {
		return
	}
	c.done = make(chan struct{})
	go func() {
		defer close(c.done)
		c.err = copyChaseInput(c.write, input, kill, &c.finishing)
	}()
}

// finish は Wait の後に呼び、写しが入力のエラーで ffmpeg を kill したならそのエラーを返す
// （それ以外は nil）。ffmpeg が自分で終わったとき、写しは入力の Read か stdin への Write で
// 待っているかもしれない。入力と stdin の書き側を閉じて抜けさせてから待つ。stdin を閉じるのは、
// ffmpeg の孫が読み側を握ったまま読まないと、パイプが埋まった Write が終わらないためである
// （TestChaseInputCopyFinishUnblocksStuckWrite）。
func (c *chaseInputCopy) finish(input io.Closer) error {
	if c == nil || c.done == nil {
		return nil
	}
	c.finishing.Store(true)
	_ = input.Close()
	_ = c.write.Close()
	<-c.done
	return c.err
}

// copyChaseInput は追っかけの入力を ffmpeg の stdin へ写す。ffmpeg は stdin の EOF で
// ENDLIST を書くので、stdin を閉じるのは入力が正常な EOF で終わったときだけにする。入力が
// エラーで終わったら、先に kill してから stdin を閉じる（kill の後の ffmpeg は何も実行しない）。
// そうしないと途中までの入力に ENDLIST が付く（TestChaseInputErrorDoesNotWriteEndlist）。
// finishing が立った後の入力のエラーは finish が閉じたためなので kill せず nil を返す
// （TestChaseFFmpegCrashIsNotReportedAsInputFailure）。戻り値は kill した入力のエラー。
func copyChaseInput(stdin io.WriteCloser, input io.Reader, kill func() error, finishing *atomic.Bool) error {
	buf := make([]byte, 64<<10)
	for {
		n, readErr := input.Read(buf)
		if n > 0 {
			if _, writeErr := stdin.Write(buf[:n]); writeErr != nil {
				_ = stdin.Close()
				//nolint:nilerr // ffmpeg が先に終わったか finish が閉じた。終わり方は Wait が扱う。
				return nil
			}
		}
		switch {
		case readErr == nil:
		case errors.Is(readErr, io.EOF):
			_ = stdin.Close()
			return nil
		case finishing.Load():
			_ = stdin.Close()
			return nil
		default:
			_ = kill()
			_ = stdin.Close()
			return readErr
		}
	}
}
