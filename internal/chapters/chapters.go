// Package chapters は原本の時間軸上のチャプター区間を扱う純関数をまとめる。
//
// 時間軸の単位は**原本の最初の映像フレームを 0 とする ms**（決定済み）。値は
// JLSE のフレーム番号から ms = round(frame × 1001 / 30) で作り、フレーム番号へは
// frame = round(ms × 30 / 1001) で戻す。1 フレーム ≈ 33.4ms なので往復で誤差は
// 出ない。フレームレートは 30000/1001 で固定する（地上波・BS の SD / HD はすべて
// この値）。ドロップで PTS が飛んだ録画では数フレームずれうるが、既知の制約として
// 受け入れる（スキップにも修正 UI にも害がない）。
//
// **自動層（検出結果）とユーザー層で導出を分岐させない。** 有効なタイムラインを
// 作るのは Derive 1 か所だけで、プレイヤーの描画（api → web）とカット版の encode
// が同じ関数を通る。TS 側に複製しない（シークタイルの固定値が Go と TS の 2 か所に
// あった轍を踏まない）。
package chapters

import (
	"errors"
	"fmt"
	"sort"
)

const (
	// FrameNumerator / FrameDenominator は固定のフレームレート 30000/1001。
	FrameNumerator   int64 = 30000
	FrameDenominator int64 = 1001

	// LabelCM は自動検出の CM 区間に付くラベル。引き取りでユーザー層へ複製される
	// ときも同じ値を使う（ユーザーは自由に書き換えられる）。
	LabelCM = "CM"
)

// ErrInvalid は入力スパン列が受け付けられないことを表す。API はこれを 400 に写す。
var ErrInvalid = errors.New("invalid chapter spans")

// Span は原本の時間軸上の半開区間 [StartMs, EndMs)。Label が空文字なのは
// 「ラベル無し」で、そのときは Cut が真でなければならない（意味を持たない行を
// 作らない。DB の CHECK と同じ規則）。
type Span struct {
	StartMs int64  `json:"startMs"`
	EndMs   int64  `json:"endMs"`
	Label   string `json:"label"`
	Cut     bool   `json:"cut"`
}

// Range は ms の半開区間 [StartMs, EndMs)。
type Range struct {
	StartMs int64 `json:"startMs"`
	EndMs   int64 `json:"endMs"`
}

// Timeline は原本の時間軸を隙間なく覆うスパン列（昇順）。本編の区間は
// Label が空で Cut が偽。
type Timeline []Span

// MsToFrame は ms を最も近いフレーム番号へ丸める。
func MsToFrame(ms int64) int64 {
	if ms <= 0 {
		return 0
	}
	return (ms*FrameNumerator/1000 + FrameDenominator/2) / FrameDenominator
}

// FrameToMs はフレーム番号を ms へ写す（丸め込みあり）。
func FrameToMs(frame int64) int64 {
	if frame <= 0 {
		return 0
	}
	return (frame*FrameDenominator + FrameNumerator/2000) / (FrameNumerator / 1000)
}

// QuantizeMs は ms を最も近いフレーム境界へ丸める。ユーザーが入れた境界も、
// 検出器が返した境界も、保存・導出の前にここを通す。
func QuantizeMs(ms int64) int64 { return FrameToMs(MsToFrame(ms)) }

// AutoSpans は CM 区間（ms、半開区間）から自動層のスパン列を作る。
//
// **CM 率が 50% を超える検出は「CM 無し」として扱う。** 検出が壊れると本編の
// 半分以上を CM と主張する結果が返り、プレイヤーが本編を飛ばし、カット版が本編を
// 削ってしまう。判定を読む側（ここ）に置くので、引き取りでユーザー層へ複製される
// ときもこの扱いのままになる。
//
// 率の分母は EPG の尺ではなく「EPG の尺と検出区間の終端の大きい方」を使う。録画は
// EIT 追従で EPG より延長されうるので、program_duration_ms で割ると延長された録画の
// 正常な検出まで 50% を超えて見える。
func AutoSpans(cmRanges []Range, programDurationMs int64) []Span {
	var cutMs, length int64
	length = programDurationMs
	for _, r := range cmRanges {
		if r.EndMs > r.StartMs {
			cutMs += r.EndMs - r.StartMs
		}
		if r.EndMs > length {
			length = r.EndMs
		}
	}
	if length <= 0 || cutMs*2 > length {
		return nil
	}
	spans := make([]Span, 0, len(cmRanges))
	for _, r := range cmRanges {
		if r.EndMs <= r.StartMs {
			continue
		}
		spans = append(spans, Span{StartMs: r.StartMs, EndMs: r.EndMs, Label: LabelCM, Cut: true})
	}
	// 検出器の境界もフレーム境界へ丸める。丸めた結果として空になった区間は落とす。
	return Quantized(spans)
}

