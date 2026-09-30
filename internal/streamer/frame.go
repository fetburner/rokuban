package streamer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/mediapath"
)

// frameTimeout は 1 コマの切り出しの上限（ffprobe 1 回 + ffmpeg 1 回）。
// ユーザーが帯を押したときに待つ時間なので、短く切って接続ごと諦めさせる。
const frameTimeout = 30 * time.Second

// RecordingFrame は GET /api/media/recordings/{id}/frame?at=<ms> を処理する。
//
// **原本からだけ切り出す。** encoded は縮小済みなので、CM の枠を教えるための
// 座標が合わない（原本が消えた録画にはコマが無い。クライアントは
// `frameRecordingId` が無いことで「原本のある録画がありません」と出す）。
// ごみ箱の録画・原本の無い録画・実体の無い原本は 404（配信の他の経路と同じ契約）。
//
// **応答は記録上の大きさ（X-Coded-Width / X-Coded-Height）をヘッダで返す。**
// 映像は縮小も SAR の焼き込みもせずに出すので、返るコマは記録上の画素そのもの。
// poster やシークタイルは SAR を正方形画素へ焼き込んでいるため、枠の座標には
// 使えない（1440x1080 の地上波 HD で 4/3 倍ずれる）。
//
// openapi には載せない（バイナリ配信。/file と /thumbnail と同じ）。
func (s *Streamer) RecordingFrame(w http.ResponseWriter, r *http.Request) {
	id, err := parseRecordingID(r)
	if err != nil {
		http.Error(w, "invalid recording id", http.StatusBadRequest)
		return
	}
	atMs, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("at")), 10, 64)
	if err != nil || atMs < 0 {
		http.Error(w, "invalid at", http.StatusBadRequest)
		return
	}

	row, err := sqlcgen.New(s.pool).GetOriginalMediaAssetForServing(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		slog.Error("streamer: looking up the original for a frame", "recording_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	path, err := mediapath.Resolve(s.cfg.MediaDir, row.RelPath)
	if err != nil {
		slog.Error("streamer: rejecting rel_path outside the media directory",
			"recording_id", id, "rel_path", row.RelPath, "err", err)
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(path); err != nil {
		// 配信（serveAsset）と同じ扱い: 行があるのに実体が無いのは不整合。
		slog.Warn("streamer: media asset row exists but the file is missing",
			"recording_id", id, "rel_path", row.RelPath)
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), frameTimeout)
	defer cancel()
	geometry, err := s.probeVideoGeometry(ctx, path)
	if err != nil {
		slog.Error("streamer: probing the original size for a frame", "recording_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	frame, err := s.extractFrame(ctx, path, atMs)
	if err != nil {
		slog.Error("streamer: extracting a frame", "recording_id", id, "at_ms", atMs, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", thumbnailContentType)
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("X-Coded-Width", strconv.Itoa(geometry.width))
	w.Header().Set("X-Coded-Height", strconv.Itoa(geometry.height))
	w.Header().Set("Content-Length", strconv.Itoa(len(frame)))
	if _, err := w.Write(frame); err != nil {
		slog.Warn("streamer: writing a frame to the client", "recording_id", id, "err", err)
	}
}

// extractFrame は atMs の位置の 1 コマを JPEG で返す（縮小も SAR の焼き込みもしない）。
//
// 位置は入力シーク（`-ss` を `-i` の前）で指定する。読む量が位置に比例せず、
// 番組長に比例しない（シークタイルと同じ理由）。
func (s *Streamer) extractFrame(ctx context.Context, path string, atMs int64) ([]byte, error) {
	seconds := strconv.FormatFloat(float64(atMs)/1000, 'f', 3, 64)
	return s.runCommand(ctx, s.ffmpegPath(),
		"-hide_banner", "-nostdin",
		"-ss", seconds,
		"-i", path,
		"-frames:v", "1",
		"-f", "image2pipe", "-c:v", "mjpeg", "-q:v", "3",
		"pipe:1",
	)
}

// probeVideoGeometry は原本の映像ストリームの記録上の大きさを返す。
//
// **stream=width,height は SAR を掛けない**（SAR は sample_aspect_ratio 側）。
// 1440x1080 の地上波 HD でも width=1440 が返る。人が教える枠の座標系はこれで、
// internal/worker の probeVideoGeometry と同じ問い合わせである --- **片方だけ
// 変えると、教えた枠が検出側で「解像度が違う」と判定されて静かに使われなくなる。**
func (s *Streamer) probeVideoGeometry(ctx context.Context, path string) (videoGeometry, error) {
	ffprobe := s.cfg.FFprobe
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	out, err := s.runCommand(ctx, ffprobe, ffargs.VideoGeometryProbeArgs(path)...)
	if err != nil {
		return videoGeometry{}, err
	}
	width, height, err := ffargs.ParseVideoGeometry(out)
	if err != nil {
		return videoGeometry{}, err
	}
	return videoGeometry{width: width, height: height}, nil
}

// videoGeometry は映像ストリームの記録上の大きさ（SAR を掛ける前の画素数）。
type videoGeometry struct{ width, height int }

func (s *Streamer) ffmpegPath() string {
	if s.cfg.FFmpeg == "" {
		return "ffmpeg"
	}
	return s.cfg.FFmpeg
}

// runCommand は ffmpeg/ffprobe を stdout 付きで実行する。テストは runCmd を差し替える。
func (s *Streamer) runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	if s.runCmd != nil {
		return s.runCmd(ctx, name, args...)
	}
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		message := err.Error()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// ffmpeg は失敗の理由を stderr にしか書かない（末尾だけ残す）。
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if len(stderr) > 1024 {
				stderr = stderr[len(stderr)-1024:]
			}
			message = stderr
		}
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(name), err, message)
	}
	return out, nil
}
