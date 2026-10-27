package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestServerRecordingHLSConfig(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			pool := testutil.SetupDB(t)
			base := startTestServer(t, "api", fmt.Sprintf(`  media_store_name: main-2
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