// Derive は有効なタイムラインを導出する。
//
// 所有していれば（owned）ユーザー層を、していなければ自動層を使う。どの区間にも
// 属さない時間は本編（Label 空 / Cut 偽）で埋める。境界はすべてフレーム境界へ
// 量子化する。
//
// end は programDurationMs と既知の区間の終端の大きい方にする。ユーザーが入れた
// 区間が EPG の尺を越えることは正当（EIT 追従で延長された録画）なので、そこで
// 打ち切らない。
func Derive(owned bool, user, auto []Span, programDurationMs int64) Timeline {
	known := auto
	if owned {
		known = user
	}
	spans := Quantized(known)
	end := programDurationMs
	if len(spans) > 0 && spans[len(spans)-1].EndMs > end {
		end = spans[len(spans)-1].EndMs
	}
	if end < 0 {
		end = 0
	}

	out := make(Timeline, 0, len(spans)*2+1)
	cursor := int64(0)
	for _, s := range spans {
		start := max(s.StartMs, cursor)
		stop := max(s.EndMs, start)
		if start > cursor {
			out = append(out, Span{StartMs: cursor, EndMs: start})
		}
		if stop > start {
			out = append(out, Span{StartMs: start, EndMs: stop, Label: s.Label, Cut: s.Cut})
			cursor = stop
		}
	}
	if cursor < end {
		out = append(out, Span{StartMs: cursor, EndMs: end})
	}
	return out
}

// KeepRanges はタイムラインのうち cut でない区間を、隣接するものを結合して返す。
//
// カット版が残す区間そのもの。タイムラインは隙間なく覆っている（Derive）ので、
// 結合しないと本編が 1 フレームごとに分かれた区間列になりうる。
//
// 戻り値は昇順で重ならない半開区間（原本時間軸の ms）。空なら「全部カット」で、
// カット版は作れない（呼び出し側が先に落とす）。
func KeepRanges(t Timeline) []Range {
	var out []Range
	for _, s := range t {
		if s.Cut || s.EndMs <= s.StartMs {
			continue
		}
		if n := len(out); n > 0 && out[n-1].EndMs == s.StartMs {
			out[n-1].EndMs = s.EndMs
			continue
		}
		out = append(out, Range{StartMs: s.StartMs, EndMs: s.EndMs})
	}
	return out
}

// SameRanges は 2 つの区間列が同じ時間軸の被覆を表すかを返す（量子化後の値
// どうしで比べる前提。丸めは呼び出し側の責任）。
//
// 「編集前の内容です」の判定がこれを使う。凍結した keep_ranges と現在の
// タイムラインから導出した keep を比べるので、**判定は保存値ではなく毎回の
// 導出になる**（不変条件 9）。
func SameRanges(a, b []Range) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// MapMs は原本時間軸の時刻 t を、keep 区間を残したカット後の時間軸へ写す。
// keep の外側の時刻は直後の keep 区間の先頭（末尾より後ろなら末尾）へ丸める。
func MapMs(keep []Range, t int64) int64 {
	var out int64
	for _, r := range keep {
		if t < r.StartMs {
			return out
		}
		if t >= r.EndMs {
			out += r.EndMs - r.StartMs
			continue
		}
		return out + (t - r.StartMs)
	}
	return out
}

// quantizeSpan は 1 つのスパンの境界をフレーム境界へ丸める。
func quantizeSpan(s Span) Span {
	return Span{
		StartMs: QuantizeMs(s.StartMs),
		EndMs:   QuantizeMs(s.EndMs),
		Label:   s.Label,
		Cut:     s.Cut,
	}
}

// Quantized はスパン列の境界をフレーム境界へ丸め、空になった区間を落として
// 昇順に並べる。量子化は単調なので、重ならない入力は重ならないまま残る。
func Quantized(spans []Span) []Span {
	out := make([]Span, 0, len(spans))
	for _, s := range spans {
		q := quantizeSpan(s)
		if q.EndMs <= q.StartMs {
			continue
		}
		out = append(out, q)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartMs < out[j].StartMs })
	return out
}

// Validate は API が受け取ったユーザー層を検証し、保存できる形（量子化済み・
// 昇順）にして返す。
//
// 拒否するのは次の 3 つ。空の区間、重なる区間、ラベルも無く cut でもない区間。
// いずれも DB の CHECK / EXCLUDE が拒否する組み合わせで、ここで先に弾いて 400 に
// するためのもの（DB 側の制約は直接 INSERT が来ても壊れないようにする二重化）。
func Validate(spans []Span) ([]Span, error) {
	for _, s := range spans {
		if s.Label == "" && !s.Cut {
			return nil, fmt.Errorf("%w: span [%d,%d) has neither a label nor cut=true", ErrInvalid, s.StartMs, s.EndMs)
		}
		if q := quantizeSpan(s); q.EndMs <= q.StartMs {
			return nil, fmt.Errorf("%w: span [%d,%d) is empty", ErrInvalid, s.StartMs, s.EndMs)
		}
	}
	quantized := Quantized(spans)
	for i := 1; i < len(quantized); i++ {
		if quantized[i].StartMs < quantized[i-1].EndMs {
			return nil, fmt.Errorf("%w: span [%d,%d) overlaps [%d,%d)",
				ErrInvalid, quantized[i].StartMs, quantized[i].EndMs,
				quantized[i-1].StartMs, quantized[i-1].EndMs)
		}
	}
	return quantized, nil
}
