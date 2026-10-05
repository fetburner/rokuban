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

// frameTimeout は 1 コマの切り出しの上限（ffprobe 2 回 + ffmpeg 1 回）。
// ユーザーが帯を押したときに待つ時間なので、短く切って接続ごと諦めさせる。
const frameTimeout = 30 * time.Second

// RecordingFrame は GET /api/media/recordings/{id}/frame?at=<ms> を処理する。
//
// **原本からだけ切り出す。** encoded は縮小済みなので、CM の枠を教えるための
// 座標が合わない（原本が消えた録画にはコマが無い。クライアントは
// `frameRecordingId` が無いことで「原本のある録画がありません」と出す）。
// ごみ箱の録画・原本の無い録画・実体の無い原本は 404（配信の他の経路と同じ契約）。
//
// **応答は返す JPEG 自身の大きさ（X-Coded-Width / X-Coded-Height）と、at のコマの SAR を
// ヘッダで返す。** 映像は縮小も SAR の焼き込みもせずに出すので、返るコマは記録上の画素そのもの。
// 大きさは JPEG の SOF から、SAR は at の位置の ffprobe から読む（途中で解像度や SAR が
// 変わる録画でも、先頭のストリームの値にならない）。
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
	frame, err := s.extractFrame(ctx, path, atMs)
	if err != nil {
		slog.Error("streamer: extracting a frame", "recording_id", id, "at_ms", atMs, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	width, height, err := jpegSize(frame)
	if err != nil {
		slog.Error("streamer: reading the size of an extracted frame", "recording_id", id, "at_ms", atMs, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	probed, err := s.probeFrame(ctx, path, atMs, width, height)
	if err != nil {
		// ffmpeg と ffprobe が別のコマを見ているときも、SAR を信用できないので返さない。
		slog.Error("streamer: probing the SAR of a frame", "recording_id", id, "at_ms", atMs, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sar := probed.sar

	w.Header().Set("Content-Type", thumbnailContentType)
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("X-Coded-Width", strconv.Itoa(width))
	w.Header().Set("X-Coded-Height", strconv.Itoa(height))
	w.Header().Set("X-Sample-Aspect-Ratio", sar)
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

// jpegSize は JPEG の SOF から幅と高さを返す。返す画像そのものの寸法なので、
// at のコマと食い違う窓が無い（途中で解像度が変わる録画でも）。
func jpegSize(b []byte) (width, height int, err error) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 0, 0, errors.New("frame is not a JPEG")
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return 0, 0, errors.New("malformed JPEG marker")
		}
		m := b[i+1]
		if m == 0xFF { // fill byte
			i++
			continue
		}
		n := int(b[i+2])<<8 | int(b[i+3])
		if n < 2 || len(b) < i+2+n {
			return 0, 0, errors.New("truncated JPEG segment")
		}
		if m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC {
			seg := b[i+4 : i+2+n]
			if len(seg) < 5 {
				return 0, 0, errors.New("truncated JPEG SOF")
			}
			height, width = int(seg[1])<<8|int(seg[2]), int(seg[3])<<8|int(seg[4])
			if width <= 0 || height <= 0 {
				return 0, 0, errors.New("JPEG has a non-positive size")
			}
			return width, height, nil
		}
		i += 2 + n
	}
	return 0, 0, errors.New("JPEG has no SOF")
}

// sarWindow は probeFrame が at の手前に遡って読む長さ（秒）。ffprobe の
// -read_intervals は次のキーフレームから先しか出さず、狭い窓だと 1 コマも返らない
// （合成 TS で実測）。放送の GOP より十分長く取る。
const sarWindow = 3.0

// probedFrame は ffprobe が at のコマについて返す大きさと SAR。
type probedFrame struct {
	width, height int
	sar           string
}

// probeFrame は原本の atMs のコマの大きさと SAR を ffprobe で返す。**ffmpeg が返す JPEG の
// JFIF からは SAR を読めない。** ffmpeg CLI は途中で SAR が変わっても、フレームではなく
// ストリーム先頭の SAR を出力へ渡す（合成 TS で実測。フレーム自体の SAR は
// ffprobe -show_frames が正しく返す）。
//
// ffmpeg の `-ss` は start_time を足した位置へ飛ぶので、ffprobe の絶対 pts へ揃える。
// start_time は 33bit wrap 直前に始まる TS では負になるため、窓の開始も 0 へ丸めない。
// 窓の中から JPEG と大きさの合うコマを選ぶ（pickFrame）。
func (s *Streamer) probeFrame(ctx context.Context, path string, atMs int64, width, height int) (probedFrame, error) {
	ffprobe := ffargs.FFprobePath(s.cfg.FFprobe)
	out, err := s.runCommand(ctx, ffprobe, "-v", "error",
		"-show_entries", "format=start_time", "-of", "default=noprint_wrappers=1:nokey=1", path)
	if err != nil {
		return probedFrame{}, err
	}
	start, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64) // "N/A" は 0
	target := start + float64(atMs)/1000
	out, err = s.runCommand(ctx, ffprobe, "-v", "error", "-select_streams", "v:0",
		"-read_intervals", fmt.Sprintf("%.3f%%+%.3f", target-sarWindow, sarWindow+0.5),
		"-show_entries", "frame=best_effort_timestamp_time,width,height,sample_aspect_ratio",
		"-of", "csv=p=0", path)
	if err != nil {
		return probedFrame{}, err
	}
	return pickFrame(string(out), target, width, height)
}

// pickFrame は ffprobe の csv（`pts,width,height,sar` の行）から、ffmpeg が返した JPEG
// （width x height）のコマの SAR を選ぶ。まず pts が target 以上の最初のコマ（無ければ最後）、
// その大きさが JPEG と違えば 1 つ前のコマを見る。**ffmpeg の `-ss` は target から 1 コマ
// 前後ずれたコマを返すことがある**（合成 TS で実測: ffprobe の pts が 3.36 / 3.40 のとき
// target 3.37 は 3.36、3.371 は 3.40 を返した。規則は特定できていない）。どちらも JPEG と
// 大きさが合わなければ、別のコマを見ているので error。**大きさが同じで SAR だけ違う
// 隣り合うコマは区別できない**（境目の 1 コマだけ。未検証）。
func pickFrame(csv string, target float64, width, height int) (probedFrame, error) {
	var frames []probedFrame
	pick := -1
	for _, line := range strings.Split(csv, "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) < 4 {
			continue
		}
		pts, err1 := strconv.ParseFloat(f[0], 64)
		w, err2 := strconv.Atoi(f[1])
		h, err3 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		frames = append(frames, probedFrame{width: w, height: h, sar: normalizeSAR(f[3])})
		if pick < 0 && pts >= target-1e-4 {
			pick = len(frames) - 1
		}
	}
	if len(frames) == 0 {
		return probedFrame{}, errors.New("ffprobe returned no frame near the position")
	}
	if pick < 0 {
		pick = len(frames) - 1
	}
	for _, i := range []int{pick, pick - 1} {
		if i >= 0 && frames[i].width == width && frames[i].height == height {
			return frames[i], nil
		}
	}
	return probedFrame{}, fmt.Errorf("ffprobe frames near the position (%dx%d at the pick) do not match the JPEG (%dx%d)",
		frames[pick].width, frames[pick].height, width, height)
}

// normalizeSAR は "N:M" を返す。不明・0 以下・`N/A` は "1:1"。
func normalizeSAR(v string) string {
	var num, den int
	if _, err := fmt.Sscanf(v, "%d:%d", &num, &den); err == nil && num > 0 && den > 0 {
		return fmt.Sprintf("%d:%d", num, den)
	}
	return "1:1"
}

func (s *Streamer) ffmpegPath() string {
	return ffargs.FFmpegPath(s.cfg.FFmpeg)
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
