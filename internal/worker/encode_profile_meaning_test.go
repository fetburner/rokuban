package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
)

func meaningCfg(name string, cut bool, container string) config.EncodeConfig {
	return config.EncodeConfig{Profiles: []config.EncodeProfile{{Name: name, Cut: cut, Container: container}}}
}

// 4 つの受け入れケースと「active な encoded が無い名前は対象外」を固定する。
func TestCheckEncodeProfileMeaning(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	recID := insertTestRecording(t, pool)

	// "plain" は cut なしの mp4、"cutp" はカット版の mkv。
	seedEncodedAsset(t, pool, recID, "plain", "m/orig_plain.mp4")
	cutID := seedEncodedAsset(t, pool, recID, "cutp", "m/orig_cutp.g1.mkv")
	if err := setFrozenCuts(t, pool, cutID, []chapters.Range{{StartMs: 0, EndMs: 1000}}); err != nil {
		t.Fatalf("freezing cuts: %v", err)
	}
	// deleted の encoded は過去の意味に数えない。
	delID := seedEncodedAsset(t, pool, recID, "gone", "m/orig_gone.mp4")
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET state = 'deleted' WHERE id = $1`, delID); err != nil {
		t.Fatalf("deleting asset: %v", err)
	}

	cases := []struct {
		name    string
		cfg     config.EncodeConfig
		wantErr string
	}{
		{"unchanged plain", meaningCfg("plain", false, "mp4"), ""},
		{"unchanged cut", meaningCfg("cutp", true, "mkv"), ""},
		{"cut true to false", meaningCfg("cutp", false, "mkv"), "cut=false"},
		{"cut false to true", meaningCfg("plain", true, "mp4"), "cut=true"},
		{"container changed", meaningCfg("plain", false, "mkv"), "container"},
		{"profile removed", config.EncodeConfig{Profiles: []config.EncodeProfile{{Name: "other", Container: "mp4"}}}, ""},
		{"no active encoded for name", meaningCfg("gone", true, "mkv"), ""},
		{"brand new name", meaningCfg("fresh", true, "mkv"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckEncodeProfileMeaning(ctx, pool, tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "new name") {
				t.Fatalf("error = %v, want it to contain %q and the rename hint", err, tc.wantErr)
			}
		})
	}
}
