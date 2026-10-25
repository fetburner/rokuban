package config

import (
	"strings"
	"testing"
)

func TestLoadRecordingHLS(t *testing.T) {
	for _, tc := range []struct {
		name, setting, store   string
		wantEnabled, wantError bool
	}{
		{name: "default"},
		{name: "disabled", setting: "recording_hls:\n  enabled: false\n"},
		{name: "enabled missing store", setting: "recording_hls:\n  enabled: true\n", wantError: true},
		{name: "enabled", setting: "recording_hls:\n  enabled: true\n", store: "disk-2", wantEnabled: true},
		{name: "uppercase", store: "Disk", wantError: true},
		{name: "slash", store: "a/b", wantError: true},
		{name: "underscore", store: "a_b", wantError: true},
		{name: "unicode", store: "録画", wantError: true},
		{name: "space", store: "a b", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := minimalConfig + tc.setting
			if tc.store != "" {
				data = strings.Replace(data, "media_dir: /mnt/media", "media_dir: /mnt/media\n  media_store_name: "+tc.store, 1)
			}
			cfg, err := Load(writeConfig(t, data))
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "storage.media_store_name") {
					t.Fatalf("error=%v, want media_store_name validation", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RecordingHLS.Enabled != tc.wantEnabled || cfg.Storage.MediaStoreName != tc.store {
				t.Fatalf("recording_hls=%+v, store=%q", cfg.RecordingHLS, cfg.Storage.MediaStoreName)
			}
		})
	}
}
