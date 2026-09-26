package streamer

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestBuildLiveFFmpegArgs_RealFFmpegAudioRenditions は BuildLiveFFmpegArgs の argv を
// **本物の ffmpeg** に流し、master に載った音声レンディションを 1 本ずつデコードして
// 左右のチャンネルの周波数を測る。引数の文字列比較では、`-filter:a:N` の index の
// ずれ（主の pan が標準に掛かる等）も、master の並びと UI の契約（0 = 標準 / 1 = 主 /
// 2 = 副）の食い違いも捕まらない。
//
// 入力は L = 440Hz / R = 880Hz のステレオで、二重音声を既定でデコードした形
// （L = 主 / R = 副）の代わりにしている。二重音声そのもの（1 フレームに SCE 2 つ）で
// pan が `-dual_mono_mode` とバイト一致することは dualMonoPans の doc コメントの実測。
//
// ffmpeg が PATH に無ければ skip する（偽 ffmpeg を使う他のテストと違い、ここは
// 実物でしか意味が無い）。
func TestBuildLiveFFmpegArgs_RealFFmpegAudioRenditions(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not in PATH")
	}
	in := filepath.Join(t.TempDir(), "in.ts")
	runFFmpeg(t, ffmpeg, nil,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30",
		"-f", "lavfi", "-i", "sine=f=440:r=48000",
		"-f", "lavfi", "-i", "sine=f=880:r=48000",
		"-filter_complex", "[1:a][2:a]join=inputs=2:channel_layout=stereo[a]",
		"-map", "0:v", "-map", "[a]", "-t", "5",
		"-c:v", "mpeg2video", "-c:a", "aac", "-f", "mpegts", in)

	profiles := []LiveProfile{
		{Name: "hd", VideoCodec: "mpeg2video", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
		{Name: "sd", VideoCodec: "mpeg2video", AudioCodec: "aac", Height: 120, SegmentSeconds: 2, PlaylistSize: 6},
	}
	// 標準 / 主 / 副の順に、期待する (L, R) の周波数。
	want := [][2]float64{{440, 880}, {440, 440}, {880, 880}}

	for _, tc := range []struct {
		name     string
		captions bool
		masters  []string
	}{
		{"default", false, []string{"hd.m3u8", "sd.m3u8"}},
		{"captions without subtitles", true, []string{"playlist.m3u8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := LiveConfig{Captions: tc.captions, Profiles: profiles}
			stdin, err := os.Open(in)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stdin.Close() }()
			if err := os.MkdirAll(filepath.Join(dir, "segments"), 0o755); err != nil {
				t.Fatal(err)
			}
			runFFmpeg(t, ffmpeg, stdin, BuildLiveFFmpegArgs(cfg, dir, false)...)

			var streams int
			for _, master := range tc.masters {
				body, err := os.ReadFile(filepath.Join(dir, master))
				if err != nil {
					t.Fatalf("master %s: %v", master, err)
				}
				for _, v := range parseMaster(t, string(body)) {
					streams++
					if len(v.audio) != 3 {
						t.Fatalf("%s: variant %s has %d audio renditions, want 3:\n%s", master, v.uri, len(v.audio), body)
					}
					variant, err := os.ReadFile(filepath.Join(dir, v.uri))
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Contains(variant, []byte("#EXT-X-PROGRAM-DATE-TIME")) {
						t.Errorf("%s: live variant playlist has no EXT-X-PROGRAM-DATE-TIME (hls.js stalls when switching back to an audio track after the live window):\n%s", v.uri, variant)
					}
					for i, uri := range v.audio {
						l, r := decodeChannelFrequencies(t, ffmpeg, filepath.Join(dir, uri))
						if math.Abs(l-want[i][0]) > 15 || math.Abs(r-want[i][1]) > 15 {
							t.Errorf("%s: audio rendition %d (%s) L/R = %.0f/%.0f Hz, want %.0f/%.0f",
								master, i, uri, l, r, want[i][0], want[i][1])
						}
					}
				}
			}
			if streams != len(profiles) {
				t.Errorf("video variants = %d, want %d (one per profile)", streams, len(profiles))
			}
		})
	}
}

type masterVariant struct {
	uri   string
	audio []string // グループ内の並び順
}

var (
	attrGroupID = regexp.MustCompile(`GROUP-ID="([^"]+)"`)
	attrURI     = regexp.MustCompile(`URI="([^"]+)"`)
	attrAudio   = regexp.MustCompile(`AUDIO="([^"]+)"`)
)

// parseMaster は master playlist の video variant と、それが参照する音声グループの
// レンディション URI（master に書かれた順）を返す。
func parseMaster(t *testing.T, body string) []masterVariant {
	t.Helper()
	groups := map[string][]string{}
	var out []masterVariant
	var pendingGroup string
	var inf bool
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA:") && strings.Contains(line, "TYPE=AUDIO"):
			g := attrGroupID.FindStringSubmatch(line)
			u := attrURI.FindStringSubmatch(line)
			if g == nil || u == nil {
				t.Fatalf("audio media line without GROUP-ID/URI: %q", line)
			}
			groups[g[1]] = append(groups[g[1]], u[1])
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			inf = true
			pendingGroup = ""
			if m := attrAudio.FindStringSubmatch(line); m != nil {
				pendingGroup = m[1]
			}
		case inf && line != "" && !strings.HasPrefix(line, "#"):
			out = append(out, masterVariant{uri: line, audio: groups[pendingGroup]})
			inf = false
		}
	}
	return out
}

// decodeChannelFrequencies は playlist の音声を 48kHz ステレオにデコードし、左右
// それぞれのゼロ交差数から周波数を見積もる（先頭 0.5 秒はエンコーダの助走として捨てる）。
func decodeChannelFrequencies(t *testing.T, ffmpeg, playlist string) (float64, float64) {
	t.Helper()
	var out bytes.Buffer
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-i", playlist,
		"-f", "s16le", "-ac", "2", "-ar", "48000", "-")
	cmd.Stdout = &out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("decoding %s: %v: %s", playlist, err, stderr.String())
	}
	samples := make([]int16, out.Len()/2)
	if err := binary.Read(&out, binary.LittleEndian, samples); err != nil {
		t.Fatal(err)
	}
	const skip = 48000 / 2 * 2
	if len(samples) <= skip+48000 {
		t.Fatalf("%s: only %d samples decoded", playlist, len(samples))
	}
	samples = samples[skip:]
	freq := func(ch int) float64 {
		var crossings, n int
		prev := samples[ch]
		for i := ch + 2; i < len(samples); i += 2 {
			if (prev < 0) != (samples[i] < 0) {
				crossings++
			}
			prev = samples[i]
			n++
		}
		return float64(crossings) / 2 / (float64(n) / 48000)
	}
	return freq(0), freq(1)
}

func runFFmpeg(t *testing.T, ffmpeg string, stdin *os.File, args ...string) {
	t.Helper()
	cmd := exec.Command(ffmpeg, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}
