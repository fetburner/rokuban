package worker

import (
	"context"
	"fmt"
	"path"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
)

func meaningCfg(name string, cut bool, container string) config.EncodeConfig {
	return config.EncodeConfig{Profiles: []config.EncodeProfile{{Name: name, Cut: cut, Container: container}}}
}

// mustEncodedRelPath は書き手（EncodedRelPath）でパスを作る。
func mustEncodedRelPath(t *testing.T, original, profile, container string, generation int, cut bool) string {
	t.Helper()
	p, err := EncodedRelPath(original, profile, container, generation, cut)
	if err != nil {
		t.Fatalf("EncodedRelPath(%q, %q, %q, %d, %t): %v", original, profile, container, generation, cut, err)
	}
	return p
}

// 受け入れの 4 ケース、rescue 後の状態、作り直し要求だけの名前、証拠の無い名前を固定する。
func TestCheckEncodeProfileMeaning(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	recID := dbtest.Recording(t, pool)
	const orig = "sites/default/20240101/120000_title_1024.m2ts"

	// "plain" は cut なしの mp4。
	seedEncodedAsset(t, pool, recID, "plain", mustEncodedRelPath(t, orig, "plain", "mp4", 0, false))
	// "cutp" はカット版の mkv。media_asset_cuts の行は意図して作らない
	// （catalog の rescue は encoded を active で戻すが cuts は戻さない）。
	seedEncodedAsset(t, pool, recID, "cutp", mustEncodedRelPath(t, orig, "cutp", "mkv", 1, true))
	// cut は rel_path の意味印から読む。DB の世代が正でも uncut のまま。
	seedEncodedAsset(t, pool, recID, "plainwithgeneration", mustEncodedRelPath(t, orig, "plainwithgeneration", "mp4", 0, false), 1)
	// 世代列が 0 でも、カタログ復元後に残る `.g1` は cut の意味を保つ。
	seedEncodedAsset(t, pool, recID, "pathcut", mustEncodedRelPath(t, orig, "pathcut", "mp4", 1, true), 0)
	// 非 cut の削除後再生成は世代を持つが、cut とは区別できる。
	seedEncodedAsset(t, pool, recID, "plainregen", mustEncodedRelPath(t, orig, "plainregen", "mp4", 1, false), 1)
	// 名前に `.g2` を含む cut でないプロファイルは、世代番号と取り違えない。
	seedEncodedAsset(t, pool, recID, "x.g2.hd", mustEncodedRelPath(t, orig, "x.g2.hd", "mp4", 0, false))
	// deleted の encoded は過去の意味に数えない。
	delID := seedEncodedAsset(t, pool, recID, "gone", mustEncodedRelPath(t, orig, "gone", "mp4", 0, false))
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
		{"dotted name stays non-cut", meaningCfg("x.g2.hd", false, "mp4"), ""},
		{"dotted name flipped to cut", meaningCfg("x.g2.hd", true, "mp4"), "cut=true"},
		{"uncut generation does not become cut", meaningCfg("plainwithgeneration", false, "mp4"), ""},
		{"uncut generation rejects cut flip", meaningCfg("plainwithgeneration", true, "mp4"), "cut=true"},
		{"restored cut path survives zero generation", meaningCfg("pathcut", true, "mp4"), ""},
		{"restored cut path rejects false", meaningCfg("pathcut", false, "mp4"), "cut=false"},
		{"uncut tombstone regeneration remains uncut", meaningCfg("plainregen", false, "mp4"), ""},
		{"uncut tombstone regeneration rejects cut flip", meaningCfg("plainregen", true, "mp4"), "cut=true"},
		{"request only, cut kept", meaningCfg("reqonly", true, "mp4"), ""},
		{"request only, cut flipped", meaningCfg("reqonly", false, "mp4"), "cut=false"},
		{"profile removed", config.EncodeConfig{Profiles: []config.EncodeProfile{{Name: "other", Container: "mp4"}}}, ""},
		{"no active encoded for name", meaningCfg("gone", true, "mkv"), ""},
		{"brand new name", meaningCfg("fresh", true, "mkv"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckEncodeProfileMeaning(ctx, pgstoretest.Media(pool), tc.cfg)
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

// TestEncodedRelPathPreservesProfileAndContainer は受理する profile 名をそのまま
// パスに書き、拡張子から container を読み戻せることを固定する。
func TestEncodedRelPathPreservesProfileAndContainer(t *testing.T) {
	accepted := []string{"h264", "h264_vaapi", "cut", "e2e-slow", "x", "x.g2.hd", "x.g", "x.g0", "x.g01", "x.gz", "g", "1", "a_b", "_", "__", "_x", "x_", ".hidden", "日本語 720p"}
	originals := []string{
		"sites/home/20240101/120000_title_1024.m2ts",
		"sites/home/a.b/c_x.g1.m2ts", // ディレクトリと原本の名前に紛らわしい形を含む
		"sites/home/noext",
	}
	// 同じ原本からは、受け入れる名前と世代ごとに異なる rel_path になる（上書きしない）。
	seen := map[string]string{}
	for _, name := range accepted {
		if err := config.ValidateEncodeProfileName(name); err != nil {
			t.Errorf("config rejects %q: %v", name, err)
			continue
		}
		for _, orig := range originals {
			for _, container := range []string{"mp4", "mkv"} {
				for gen := 0; gen <= 12; gen++ {
					modes := []bool{false}
					if gen > 0 {
						modes = append(modes, true)
					}
					for _, cut := range modes {
						rel := mustEncodedRelPath(t, orig, name, container, gen, cut)
						who := fmt.Sprintf("%q gen %d cut=%t", name, gen, cut)
						if prev, dup := seen[rel]; dup {
							t.Errorf("%s and %s share the rel_path %q", prev, who, rel)
						}
						seen[rel] = who
						if got := path.Ext(rel); got != "."+container {
							t.Errorf("path.Ext(%q) = %q, want %q", rel, got, "."+container)
						}
						if !strings.Contains(path.Base(rel), "_"+name+".") {
							t.Errorf("rel_path %q does not contain the profile name %q as is", rel, name)
						}
					}
				}
			}
		}
	}

	// 設定が拒む名前は、書き手も書かない（同じ規則を引く）。拒む理由は、名前を
	// サニタイズして入れたときに起きることで示す。
	const orig = "sites/home/a.m2ts"
	sanitized := func(name string) string { return contentpath.SanitizeContentPath(strings.ReplaceAll(name, "/", "_")) }
	// サニタイズで変わる名前は、受け入れる別の名前と同じ rel_path になる。
	collide := map[string]string{"a:b": "a_b", "a/b": "a_b", " a": "a", "a..b": "a_b"}
	for name, same := range collide {
		if sanitized(name) != same {
			t.Errorf("%q sanitizes to %q, not %q; the rejection would be unnecessary", name, sanitized(name), same)
		}
	}
	// `.` で終わる名前は、続く `.` と合わさった `..` が潰され、世代を読めない。
	dotted := []string{"x.", "x. "}
	for _, name := range dotted {
		component := "a_" + sanitized(name) + ".g1.mp4"
		if contentpath.SanitizeContentPath(component) == component {
			t.Errorf("%q survives sanitizing as %q; the rejection would be unnecessary", name, component)
		}
	}
	// 世代の接尾辞で終わる名前は、別の profile の cut / uncut 世代版と衝突する。
	generationSuffixed := map[string]struct {
		head string
		gen  int
		cut  bool
	}{"x.g1": {"x", 1, true}, "x.g2.hd.g3": {"x.g2.hd", 3, true}, "h264.g10": {"h264", 10, true}, "x.u1": {"x", 1, false}, "h264.u10": {"h264", 10, false}}
	for name, suffix := range generationSuffixed {
		if got, want := mustEncodedRelPath(t, orig, suffix.head, "mp4", suffix.gen, suffix.cut), "sites/home/a_"+name+".mp4"; got != want {
			t.Errorf("profile %q generation %d cut=%t = %q, not %q; the rejection of %q would be unnecessary", suffix.head, suffix.gen, suffix.cut, got, want, name)
		}
	}
	for _, name := range append(dotted, "a:b", "a/b", " a", "a..b", "x.g1", "x.g2.hd.g3", "h264.g10", "x.u1", "h264.u10") {
		if config.ValidateEncodeProfileName(name) == nil {
			t.Errorf("config accepts %q", name)
		}
		if got, err := EncodedRelPath(orig, name, "mp4", 1, false); err == nil {
			t.Errorf("EncodedRelPath(%q) = %q, want an error", name, got)
		}
	}
	// rescue が拾った encoded の名前（catalog の rescue_scan）。
	rescue := config.RescueEncodedProfilePrefix + "mp4"
	if config.ValidateEncodeProfileName(rescue) == nil {
		t.Errorf("config accepts the rescue profile name")
	}
	if _, err := EncodedRelPath(orig, rescue, "mp4", 1, false); err == nil {
		t.Errorf("EncodedRelPath accepts the rescue profile name")
	}
}
