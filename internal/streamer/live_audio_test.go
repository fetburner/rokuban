package streamer

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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
// 実物でしか意味が無い）。CI は ROKUBAN_REQUIRE_FFMPEG で skip を禁じる
// （lookPathFFmpeg）。
func TestBuildLiveFFmpegArgs_RealFFmpegAudioRenditions(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
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

// lookPathFFmpeg は ffmpeg のパスを返す。無ければ skip するが、ROKUBAN_REQUIRE_FFMPEG
// が設定されていれば落とす --- CI で ffmpeg の導入が壊れたときに、実経路の判定が
// 黙って skip に戻らないようにする。
func lookPathFFmpeg(t *testing.T) string {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv("ROKUBAN_REQUIRE_FFMPEG") != "" {
			t.Fatalf("ffmpeg not in PATH but ROKUBAN_REQUIRE_FFMPEG is set: %v", err)
		}
		t.Skip("ffmpeg not in PATH")
	}
	return ffmpeg
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

// TestBuildChaseFFmpegArgs_RealFFmpegAudioRenditions は追っかけの実出力に標準 / 主 / 副
// の rendition があり、master の並びと聞こえる音が UI の選択契約に一致することを測る。
// イベント playlist で長く聴いた後のトラック切替と seek・再開の実ブラウザ判定は
// web/e2e/chase-audio.mjs が担う。
func TestBuildChaseFFmpegArgs_RealFFmpegAudioRenditions(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	in := filepath.Join(t.TempDir(), "in.ts")
	runFFmpeg(t, ffmpeg, nil,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30",
		"-f", "lavfi", "-i", "sine=f=440:r=48000",
		"-f", "lavfi", "-i", "sine=f=880:r=48000",
		"-filter_complex", "[1:a][2:a]join=inputs=2:channel_layout=stereo[a]",
		"-map", "0:v", "-map", "[a]", "-t", "6",
		"-c:v", "mpeg2video", "-c:a", "aac", "-f", "mpegts", in)

	profiles := []LiveProfile{
		{Name: "hd", VideoCodec: "mpeg2video", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
		{Name: "sd", VideoCodec: "mpeg2video", AudioCodec: "aac", Height: 120, SegmentSeconds: 2, PlaylistSize: 6},
	}
	want := [][2]float64{{440, 880}, {440, 440}, {880, 880}}

	for _, tc := range []struct {
		name     string
		captions bool
		masters  []string
	}{
		{name: "profile masters", masters: []string{"hd.m3u8", "sd.m3u8"}},
		{name: "caption master", captions: true, masters: []string{"playlist.m3u8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "segments"), 0o755); err != nil {
				t.Fatal(err)
			}
			stdin, err := os.Open(in)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stdin.Close() }()
			args := BuildChaseFFmpegArgs(LiveConfig{Captions: tc.captions, Profiles: profiles}, dir, false)
			runFFmpeg(t, ffmpeg, stdin, args...)

			var videoVariants int
			for _, master := range tc.masters {
				body, err := os.ReadFile(filepath.Join(dir, master))
				if err != nil {
					t.Fatalf("master %s: %v", master, err)
				}
				for _, variant := range parseMaster(t, string(body)) {
					videoVariants++
					if len(variant.audio) != 3 {
						t.Fatalf("%s: variant %s has %d audio renditions, want 3:\n%s", master, variant.uri, len(variant.audio), body)
					}
					videoPlaylist, err := os.ReadFile(filepath.Join(dir, variant.uri))
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Contains(videoPlaylist, []byte("#EXT-X-PLAYLIST-TYPE:EVENT")) {
						t.Errorf("%s: video playlist is not EVENT:\n%s", variant.uri, videoPlaylist)
					}
					for i, uri := range variant.audio {
						playlist := filepath.Join(dir, uri)
						audioPlaylist, err := os.ReadFile(playlist)
						if err != nil {
							t.Fatal(err)
						}
						if !bytes.Contains(audioPlaylist, []byte("#EXT-X-PLAYLIST-TYPE:EVENT")) {
							t.Errorf("%s: audio playlist is not EVENT:\n%s", uri, audioPlaylist)
						}
						l, r := decodeChannelFrequencies(t, ffmpeg, playlist)
						if math.Abs(l-want[i][0]) > 15 || math.Abs(r-want[i][1]) > 15 {
							t.Errorf("%s: audio rendition %d L/R = %.0f/%.0f Hz, want %.0f/%.0f",
								master, i, l, r, want[i][0], want[i][1])
						}
					}
				}
			}
			if videoVariants != len(profiles) {
				t.Errorf("video variants = %d, want %d", videoVariants, len(profiles))
			}
		})
	}
}

