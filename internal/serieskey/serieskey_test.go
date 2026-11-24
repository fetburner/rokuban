package serieskey

import "testing"

// cases は境界の表である。want と ok の期待値はすべて postgres:17.12 の series_key() で
// 求めた（ok が false の行は SQL の NULL）。
// 全角空白・結合文字・見分けにくい記号は Go のエスケープで書く。
// comparison テストも同じ入力を Postgres と突き合わせる。
var cases = []struct {
	in   string
	want string
	ok   bool
}{
	// 括弧の除去（段 b）
	{"[字幕]番組", "番組", true},
	{"番組[HD]", "番組", true},
	{"[a]b[c]d", "bd", true},
	{"【新】番組", "番組", true},
	{"番組【終】", "番組", true},
	{"【x】[y]題名", "題名", true},
	{"[]", "", false},
	{"【】", "", false},
	{"[未閉じ", "[未閉じ", true},
	{"【未閉じ", "【未閉じ", true},
	{"番組[x", "番組[x", true},
	{"番組 [x]", "番組", true},
	{"[x]▽番組", "番組", true},

	// 先頭の記号の除去と、記号での切り捨て（段 c）
	{"▽番組", "番組", true},
	{"★☆番組★", "番組", true},
	{"「番組」", "番組", true},
	{"『番組』", "番組", true},
	{"〜番組", "番組", true},
	{"番組▽続き", "番組", true},
	{"番組「一」", "番組", true},
	{"※注記", "注記", true},
	{"#タイトル", "タイトル", true},
	{"|題名|", "題名", true},
	{"◆■●○◎", "", false},
	{"番組◆後ろ", "番組", true},
	{"A#B", "A", true},
	{"番組」", "番組", true},
	{"番組』", "番組", true},

	// 話数の除去（段 d）。4 つの選択肢のそれぞれ
	{"番組 第3話", "番組", true},
	{"番組第12回", "番組", true},
	{"番組第5週", "番組", true},
	{"番組 #7", "番組", true},
	{"番組(3)", "番組", true},
	{"番組 12話", "番組", true},
	{"番組第12", "番組第12", true},
	{"番組(a)", "番組(a)", true},
	{"番組 第話", "番組", true},
	{"番組#", "番組", true},
	{"番組第1話第2話", "番組", true},
	{"3話", "", false},
	{"第1話", "", false},
	{"番組 #1 続き", "番組", true},

	// 枠名（段 e）と、最初の空白での切り捨て
	{"アニメ 番組", "番組", true},
	{"TVアニメ 番組", "番組", true},
	{"日5 番組", "番組", true},
	{"アニメ番組", "アニメ番組", true},
	{"アニメ  番組", "番組", true},
	{"アニメ 番組 続き", "番組", true},
	{"TVアニメ", "TVアニメ", true},
	{"アニメ 。", "", false},
	{"  番組  ", "", false},
	{"番組\u3000続き", "番組", true},

	// 末尾と先頭の句読点（btrim）
	{"番組。", "番組", true},
	{"番組、", "番組", true},
	{"番組・", "番組", true},
	{"番組!", "番組", true},
	{"番組?", "番組", true},
	{"、番組", "番組", true},
	{"番組：", "番組", true},
	{"番組；", "番組", true},
	{"番組 、", "番組", true},
	{". 番組 .", "", false},

	// 英数字の無い題名は NULL
	{"", "", false},
	{"。", "", false},
	{"(", "", false},
	{")", "", false},
	{"...", "", false},

	// NFKC に依存する行。全角英数・全角空白・半角カナ・合成
	{"＃１２３", "123", true},
	{"ＡＢＣ１２３", "ABC123", true},
	{"Ｈｅｌｌｏ\u3000Ｗｏｒｌｄ", "Hello", true},
	{"Ｓｐｅｃｉａｌ\u3000番組 第3話 【新】", "Special", true},
	{"ｶﾀｶﾅ", "カタカナ", true},
	{"ﾊﾟｰﾃｨｰ", "パーティー", true},
	{"（全角）", "(全角)", true},
	{"㈱番組", "(株)番組", true},
	{"\u216b", "XII", true},
	{"\u2081", "1", true},
	// 合成済みの「が」（U+304C）と、「か」+ 結合濁点（U+3099）。どちらも「が」になる
	{"\u304c", "\u304c", true},
	{"\u304b\u3099", "\u304c", true},

	// ASCII 以外の英数字は残る（trgm.IsAlnum の判定）
	{"ÉCOLE", "ÉCOLE", true},
	{"ß", "ß", true},
	{"Ω", "Ω", true},
	{"한국", "한국", true},
	{"中文", "中文", true},
	{"\u0663", "\u0663", true},
	{"\u05b0", "\u05b0", true},
}

// TestSeriesKey は境界の表の各行で、Go の値が期待値（Postgres の値）と一致することを確かめる。
func TestSeriesKey(t *testing.T) {
	for _, c := range cases {
		got, ok := SeriesKey(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("SeriesKey(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
