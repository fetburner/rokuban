package ffargs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 実 ffprobe 9.0.2 の MPEG-TS 出力（programs 側と streams 側の 2 回出る。MPEG-2 は
// side_data_list に空オブジェクトが付く）。旧 csv 形式の "1440x1080x\n\n1440x1080x" は
// json では出ないので、ここには残さない。
const (
	mpeg2Out = `{"programs":[{"streams":[{"width":1440,"height":1080,"sample_aspect_ratio":"4:3","side_data_list":[{}]}]}],"stream_groups":[],"streams":[{"width":1440,"height":1080,"sample_aspect_ratio":"4:3","side_data_list":[{}]}]}`
	h264Out  = `{"programs":[{"streams":[{"width":1920,"height":1080}]}],"stream_groups":[],"streams":[{"width":1920,"height":1080}]}`
)

func TestParseVideoGeometry(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		w, h     int
		wantErr  bool
	}{
		{"mpeg2 ts", mpeg2Out, 1440, 1080, false},
		{"h264 ts", h264Out, 1920, 1080, false},
		{"no streams", `{"streams":[]}`, 0, 0, true},
		{"zero size", `{"streams":[{"width":0,"height":0}]}`, 0, 0, true},
		{"csv form", "1440x1080x\n\n1440x1080x", 0, 0, true},
	} {
		w, h, err := ParseVideoGeometry([]byte(tc.in))
		if (err != nil) != tc.wantErr || w != tc.w || h != tc.h {
			t.Errorf("%s: got %dx%d err=%v, want %dx%d wantErr=%v", tc.name, w, h, err, tc.w, tc.h, tc.wantErr)
		}
	}
}

func TestVideoGeometryProbeArgsIncludesSAR(t *testing.T) {
	args := VideoGeometryProbeArgs("input.ts")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "stream=width,height,sample_aspect_ratio") {
		t.Fatalf("args = %q, want sample_aspect_ratio", joined)
	}
}

func TestParseVideoGeometryWithSAR(t *testing.T) {
	for _, tc := range []struct {
		name, in, wantSAR string
	}{
		{"anamorphic", mpeg2Out, "4:3"},
		{"missing", h264Out, "1:1"},
		{"zero numerator", strings.ReplaceAll(mpeg2Out, `"4:3"`, `"0:1"`), "1:1"},
		{"not available", strings.ReplaceAll(mpeg2Out, `"4:3"`, `"N/A"`), "1:1"},
		{"malformed", strings.ReplaceAll(mpeg2Out, `"4:3"`, `"4/3"`), "1:1"},
	} {
		geometry, err := ParseVideoGeometryWithSAR([]byte(tc.in))
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}
		if geometry.SampleAspectRatio != tc.wantSAR {
			t.Errorf("%s: SAR = %q, want %q", tc.name, geometry.SampleAspectRatio, tc.wantSAR)
		}
		if geometry.Width <= 0 || geometry.Height <= 0 {
			t.Errorf("%s: geometry = %#v, want positive coded size", tc.name, geometry)
		}
	}
}

// 実物の ffprobe / ffmpeg で MPEG-2 1440x1080 SAR 4:3 と H.264 1920x1080 の TS を作り、
// 引数と出力がそのまま読めることを確かめる（SAR を掛けない大きさが返る）。
func TestVideoGeometryAgainstRealFFprobe(t *testing.T) {
	ffmpeg, err1 := exec.LookPath("ffmpeg")
	ffprobe, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		if os.Getenv("ROKUBAN_REQUIRE_FFMPEG") != "" {
			t.Fatalf("ffmpeg/ffprobe not in PATH but ROKUBAN_REQUIRE_FFMPEG is set: %v %v", err1, err2)
		}
		t.Skip("ffmpeg/ffprobe not in PATH")
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		gen  []string
		w, h int
	}{
		{"mpeg2.ts", []string{"-f", "lavfi", "-i", "testsrc=size=1440x1080:rate=25", "-t", "1", "-vf", "setsar=4/3", "-c:v", "mpeg2video"}, 1440, 1080},
		{"h264.ts", []string{"-f", "lavfi", "-i", "testsrc=size=1920x1080:rate=25", "-t", "1", "-c:v", "libx264"}, 1920, 1080},
	} {
		path := filepath.Join(dir, tc.name)
		args := append(append([]string{"-v", "error", "-y"}, tc.gen...), "-f", "mpegts", path)
		if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: ffmpeg: %v: %s", tc.name, err, out)
		}
		out, err := exec.Command(ffprobe, VideoGeometryProbeArgs(path)...).Output()
		if err != nil {
			t.Fatalf("%s: ffprobe: %v", tc.name, err)
		}
		w, h, err := ParseVideoGeometry(out)
		if err != nil || w != tc.w || h != tc.h {
			t.Errorf("%s: got %dx%d err=%v from %q, want %dx%d", tc.name, w, h, err, out, tc.w, tc.h)
		}
	}
}
