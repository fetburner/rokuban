package worker

import (
	"context"
	"path"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/contentpath"
)

func meaningCfg(name string, cut bool, container string) config.EncodeConfig {
	return config.EncodeConfig{Profiles: []config.EncodeProfile{{Name: name, Cut: cut, Container: container}}}
}

// mustEncodedRelPath は書き手（EncodedRelPath）でパスを作る。手書きのリテラルにすると、
// 書き手と読み手（cutGeneration）が食い違っても検査のテストは通り続ける。
func mustEncodedRelPath(t *testing.T, original, profile, container string, generation int) string {
	t.Helper()
	p, err := EncodedRelPath(original, profile, container, generation)
	if err != nil {
		t.Fatalf("EncodedRelPath(%q, %q, %q, %d): %v", original, profile, container, generation, err)
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
	recID := insertTestRecording(t, pool)
	const orig = "sites/default/20240101/120000_title_1024.m2ts"

	// "plain" は cut なしの mp4。
	seedEncodedAsset(t, pool, recID, "plain", mustEncodedRelPath(t, orig, "plain", "mp4", 0))
	// "cutp" はカット版の mkv。media_asset_cuts の行は意図して作らない
	// （catalog の rescue は encoded を active で戻すが cuts は戻さない）。
	seedEncodedAsset(t, pool, recID, "cutp", mustEncodedRelPath(t, orig, "cutp", "mkv", 1))
	// 名前に `.g2` を含む cut でないプロファイルは、世代番号と取り違えない。
	seedEncodedAsset(t, pool, recID, "x.g2", mustEncodedRelPath(t, orig, "x.g2", "mp4", 0))
	// deleted の encoded は過去の意味に数えない。
	delID := seedEncodedAsset(t, pool, recID, "gone", mustEncodedRelPath(t, orig, "gone", "mp4", 0))
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

// TestCutGeneration_RoundTripsEncodedRelPath は、設定が受け入れる名前について、
// 書き手（EncodedRelPath）の出力から読み手（cutGeneration）が世代と cut の有無を、
// path.Ext が container を読み戻せることを固定する。起動時の意味の検査は rel_path を
// 過去の意味の証拠にするので、読み戻せない名前を設定が受け入れると、設定を変えていない
// のに起動を拒む。
//
// 設定が拒む名前は、書き手もエラーにすることと、拒む理由も確かめる。
func TestCutGeneration_RoundTripsEncodedRelPath(t *testing.T) {
	accepted := []string{"h264", "h264_vaapi", "cut", "e2e-slow", "x.g2", "x.g1.g1", "g", "1", "a_b", "_", "__", "_x", "x_", ".hidden", "日本語 720p"}
	originals := []string{
		"sites/home/20240101/120000_title_1024.m2ts",
		"sites/home/a.b/c_x.g1.m2ts", // ディレクトリと原本の名前に紛らわしい形を含む
		"sites/home/noext",
	}
	for _, name := range accepted {
		if err := config.ValidateEncodeProfileName(name); err != nil {
			t.Errorf("config rejects %q: %v", name, err)
			continue
		}
		for _, orig := range originals {
			for _, container := range []string{"mp4", "mkv"} {
				for gen := 0; gen <= 12; gen++ {
					rel := mustEncodedRelPath(t, orig, name, container, gen)
					n, ok := cutGeneration(rel, name)
					if ok != (gen > 0) || (ok && n != gen) {
						t.Errorf("cutGeneration(%q, %q) = (%d, %t), want (%d, %t)", rel, name, n, ok, gen, gen > 0)
					}
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
	for _, name := range append(dotted, "a:b", "a/b", " a", "a..b") {
		if config.ValidateEncodeProfileName(name) == nil {
			t.Errorf("config accepts %q", name)
		}
		if got, err := EncodedRelPath(orig, name, "mp4", 1); err == nil {
			t.Errorf("EncodedRelPath(%q) = %q, want an error", name, got)
		}
	}
	// rescue が拾った encoded の名前（catalog の rescue_scan）。
	rescue := config.RescueEncodedProfilePrefix + "mp4"
	if config.ValidateEncodeProfileName(rescue) == nil {
		t.Errorf("config accepts the rescue profile name")
	}
	if _, err := EncodedRelPath(orig, rescue, "mp4", 1); err == nil {
		t.Errorf("EncodedRelPath accepts the rescue profile name")
	}
}

// TestCutGeneration_RejectsFormsTheWriterNeverWrites は、EncodedRelPath が書かない
// 形を cut と読まないことを固定する。書き手の像の上では `_` の境界と正規の整数の検査は
// 結果を変えない（上の往復テスト）ので、境界を外す・正規の検査を外す変異はここでしか
// 落ちない。読み違えると、cut でないファイルを cut の証拠にして、cut: true への反転を
// 通してしまう。
func TestCutGeneration_RejectsFormsTheWriterNeverWrites(t *testing.T) {
	for _, rel := range []string{
		"sites/home/a-cut.g1.mp4",   // `_` の境界が無い
		"sites/home/a_xcut.g1.mp4",  // 別の名前の末尾が一致しているだけ
		"sites/home/a_cut.g01.mp4",  // 先頭ゼロ
		"sites/home/a_cut.g+1.mp4",  // 符号
		"sites/home/a_cut.g0.mp4",   // 0 は世代ではない
		"sites/home/a_cut.g1x.mp4",  // 数字の後ろに残りがある
		"sites/home/a_cut.g1/b.mp4", // 世代の形はディレクトリでなく basename にある
	} {
		if n, ok := cutGeneration(rel, "cut"); ok {
			t.Errorf("cutGeneration(%q, \"cut\") = (%d, true), want false", rel, n)
		}
	}
}
