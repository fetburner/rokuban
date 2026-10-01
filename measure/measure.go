package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/streamer"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./measure <synthetic.ts>")
		os.Exit(2)
	}
	input := os.Args[1]
	root, err := os.MkdirTemp("", "original-vod-offset-measure-")
	must(err)
	defer func() { must(os.RemoveAll(root)) }()
	cfg := streamer.LiveConfig{
		FFmpeg: "ffmpeg",
		Profiles: []streamer.LiveProfile{{
			Name: "hd", VideoCodec: "libx264", AudioCodec: "aac",
			SegmentSeconds: 2, PlaylistSize: 6,
		}},
	}

	for trial := 1; trial <= 3; trial++ {
		for _, offset := range []int64{0, 61, 307, 603, 659} {
			dir := filepath.Join(root, fmt.Sprintf("trial-%d", trial), fmt.Sprint(offset))
			if err := os.MkdirAll(filepath.Join(dir, "segments"), 0o755); err != nil {
				must(err)
			}
			f, err := os.Open(input)
			must(err)
			args := streamer.BuildOriginalVODFFmpegArgs(cfg, dir, false, offset)
			cmd := exec.Command("ffmpeg", args...)
			cmd.Stderr = os.Stderr
			cmd.ExtraFiles = []*os.File{f}
			started := time.Now()
			probeLatency := time.Duration(0)
			if offset > 0 {
				probeLatency = probeDuration(f)
			}
			must(cmd.Start())
			masterLatency := waitFor(filepath.Join(dir, "hd.m3u8"), "#EXT-X-STREAM-INF", started)
			variantLatency := waitForVariant(dir, started)
			err = cmd.Wait()
			_ = f.Close()
			if err != nil {
				fmt.Fprintf(os.Stderr, "ffmpeg offset %d failed: %v\n", offset, err)
				os.Exit(1)
			}
			segmentPath := filepath.Join(dir, "segments", "hd.0_seg00000.ts")
			seconds, frame := firstBurnedTimecode(segmentPath, offset)
			errorFrames := (seconds-offset)*fps + int64(frame)
			errorMS := errorFrames * 1000 / fps
			if errorMS < -500 || errorMS > 500 {
				must(fmt.Errorf("offset %d first frame error %dms exceeds 500ms", offset, errorMS))
			}
			fmt.Printf("trial=%d offset=%d first_frame=%05d:%02d error_ms=%d probe_ms=%d master_ms=%d first_segment_ms=%d\n",
				trial, offset, seconds, frame, errorMS, probeLatency.Milliseconds(),
				masterLatency.Milliseconds(), variantLatency.Milliseconds())
		}
	}
}

func probeDuration(file *os.File) time.Duration {
	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", "-i", "/dev/fd/3")
	cmd.ExtraFiles = []*os.File{file}
	started := time.Now()
	out, err := cmd.Output()
	must(err)
	if _, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err != nil {
		must(fmt.Errorf("invalid duration %q: %w", strings.TrimSpace(string(out)), err))
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		must(err)
	}
	return time.Since(started)
}

func firstBurnedTimecode(segmentPath string, offset int64) (int64, int) {
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", segmentPath,
		"-map", "0:v:0", "-frames:v", "1", "-fps_mode", "passthrough",
		"-vf", "format=gray", "-f", "rawvideo", "pipe:1")
	image, err := cmd.Output()
	must(err)
	if len(image) != width*height {
		must(fmt.Errorf("decoded frame has %d bytes, want %d", len(image), width*height))
	}

	bestSeconds, bestFrame, bestErrors := int64(-1), -1, width*height
	firstSecond := offset - 1
	if firstSecond < 0 {
		firstSecond = 0
	}
	lastSecond := offset + 2
	for second := firstSecond; second <= lastSecond; second++ {
		for frame := 0; frame < fps; frame++ {
			text := fmt.Sprintf("%05d:%02d", second, frame)
			if errors := pixelMismatches(image, text); errors < bestErrors {
				bestSeconds, bestFrame, bestErrors = second, frame, errors
			}
		}
	}
	if bestSeconds < 0 || bestErrors > 20 {
		must(fmt.Errorf("could not read burned timecode; best pixel mismatches = %d", bestErrors))
	}
	return bestSeconds, bestFrame
}

func pixelMismatches(image []byte, text string) int {
	mismatches := 0
	x := 8
	for _, char := range text {
		glyph, ok := measurementFont[byte(char)]
		if !ok {
			return width * height
		}
		for row, bits := range glyph {
			for col := 0; col < 5; col++ {
				wantBright := bits&(1<<(4-col)) != 0
				sampleX := x + col*scale + scale/2
				sampleY := 16 + row*scale + scale/2
				gotBright := image[sampleY*width+sampleX] >= 128
				if gotBright != wantBright {
					mismatches++
				}
			}
		}
		x += 38
	}
	return mismatches
}

const (
	width  = 320
	height = 80
	fps    = 25
	scale  = 6
)

var measurementFont = map[byte][7]byte{
	'0': {0b01110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
	'1': {0b00100, 0b01100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110},
	'2': {0b01110, 0b10001, 0b00001, 0b00010, 0b00100, 0b01000, 0b11111},
	'3': {0b11110, 0b00001, 0b00001, 0b01110, 0b00001, 0b00001, 0b11110},
	'4': {0b00010, 0b00110, 0b01010, 0b10010, 0b11111, 0b00010, 0b00010},
	'5': {0b11111, 0b10000, 0b10000, 0b11110, 0b00001, 0b00001, 0b11110},
	'6': {0b01110, 0b10000, 0b10000, 0b11110, 0b10001, 0b10001, 0b01110},
	'7': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b01000, 0b01000},
	'8': {0b01110, 0b10001, 0b10001, 0b01110, 0b10001, 0b10001, 0b01110},
	'9': {0b01110, 0b10001, 0b10001, 0b01111, 0b00001, 0b00001, 0b01110},
	':': {0, 0b00100, 0b00100, 0, 0b00100, 0b00100, 0},
}

func waitForVariant(dir string, started time.Time) time.Duration {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".m3u8") || entry.Name() == "hd.m3u8" {
					continue
				}
				body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
				if err == nil && strings.Contains(string(body), "#EXTINF") {
					return time.Since(started)
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "timed out waiting for first variant playlist in %s\n", dir)
	os.Exit(1)
	return 0
}

func waitFor(path, marker string, started time.Time) time.Duration {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(path); err == nil && strings.Contains(string(body), marker) {
			return time.Since(started)
		}
		time.Sleep(time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "timed out waiting for %s in %s\n", marker, path)
	os.Exit(1)
	return 0
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
