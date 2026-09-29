package chapters

import (
	"strings"
	"testing"
)

// TestKeepRanges_MergesAdjacentAndDropsCut は「cut でない区間を結合して返す」
// ことを固定する。結合しないと、隙間なく覆っているタイムラインが 1 フレーム
// ごとの区間列になり、filtergraph が数千本の concat になる。
func TestKeepRanges_MergesAdjacentAndDropsCut(t *testing.T) {
	timeline := Timeline{
		{StartMs: 0, EndMs: 100},
		{StartMs: 100, EndMs: 200},
		{StartMs: 200, EndMs: 300, Label: LabelCM, Cut: true},
		{StartMs: 300, EndMs: 400},
	}
	got := KeepRanges(timeline)
	want := []Range{{StartMs: 0, EndMs: 200}, {StartMs: 300, EndMs: 400}}
	if !SameRanges(got, want) {
		t.Errorf("KeepRanges = %v, want %v", got, want)
	}

	// 空のタイムライン（全部カット）は空を返す。呼び出し側はこれを見て
	// 「カット版は作れない」と判断する。
	if got := KeepRanges(Timeline{{StartMs: 0, EndMs: 100, Cut: true}}); len(got) != 0 {
		t.Errorf("all-cut timeline = %v, want empty", got)
	}
}

// TestDeriveThenKeepRanges_OnlyCutSpansFall は「ユーザー層の cut=true だけが
// 落ちる」ことを、Derive を通した形で固定する（実際の経路は
// Derive → KeepRanges の 2 段。片方だけを見るテストは導出の抜けを見逃す）。
func TestDeriveThenKeepRanges_OnlyCutSpansFall(t *testing.T) {
	user := []Span{
		{StartMs: 0, EndMs: 30000, Label: "OP", Cut: true},
		{StartMs: 60000, EndMs: 90000, Label: LabelCM, Cut: true},
	}
	got := KeepRanges(Derive(true, user, nil, 120000))
	// 境界は Derive がフレーム境界へ量子化するので、期待値も同じ関数で作る
	// （リテラルで書くと量子化の粒度を変えたときに意味が変わる）。フレーム
	// レート 30000/1001 で 1 フレーム ≈ 33.4ms ずれる。
	want := []Range{
		{StartMs: QuantizeMs(30000), EndMs: QuantizeMs(60000)},
		{StartMs: QuantizeMs(90000), EndMs: 120000},
	}
	// 末尾は duration のまま（量子化しない）なので、後ろの境界だけは素の値。
	if !SameRanges(got, want) {
		t.Errorf("keep = %v, want %v", got, want)
	}
}

