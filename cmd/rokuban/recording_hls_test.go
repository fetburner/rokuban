package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestServerRecordingHLSConfig(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			pool := testutil.SetupDB(t)
			base := startTestServer(t, "api", fmt.Sprintf(`  stores:
    main-2:
      kind: fs
  originals:
    - store: main-2
  derived: main-2
  transfer:
    overlap: 48h
recording_hls:
  enabled: %t
  segment_dir: /tmp/recording-hls
live:
  enabled: false
  profiles:
    - name: hd
      video_codec: libx264
      audio_codec: aac
`, enabled))
			id := dbtest.Recording(t, pool)
			dbtest.MediaAsset(t, pool, id)
			resp, err := http.Get(fmt.Sprintf("%s/api/recordings/%d", base, id))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			var body struct {
				HLSPlayback []struct {
					Profile   string `json:"profile"`
					MasterURL string `json:"masterUrl"`
				} `json:"hlsPlayback"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if !enabled {
				if body.HLSPlayback == nil || len(body.HLSPlayback) != 0 {
					t.Fatalf("disabled playback=%+v", body.HLSPlayback)
				}
				return
			}
			if len(body.HLSPlayback) != 1 {
				t.Fatalf("playback=%+v", body.HLSPlayback)
			}
			want := fmt.Sprintf("/api/media/stores/main-2/recordings/%d/hls/v0/hd/master.m3u8", id)
			if body.HLSPlayback[0].Profile != "hd" || body.HLSPlayback[0].MasterURL != want {
				t.Fatalf("playback=%+v, want %s", body.HLSPlayback, want)
			}
		})
	}
}

// writeFakeFFTools は起動時検査（ffmpeg -decoders に mpeg2video があること）だけを
// 通す偽の ffmpeg / ffprobe を作り、live セクションに差す YAML を返す。
func writeFakeFFTools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ffmpeg := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\necho ' V....D mpeg2video           MPEG-2 video'\n"
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ffprobe := filepath.Join(dir, "ffprobe")
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("  ffmpeg: %s\n  ffprobe: %s\n", ffmpeg, ffprobe)
}

// TestServerStreamerMountsRecordingHLS は streamer ロール + recording_hls.enabled なら
// 引数なしで世代付き録画 HLS のルートが載り、--no-recording-hls で外れることを実際の
// サーバー起動経路で見る。API は enabled のとき URL を返すので、--all 等で載らないと
// 誰も応答しない URL を返す。
//
// 判別は世代の食い違い: ハンドラが居れば 410、居なければ 404。
//
// 壊し方: server.go の `cfg.RecordingHLS.Enabled && !noRecordingHLS` から
// `!noRecordingHLS` を外すと flag 付きの行が落ち、`cfg.RecordingHLS.Enabled &&` を
// 外すと enabled=false の行が落ちる。
func TestServerStreamerMountsRecordingHLS(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		args    []string
		want    int
	}{
		{name: "enabled", enabled: true, want: http.StatusGone},
		{name: "enabled with --no-recording-hls", enabled: true, args: []string{"--no-recording-hls"}, want: http.StatusNotFound},
		{name: "disabled", enabled: false, want: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			base := startTestServer(t, "streamer", fmt.Sprintf(`  stores:
    main:
      kind: fs
  originals:
    - store: main
  derived: main
  transfer:
    overlap: 48h
recording_hls:
  enabled: %t
  segment_dir: %s
live:
  enabled: false
%s  profiles:
    - name: hd
      video_codec: libx264
      audio_codec: aac
`, tc.enabled, t.TempDir(), writeFakeFFTools(t)), tc.args...)
			id := dbtest.Recording(t, pool)
			dbtest.MediaAsset(t, pool, id)
			resp, err := http.Get(fmt.Sprintf("%s/api/media/stores/main/recordings/%d/hls/v7/hd/master.m3u8", base, id))
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status=%d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}