// TestBuildChaseFFmpegArgs_AudioRenditions は追っかけの両経路に標準 / 主 / 副の
// audio rendition を出し、HLS master を使う形を固定する。
func TestBuildChaseFFmpegArgs_AudioRenditions(t *testing.T) {
	profiles := []LiveProfile{
		{Name: "hd", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
		{Name: "sd", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
	}
	for _, captions := range []bool{false, true} {
		args := BuildChaseFFmpegArgs(LiveConfig{Captions: captions, Profiles: profiles}, "/tmp/chase", false)
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "agroup") || strings.Count(joined, "pan=") != 2*len(profiles) {
			t.Errorf("captions=%v: chase args are missing standard/main/sub audio renditions: %v", captions, args)
		}
		if n := strings.Count(joined, "-map 0:a:0"); n != 3*len(profiles) {
			t.Errorf("captions=%v: -map 0:a:0 count = %d, want %d: %v", captions, n, 3*len(profiles), args)
		}
		if !captions {
			for _, want := range []string{
				"-master_pl_name hd.m3u8",
				"-hls_segment_filename /tmp/chase/segments/hd.%v_seg%05d.ts -hls_base_url segments/ /tmp/chase/hd.%v.m3u8",
				"-master_pl_name sd.m3u8",
				"-hls_segment_filename /tmp/chase/segments/sd.%v_seg%05d.ts -hls_base_url segments/ /tmp/chase/sd.%v.m3u8",
			} {
				if !strings.Contains(joined, want) {
					t.Errorf("args missing %q: %v", want, args)
				}
			}
		} else if !strings.Contains(joined, "-master_pl_name playlist.m3u8") ||
			!strings.Contains(joined, "-var_stream_map v:0,agroup:a0 v:1,agroup:a1 a:0,agroup:a0,default:yes a:1,agroup:a0 a:2,agroup:a0 a:3,agroup:a1,default:yes a:4,agroup:a1 a:5,agroup:a1") {
			t.Errorf("captions chase var_stream_map or master changed: %v", args)
		}
	}
}

// TestLiveStreamer_VariantPlaylistRecreatesSession は、セッションが消えた後の
// variant playlist 要求がセッションを作り直して 200 を返すことを固定する
// （serveVariantPlaylist）。hls.js は master を最初の 1 回しか取らず、以後は variant
// だけを取り直す。ここで 404 を返すと、idle GC・ffmpeg の異常終了の後に hls.js が
// fatal で止まる（4xx は再試行しない）。
func TestLiveStreamer_VariantPlaylistRecreatesSession(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	const serviceID = 1024
	master := playlistURL(srv.URL, 0, serviceID, "h264")
	resp, err := http.Get(master)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	variant := resolveRelative(t, master, firstSegmentName(t, string(body)))

	// idle GC と同じくセッションを止める（map から消え、ディレクトリも消える）。
	ls.mu.Lock()
	s := ls.sessions[serviceID]
	ls.mu.Unlock()
	s.stop()
	ls.mu.Lock()
	_, stillThere := ls.sessions[serviceID]
	ls.mu.Unlock()
	if stillThere {
		t.Fatal("session is still registered after stop")
	}

	resp, err = http.Get(variant)
	if err != nil {
		t.Fatal(err)
	}
	vbody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("variant %s after the session was gone: status = %d, want 200 (a fresh session)", variant, resp.StatusCode)
	}
	if !strings.Contains(string(vbody), "#EXTINF") {
		t.Errorf("variant body = %q, want a media playlist", vbody)
	}
	if got := state.requestCount(); got != 2 {
		t.Errorf("mirakc stream requests = %d, want 2 (the original and the recreated session)", got)
	}
}

// TestLiveStreamer_UnknownPlaylistNameDoesNotStartSession は、variant の形でない
// `.m3u8`（master の名前・存在しないプロファイル・番号の無い名前）が 404 で、
// セッション（= mirakc のチューナー）を起こさないことを固定する。master を
// `/{name}` で取る要求が 15 秒待って 504 になることもない。
func TestLiveStreamer_UnknownPlaylistNameDoesNotStartSession(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	for _, name := range []string{"h264.m3u8", "other.0.m3u8", "h264.x.m3u8", "h264..m3u8", "playlist_0.m3u8"} {
		url := fmt.Sprintf("%s/api/sites/%s/networks/0/services/1024/live/%s", srv.URL, testLiveSite, name)
		start := time.Now()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", name, resp.StatusCode)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("%s: took %v, want an immediate 404", name, elapsed)
		}
	}
	if got := state.requestCount(); got != 0 {
		t.Errorf("mirakc stream requests = %d, want 0 (unknown names must not start a session)", got)
	}
}
