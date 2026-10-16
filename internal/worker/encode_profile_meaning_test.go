package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
)

func meaningCfg(name string, cut bool, container string) config.EncodeConfig {
	return config.EncodeConfig{Profiles: []config.EncodeProfile{{Name: name, Cut: cut, Container: container}}}
}

// 受け入れの 4 ケース、rescue 後の状態、作り直し要求だけの名前、証拠の無い名前を固定する。
func TestCheckEncodeProfileMeaning(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	recID := insertTestRecording(t, pool)

	// "plain" は cut なしの mp4。
	seedEncodedAsset(t, pool, recID, "plain", "m/orig_plain.mp4")
	// "cutp" はカット版の mkv。media_asset_cuts の行は意図して作らない
	// （catalog の rescue は encoded を active で戻すが cuts は戻さない）。
	seedEncodedAsset(t, pool, recID, "cutp", "m/orig_cutp.g1.mkv")
	// 名前に `.g2` を含む cut でないプロファイルは、世代番号と取り違えない。
	seedEncodedAsset(t, pool, recID, "x.g2", "m/orig_x.g2.mp4")
	// deleted の encoded は過去の意味に数えない。
	delID := seedEncodedAsset(t, pool, recID, "gone", "m/orig_gone.mp4")
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET state = 'deleted' WHERE id = $1`, delID); err != nil {
		t.Fatalf("deleting asset: %v", err)
	}
	// "reqonly" は作り直し要求の行だけがある（active な encoded は無い）。
	if _, err := pool.Exec(ctx, `INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
		VALUES ($1, 'reqonly', '{[0,1000)}'::int8multirange)`, recID); err != nil {
		t.Fatalf("seeding rebuild request: %v", err)
	}

	cases := []struct {
		name    string
		cfg     config.EncodeConfig
		wantErr string
	}{
		{"unchanged plain", meaningCfg("plain", false, "mp4"), ""},
		{"unchanged cut without cuts row (after rescue)", meaningCfg("cutp", true, "mkv"), ""},
		{"cut true to false", meaningCfg("cutp", false, "mkv"), "cut=false"},
		{"cut false to true", meaningCfg("plain", true, "mp4"), "cut=true"},
		{"container changed", meaningCfg("plain", false, "mkv"), "container"},
		{"dotted name stays non-cut", meaningCfg("x.g2", false, "mp4"), ""},
		{"dotted name flipped to cut", meaningCfg("x.g2", true, "mp4"), "cut=true"},
		{"request only, cut kept", meaningCfg("reqonly", true, "mp4"), ""},
		{"request only, cut flipped", meaningCfg("reqonly", false, "mp4"), "cut=false"},
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
