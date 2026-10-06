package streamer

// original_vod.go は原本 VOD の解決・再生ハンドラ・セッション同定を持つ。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
)

// lookupOriginalVODTarget returns an active original for a completed recording
// owned by this site. The recording id is the durable resource key; no session
// identifier is exposed in the URL or persisted in the database.
func (ls *LiveStreamer) lookupOriginalVODTarget(ctx context.Context, recordingID int64) (sqlcgen.GetOriginalVODTargetRow, error) {
	if ls.pool == nil {
		return sqlcgen.GetOriginalVODTargetRow{}, errors.New("original VOD database is unavailable")
	}
	return sqlcgen.New(ls.pool).GetOriginalVODTarget(ctx, sqlcgen.GetOriginalVODTargetParams{
		RecordingID: recordingID,
		Site:        ls.site,
	})
}

// originalVODSource opens the original read-only and only then rechecks the DB
// row (open-then-verify), without the rel_path lock.
//
// Why this holds the right inode without a lock: target was read as active, so its
// canonical file was renamed into place before its row committed, and the path
// keeps that inode until delete_reconcile unlinks it, which happens only after
// MarkMediaAssetDeleting has committed (deleteMediaAsset). media_assets_rel_path_idx
// forbids another live row at the same rel_path, so nobody else publishes there
// while target is live. If the row is still active after open, the path cannot have
// been unlinked before the open; if it is not, we return pgx.ErrNoRows. A later
// unlink cannot change the descriptor FFmpeg already holds.
func (ls *LiveStreamer) originalVODSource(recordingID, offsetSeconds int64, target sqlcgen.GetOriginalVODTargetRow) sessionSource {
	return func(ctx context.Context) (io.ReadCloser, error) {
		if ls.cfg.MediaDir == "" {
			return nil, errors.New("original VOD media directory is unavailable")
		}
		path, err := mediapath.Resolve(ls.cfg.MediaDir, target.RelPath)
		if err != nil {
			return nil, fmt.Errorf("resolving original media path: %w", err)
		}
		file, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, pgx.ErrNoRows
			}
			return nil, fmt.Errorf("opening original media: %w", err)
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = file.Close()
			if err != nil {
				return nil, fmt.Errorf("stating original media: %w", err)
			}
			return nil, pgx.ErrNoRows
		}
		if ls.afterOriginalVODOpen != nil {
			ls.afterOriginalVODOpen()
		}
		current, err := ls.lookupOriginalVODTarget(ctx, recordingID)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if current.ID != target.ID || current.RelPath != target.RelPath {
			_ = file.Close()
			return nil, pgx.ErrNoRows
		}
		if offsetSeconds > 0 {
			duration, err := probeOriginalVODDuration(ctx, ls.cfg.FFprobe, file)
			if err != nil {
				_ = file.Close()
				return nil, fmt.Errorf("probing original VOD duration: %w", err)
			}
			// 映像が残らない offset を通すと ffmpeg は何も出力せず、playlist 待ちの
			// 15 秒後に 504 になる（実バイナリで測定）。
			if float64(offsetSeconds) >= duration-originalVODTailMargin {
				_ = file.Close()
				return nil, errOriginalVODOffsetUnavailable
			}
		}
		return file, nil
	}
}

// originalVODFFmpegInputPath は ffmpeg / ffprobe が原本を開く名前。
//
// Cmd.ExtraFiles[0] が子の fd 3 になるので /dev/fd/3 でその記述子を開く。
// ffmpeg の `fd:` プロトコルを使わないのは、`-ss` の入力側シークに必要な
// 「ファイルとして開き直せる入力」を avformat に渡したいため（`fd:` は
// 記述子を直接読むので、seekable かどうかの判定が OS 任せになる。未検証）。
// /dev/fd/3 は記述子を保持する inode を指すので、DB 確認の後に canonical path が
// unlink されても読める（TestOriginalVODRetainedSessionSurvivesOriginalDeletion）。
const originalVODFFmpegInputPath = "/dev/fd/3"

// originalVODProbeTimeout は probeOriginalVODDuration（ffprobe 起動）の上限。
const originalVODProbeTimeout = 5 * time.Second

// originalVODTailMargin は映像の終端からこの秒数以内の offset を範囲外にする。
// 終端ちょうどの offset は最後のフレームより後ろを指して出力が空になりうる
// （合成 TS で offset 40 / 映像終端 40.01 が 15 秒待って 504 になった）。
const originalVODTailMargin = 0.5

