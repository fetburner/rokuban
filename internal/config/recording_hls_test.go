package config

import (
	"strings"
	"testing"
)

func TestLoadRecordingHLS(t *testing.T) {
	for _, tc := range []struct {
		name, setting, store string
		wantEnabled          bool
		wantError            string
	}{
		{name: "default"},
		{name: "disabled", setting: "recording_hls:\n  enabled: false\n"},
		{name: "enabled missing store", setting: "recording_hls:\n  enabled: true\n", wantError: "storage.stores"},
		{name: "enabled requires scratch", setting: "recording_hls:\n  enabled: true\n", store: "main", wantError: "recording_hls.segment_dir"},
		{name: "enabled", setting: "recording_hls:\n  enabled: true\n  segment_dir: /tmp/hls\nlive:\n  enabled: false\n  profiles:\n    - name: hd\n      video_codec: libx264\n      audio_codec: aac\n", store: "disk-2", wantEnabled: true},
		{name: "cache lower bound is strict", setting: "recording_hls:\n  enabled: true\n  segment_dir: /tmp/hls\n  max_concurrent: 2\n  cache_bytes: 1006632960\n", store: "main", wantError: "recording_hls.cache_bytes"},
		{name: "disabled live rejects no profiles", setting: "recording_hls:\n  enabled: true\n  segment_dir: /tmp/hls\nlive:\n  enabled: false\n", store: "main", wantError: "live.profiles"},
		{name: "disabled live rejects dot segment", setting: "recording_hls:\n  enabled: true\n  segment_dir: /tmp/hls\nlive:\n  enabled: false\n  profiles:\n    - name: ..\n", store: "main", wantError: "live.profiles"},
		{name: "disabled live rejects empty profile name", setting: "recording_hls:\n  enabled: true\n  segment_dir: /tmp/hls\nlive:\n  enabled: false\n  profiles:\n    - name: ''\n", store: "main", wantError: "live.profiles"},
		{name: "disabled live rejects duplicate names", setting: "recording_hls:\n  enabled: true\n  segment_dir: /tmp/hls\nlive:\n  enabled: false\n  profiles:\n    - name: hd\n    - name: hd\n", store: "main", wantError: "live.profiles"},
		{name: "disabled live rejects incomplete transform profile", setting: "recording_hls:\n  enabled: true\n  segment_dir: /tmp/hls\nlive:\n  enabled: false\n  profiles:\n    - name: hd\n", store: "main", wantError: "video_codec"},
		{name: "uppercase", store: "Disk", wantError: "storage.stores"},
		{name: "slash", store: "a/b", wantError: "storage.stores"},
		{name: "underscore", store: "a_b", wantError: "storage.stores"},
		{name: "unicode", store: "録画", wantError: "storage.stores"},
		{name: "space", store: "a b", wantError: "storage.stores"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := minimalConfig + tc.setting
			if tc.store != "" {
				policy := "media_dir: /mnt/media\n  stores:\n    " + tc.store + ":\n      kind: fs\n  originals:\n    - store: " + tc.store + "\n  derived: " + tc.store
				data = strings.Replace(data, "media_dir: /mnt/media", policy, 1)
			}
			cfg, err := Load(writeConfig(t, data))
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error=%v, want %s validation", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RecordingHLS.Enabled != tc.wantEnabled || cfg.Storage.MediaStoreName != tc.store {
				t.Fatalf("recording_hls=%+v, store=%q", cfg.RecordingHLS, cfg.Storage.MediaStoreName)
			}
			if cfg.RecordingHLS.CacheBytes != defaultRecordingHLSCacheBytes || cfg.RecordingHLS.MaxConcurrent != defaultRecordingHLSConcurrency || cfg.RecordingHLS.WaitTimeout != defaultRecordingHLSWaitTimeout {
				t.Errorf("recording HLS defaults = %+v", cfg.RecordingHLS)
			}
		})
	}
}