// TestSameRanges は一致判定が長さ・境界の両方を見ることを固定する。
// 「編集前の内容です」の判定がこれに乗っているので、片方だけ見る実装だと
// 区間が 1 本増えた編集を見逃す。
func TestSameRanges(t *testing.T) {
	a := []Range{{StartMs: 0, EndMs: 100}}
	cases := []struct {
		name string
		b    []Range
		want bool
	}{
		{"identical", []Range{{StartMs: 0, EndMs: 100}}, true},
		// 量子化前の値（1ms 違い）で比べると落ちること。これを壊すと
		// 「量子化後の値どうしで比べる」という判定が効かなくなる。
		{"one millisecond off", []Range{{StartMs: 0, EndMs: 101}}, false},
		{"shorter", nil, false},
		{"one more range", []Range{{StartMs: 0, EndMs: 100}, {StartMs: 200, EndMs: 300}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameRanges(a, tc.b); got != tc.want {
				t.Errorf("SameRanges = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMapMs はカット後の時間軸への写像を固定する。区間の外側は直後の keep 先頭へ
// 丸める（字幕の境界がここに落ちる）。
func TestMapMs(t *testing.T) {
	keep := []Range{{StartMs: 1000, EndMs: 2000}, {StartMs: 5000, EndMs: 6000}}
	cases := []struct {
		in   int64
		want int64
	}{
		{0, 0},    // 最初の keep より前 → 先頭
		{1000, 0}, // keep の先頭
		{1500, 500},
		{2000, 1000}, // 1 本目の終端 = 2 本目の手前の写像先
		{3000, 1000}, // カット区間の中 → 次の keep の先頭
		{5500, 1500},
		{6000, 2000},
		{9999, 2000}, // 末尾より後ろ → 末尾
	}
	for _, tc := range cases {
		if got := MapMs(keep, tc.in); got != tc.want {
			t.Errorf("MapMs(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestRetimeVTT_MovesClipsAndDrops は字幕の付け替えを固定する。CM の中に収まる
// キューは捨て、境界をまたぐキューは keep 側でクリップする。
func TestRetimeVTT_MovesClipsAndDrops(t *testing.T) {
	in := strings.Join([]string{
		"WEBVTT",
		"",
		"00:00:00.500 --> 00:00:01.500",
		"keep の中",
		"",
		"00:00:02.000 --> 00:00:04.000",
		"CM の中",
		"",
		"00:00:01.500 --> 00:00:03.000",
		"境界をまたぐ",
		"",
		"00:00:05.500 --> 00:00:05.900",
		"2 本目の keep",
		"",
	}, "\n")
	// 原本 0..2 秒 と 5..6 秒が keep。2..5 秒がカット。
	keep := []Range{{StartMs: 0, EndMs: 2000}, {StartMs: 5000, EndMs: 6000}}

	got, err := RetimeVTT([]byte(in), keep)
	if err != nil {
		t.Fatalf("RetimeVTT: %v", err)
	}
	out := string(got)
	t.Logf("retimed:\n%s", out)

	// 1 本目: 0.5-1.5 秒 → そのまま。
	if !strings.Contains(out, "00:00:00.500 --> 00:00:01.500") {
		t.Errorf("first cue was not preserved:\n%s", out)
	}
	// CM の中のキューは消える。
	if strings.Contains(out, "CM の中") {
		t.Errorf("cue inside a cut range survived:\n%s", out)
	}
	// 1.5-3.0 秒は keep の 1.5-2.0 秒へクリップされ、カット後は 1.5-2.0 秒。
	if !strings.Contains(out, "00:00:01.500 --> 00:00:02.000") {
		t.Errorf("straddling cue was not clipped to the keep range:\n%s", out)
	}
	if !strings.Contains(out, "境界をまたぐ") {
		t.Errorf("straddling cue text was dropped:\n%s", out)
	}
	// 5.5-5.9 秒 → keep の先頭 2 秒ぶん（0-2 秒が残る）手前へ写って 2.5-2.9 秒。
	if !strings.Contains(out, "00:00:02.500 --> 00:00:02.900") {
		t.Errorf("second keep range cue was not shifted:\n%s", out)
	}
	// ヘッダは残る。
	if !strings.HasPrefix(out, "WEBVTT\n") {
		t.Errorf("header was lost:\n%s", out)
	}
}

// TestRetimeVTT_AllCuesCutKeepsHeader は全キューが CM の中でも壊れないことを
// 固定する（呼び出し側はサイズ検査をするので、空の VTT でも「WEBVTT」は要る）。
func TestRetimeVTT_AllCuesCutKeepsHeader(t *testing.T) {
	in := "WEBVTT\n\n00:00:02.000 --> 00:00:03.000\nCM\n"
	got, err := RetimeVTT([]byte(in), []Range{{StartMs: 0, EndMs: 1000}})
	if err != nil {
		t.Fatalf("RetimeVTT: %v", err)
	}
	if !strings.HasPrefix(string(got), "WEBVTT") {
		t.Errorf("header was lost: %q", got)
	}
	if strings.Contains(string(got), "CM") {
		t.Errorf("cue survived although it is outside every keep range: %q", got)
	}
}