// probeOriginalVODDuration は原本の「映像が存在する長さ」（録画先頭からの秒）を
// 開いた記述子越しに測る。offset がこの値以上なら映像が 1 フレームも残らない。
//
// format の duration は音声など最長のストリームで決まり、映像の終端より長い
// （合成 660 秒 TS: format 660.010 / 映像 660.000）。そのため映像ストリームの
// 終端（start_time + duration - format の start_time）を使う。ストリームの値が
// 取れなければ format の duration に落とす。
//
// ffprobe は継承した記述子の読み位置を共有して動かすので、返す前に先頭へ戻す
// （ffmpeg の入力側 -ss は先頭からの位置で測る。外すと偽 ffmpeg が fd 3 から 0 バイトしか読めず
// TestOriginalVODOffsetIdleGCRemovesScratch が落ちる）。
func probeOriginalVODDuration(ctx context.Context, ffprobe string, file *os.File) (float64, error) {
	ffprobe = ffargs.FFprobePath(ffprobe)
	probeCtx, cancel := context.WithTimeout(ctx, originalVODProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, ffprobe, ffargs.OriginalVODDurationProbeArgs(originalVODFFmpegInputPath)...)
	cmd.ExtraFiles = []*os.File{file}
	out, err := cmd.Output()
	if err != nil {
		if probeCtx.Err() != nil {
			return 0, probeCtx.Err()
		}
		return 0, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("rewinding probed original media: %w", err)
	}
	var probed struct {
		Format struct {
			StartTime string `json:"start_time"`
			Duration  string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			StartTime string `json:"start_time"`
			Duration  string `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probed); err != nil {
		return 0, fmt.Errorf("parsing original VOD probe output: %w", err)
	}
	valid := func(raw string) (float64, bool) {
		v, err := strconv.ParseFloat(raw, 64)
		return v, err == nil && !math.IsNaN(v) && !math.IsInf(v, 0)
	}
	duration, ok := valid(probed.Format.Duration)
	if len(probed.Streams) > 0 {
		vStart, ok1 := valid(probed.Streams[0].StartTime)
		vDur, ok2 := valid(probed.Streams[0].Duration)
		fStart, ok3 := valid(probed.Format.StartTime)
		if ok1 && ok2 && ok3 {
			duration, ok = vStart+vDur-fStart, true
		}
	}
	if !ok || duration <= 0 {
		return 0, fmt.Errorf("invalid original VOD duration in %q", strings.TrimSpace(string(out)))
	}
	return duration, nil
}

// originalVODRecordingRemoved reports whether a user action (trash, purge,
// supersede) has removed the recording. Segment requests use only this: the
// retained session reads a held descriptor and scratch files, so an asset that
// became deleted (for example until_encoded) must not stop a playback in
// progress. A DB error other than ErrNoRows keeps serving the files.
func (ls *LiveStreamer) originalVODRecordingRemoved(ctx context.Context, recordingID int64) bool {
	if ls.pool == nil {
		return false
	}
	rec, err := sqlcgen.New(ls.pool).GetRecordingByID(ctx, recordingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	if err != nil {
		slog.Warn("streamer: original VOD recording check failed; serving retained files",
			"recording_id", recordingID, "err", err)
		return false
	}
	return rec.DeletedAt != nil || rec.PurgedAt != nil || rec.SupersededAt != nil
}

// OriginalVODPlaylist starts or joins the single source-TS-to-HLS session for
// this recording. Every configured profile is emitted by the same ffmpeg; the
// profile query selects a playlist and is deliberately absent from the key.
func (ls *LiveStreamer) OriginalVODPlaylist(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "invalid original VOD offset", http.StatusBadRequest)
		return
	}
	profile, ok := ls.cfg.profile(r.URL.Query().Get("profile"))
	if !ok {
		http.Error(w, "unknown original VOD profile", http.StatusBadRequest)
		return
	}
	target, err := ls.lookupOriginalVODTarget(r.Context(), recordingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			ls.invalidateOriginalVODSession(recordingID)
		}
		writeOriginalVODError(w, r, err)
		return
	}
	key := originalVODSessionKeyFor(recordingID, offsetSeconds)
	ls.mu.Lock()
	s, exists := ls.getSessionLocked(key)
	ls.mu.Unlock()
	if !exists {
		source := ls.originalVODSource(recordingID, offsetSeconds, target)
		// Reject out-of-range requests before getOrCreateSessionFor can fail on a
		// full pool or evict an unrelated idle session. The descriptor opened here
		// is closed at once; the session reopens the file itself, so a file unlinked
		// in between yields the usual 404.
		if offsetSeconds > 0 {
			probe, err := source(r.Context())
			if err != nil {
				if errors.Is(err, errOriginalVODOffsetUnavailable) {
					slog.Info("streamer: original VOD offset outside recording range",
						"recording_id", recordingID, "offset", offsetSeconds)
				}
				writeOriginalVODError(w, r, err)
				return
			}
			_ = probe.Close()
		}
		s, err = ls.getOrCreateSessionFor(
			r.Context(), key, source,
		)
		if err != nil {
			writeOriginalVODError(w, r, err)
			return
		}
	} else if err := waitReadyTouching(r.Context(), s, playlistStartupTimeout); err != nil {
		if errors.Is(err, errStartupTimeout) {
			http.Error(w, "original VOD stream did not start in time", http.StatusGatewayTimeout)
		}
		return
	} else if s.startErr != nil {
		writeOriginalVODError(w, r, s.startErr)
		return
	}
	s.touch()

	playlistName := profile.Name + ".m3u8"
	if ls.cfg.Captions {
		playlistName = "playlist.m3u8"
	}
	content, ok := waitForPlaylist(
		r.Context(), s, filepath.Join(s.dir, playlistName), playlistStartupTimeout, "#EXT-X-STREAM-INF",
	)
	if !ok {
		slog.Error("streamer: original VOD playlist did not appear in time",
			"recording_id", recordingID, "profile", profile.Name, "dir", s.dir)
		http.Error(w, "original VOD stream did not start in time", http.StatusGatewayTimeout)
		return
	}
	writeHLSPlaylist(w, content)
}

// OriginalVODSegment serves the (possibly still growing) EVENT variant playlist, subtitle
// playlist, or segment from the retained original VOD session.
func (ls *LiveStreamer) OriginalVODSegment(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "invalid original VOD offset", http.StatusBadRequest)
		return
	}
	name := chi.URLParam(r, "name")
	if !ls.cfg.servesFile(name) {
		http.Error(w, "invalid segment name", http.StatusBadRequest)
		return
	}
	if ls.originalVODRecordingRemoved(r.Context(), recordingID) {
		ls.invalidateOriginalVODSession(recordingID)
		http.NotFound(w, r)
		return
	}
	key := originalVODSessionKeyFor(recordingID, offsetSeconds)
	ls.mu.Lock()
	s, ok := ls.getSessionLocked(key)
	ls.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := waitReadyTouching(r.Context(), s, playlistStartupTimeout); err != nil {
		if errors.Is(err, errStartupTimeout) {
			http.Error(w, "original VOD stream did not start in time", http.StatusGatewayTimeout)
		}
		return
	}
	if s.startErr != nil {
		writeOriginalVODError(w, r, s.startErr)
		return
	}
	s.touch()

	path := sessionFilePath(s.dir, name)
	if strings.HasSuffix(name, ".m3u8") {
		content, ok := waitForPlaylist(r.Context(), s, path, playlistStartupTimeout, "#EXTINF")
		if !ok {
			http.Error(w, "original VOD stream did not start in time", http.StatusGatewayTimeout)
			return
		}
		writeHLSPlaylist(w, content)
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

// invalidateOriginalVODSession stops every offset session and removes its HLS
// output after the recording stops being an eligible VOD target (for example,
// trash or purge). Completed FFmpeg sessions are otherwise retained until idle GC.
func (ls *LiveStreamer) invalidateOriginalVODSession(recordingID int64) {
	ls.mu.Lock()
	var sessions []*liveSession
	for key, s := range ls.chaseSessions {
		if key.kind == originalVODSessionKind && key.id == recordingID {
			delete(ls.chaseSessions, key)
			sessions = append(sessions, s)
		}
	}
	ls.mu.Unlock()
	if len(sessions) == 0 {
		return
	}
	for _, s := range sessions {
		s.stop()
		cleanupSessionDir(s)
	}
	ls.setActiveSessionMetrics()
}

// OriginalVODLeave shortens the shared session's idle deadline. It is a hint;
// viewers of the same recording keep the session alive through their requests.
func (ls *LiveStreamer) OriginalVODLeave(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "invalid original VOD offset", http.StatusBadRequest)
		return
	}
	key := originalVODSessionKeyFor(recordingID, offsetSeconds)
	ls.mu.Lock()
	s, ok := ls.getSessionLocked(key)
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
	slog.Info("streamer: original VOD leave hint received, shortening idle deadline",
		"recording_id", recordingID, "grace", ls.cfg.leaveGrace())
	w.WriteHeader(http.StatusNoContent)
}

func writeOriginalVODError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, errSessionLimit) {
		writeSessionError(w, err)
		return
	}
	if errors.Is(err, errOriginalVODOffsetUnavailable) {
		http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}
	slog.Error("streamer: starting original VOD session", "err", err)
	http.Error(w, "original VOD stream unavailable", http.StatusServiceUnavailable)
}

func originalVODSessionDir(segmentDir, site string, recordingID, offsetSeconds int64) string {
	return filepath.Join(
		segmentDir,
		site,
		"original-vod",
		strconv.FormatInt(recordingID, 10),
		"offset",
		strconv.FormatInt(offsetSeconds, 10),
	)
}

func originalVODSessionKeyFor(recordingID, offsetSeconds int64) sessionKey {
	return sessionKey{kind: originalVODSessionKind, id: recordingID, offsetSeconds: offsetSeconds}
}
