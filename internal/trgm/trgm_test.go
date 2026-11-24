package trgm

import (
	"math"
	"testing"
)

// similarityCases は Postgres 17（pg_trgm、LC_CTYPE=en_US.utf8、libc プロバイダ）で
// SELECT similarity($1, $2) を実行して得た値である。期待値は実装を使わずリテラルで書く。
// 結合文字と全角文字は \u エスケープで書き、入力の正規化形に依らないようにする。
// trgm_pg_test.go も同じ表を Postgres に問い直す。
var similarityCases = []struct {
	name string
	a, b string
	want float32
}{
	{"両方空", "", "", 0},
	{"片方空", "abc", "", 0},
	{"語が無い記号だけ", "!!!", "abc", 0},
	{"同一", "abc", "abc", 1},
	{"大文字小文字", "ABC", "abc", 1},
	{"記号と空白で区切る", "Foo Bar", "foo-bar", 1},
	{"全角英大文字の同一", "ＡＢＣ", "ＡＢＣ", 1},
	{"全角英大文字と半角", "ＡＢＣ", "abc", 0},
	{"全角英小文字と全角英大文字", "ａｂｃ", "ＡＢＣ", 1},
	{"半角カナと全角カナ", "ｱｲｳ", "アイウ", 0},
	{"半角カナの前方一致", "ｱｲｳ", "ｱｲｳｴ", 0.5},
	{"ひらがなの一字違い", "あいうえお", "あいうえか", 0.5},
	{"Nl の大文字と小文字（Ⅻ と ⅻ）", "Ⅻ", "ⅻ", 1},
	{"Nl を含む題名", "第Ⅻ話", "第ⅻ話", 1},
	{"Nl と数字は別の語", "Ⅻ", "12", 0},
	{"分解形の結合文字 U+0301 は語の文字でない", "café", "cafe", 1},
	{"合成済みの é（U+00E9）と英字", "café", "cafe", 0.42857143},
	{"Other_Alphabetic の結合文字 U+0345 は語の文字", "aͅb", "ab", 0.16666667},
	{"Mn（U+0301）の後ろの英字", "x́", "x", 1},
	{"空白の連続", "a  b", "a b", 1},
	{"全角数字と半角数字", "１２３", "123", 0},
	{"全角空白で区切る題名", "ドラえもん 第1話", "ドラえもん　第1話", 1},
	{"題名の後ろに語が付く", "宇宙戦艦ヤマト 2199 第二章 出撃", "宇宙戦艦ヤマト 2199 第二章 出撃 再放送", 0.8333333},
	{"題名の後ろに語が付く（短い）", "宇宙戦艦ヤマト 2199", "宇宙戦艦ヤマト 2199 再放送", 0.7647059},
	{"ラテン文字の大文字小文字（重音符号付き）", "ÀÉÎ", "àéî", 1},
	{"ギリシャ文字の大文字小文字", "ΑΒΓ", "αβγ", 1},
	{"キリル文字の大文字小文字", "ПРИВЕТ", "привет", 1},
	{"絵文字は区切り", "\U0001F600abc", "abc", 1},
	{"CJK 拡張 B の同一", "\U00020000\U00020001", "\U00020000\U00020001", 1},
	{"CJK 拡張 B の包含", "\U00020000\U00020001", "\U00020000\U00020001\U00020002", 0.4},
	{"ASCII と全角の同じ字", "ab", "ａｂ", 0},
}

// TestSimilarityBoundaryTable は境界の表の各行について、Similarity の値が期待値と
// float32 のビット単位で一致することを確かめる。
func TestSimilarityBoundaryTable(t *testing.T) {
	t.Parallel()
	for _, tc := range similarityCases {
		got := Similarity(tc.a, tc.b)
		if math.Float32bits(got) != math.Float32bits(tc.want) {
			t.Errorf("%s: Similarity(%q, %q) = %v (bits %08x), want %v (bits %08x)",
				tc.name, tc.a, tc.b, got, math.Float32bits(got), tc.want, math.Float32bits(tc.want))
		}
	}
}

// TestIsAlnumBoundary は英数字の判定を codepoint 単位で確かめる。
// 期待値は、Postgres 17 の [[:alnum:]]（pg_trgm の語の区切りと同じ判定）を全 codepoint で
// 走査した結果に従う。Go と Postgres が食い違うのは、glibc の Unicode 版より後に
// 割り当てられた文字だけで、そのような文字は表に入れない。
func TestIsAlnumBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		r    rune
		want bool
	}{
		{"ASCII 英字", 'a', true},
		{"ASCII 数字", '7', true},
		{"ASCII 記号", '!', false},
		{"ASCII 空白", ' ', false},
		{"ASCII ハイフン", '-', false},
		{"ASCII アンダースコア", '_', false},
		{"全角英字", 'Ａ', true},
		{"全角数字", '１', true},
		{"半角カナ", 'ｱ', true},
		{"ひらがな", 'あ', true},
		{"カタカナ", 'ア', true},
		{"漢字", '全', true},
		{"Nl（ローマ数字 Ⅻ）", 'Ⅻ', true},
		{"Nl（Hangzhou 数字 一）", '〡', true},
		{"Other_Alphabetic（U+0345）", 'ͅ', true},
		{"結合文字 Mn（U+0301）", '́', false},
		{"全角空白", '　', false},
		{"中点（U+00B7）", '·', false},
		{"星（U+2606）", '☆', false},
		{"絵文字（U+1F600）", '\U0001F600', false},
		{"CJK 拡張 B", '\U00020000', true},
	}
	for _, tc := range cases {
		if got := IsAlnum(tc.r); got != tc.want {
			t.Errorf("%s: IsAlnum(U+%04X) = %v, want %v", tc.name, tc.r, got, tc.want)
		}
	}
}
