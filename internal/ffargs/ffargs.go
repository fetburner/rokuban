// Package ffargs は VOD エンコード（internal/worker）とライブ HLS
// トランスコード（internal/streamer）が共有する ffmpeg 引数の断片を組み立てる
// 純関数だけを持つ（issue #321）。
//
// **ここには ffmpeg/ffprobe の exec は一切無い。** []string の argv 断片を返すだけで、
// 不変条件 4「ffmpeg/ffprobe の exec は worker / streamer パッケージのみ」には触れない。
//
// config.EncodeProfile と config.LiveConfig/LiveProfile が同じ HWAccel / Scaler 型を
// 持つことで、「VOD 側に hwaccel のキーを足して live に足し忘れる」が型として起きなく
// なる --- 2 つの ffmpeg 引数ビルダが同じ形の割に別々に drift する、という issue が
// 心配していた壊れ方を、命名の一致ではなく型の同一性で止める。
package ffargs

import (
	"fmt"
	"strconv"
	"strings"
)

// Scaler はスケール filter の「系統の名前」であって filter 文字列そのものではない
// （EncodeProfile の doc コメント参照。`-vf` / `video_filter` というキーは永久に
// 作らない --- filtergraph は第 2 のコマンド言語で、`cmd:` を別名で解禁したのと
// 同じになる）。
type Scaler string

// 系統名の定数。**この集合は「filter の綴りを実際に確かめた系統」に限る**
// （issue #321 決定コメント §5-2: 確かめていない系統を通すと、間違った filter 名を
// 黙って emit する方が悪い）。
const (
	// ScalerSoftware は既定の系統（空文字列もこれに正規化される）。`scale=-2:H`。
	// この環境（Homebrew ffmpeg、libx264 ビルド）で実際に `-vf scale=-2:720` /
	// `scale=-2:360` を通して確認済み。
	ScalerSoftware Scaler = "software"

	// ScalerVAAPI は Intel/AMD の VAAPI。`scale_vaapi=w=-2:h=H`。
	//
	// **この綴りはこの repo の環境では未検証。** この環境の ffmpeg
	// （Homebrew ビルド、videotoolbox のみ）は VAAPI 自体を持たず、
	// `ffmpeg -h filter=scale_vaapi` は "Unknown filter 'scale_vaapi'" を返す
	// （確認日 2026-08-15）。綴りは公開されている ffmpeg の VAAPI フィルタ構文に
	// 従っているが、実際の VAAPI 対応 ffmpeg / GPU に通した検証ではない。
	// 実機で確認した人は、確認した ffmpeg のバージョンとコマンドをここに追記すること。
	ScalerVAAPI Scaler = "vaapi"
)

// AllowedScalers は Validate が許す `scaler` の値の全集合（`""` は暗黙に
// ScalerSoftware として許可されるのでここには含めない）。
//
// **qsv / cuda は意図的に含めない。** この環境では `scale_qsv` / `scale_cuda` も
// 検証できない（`ffmpeg -h filter=scale_qsv` / `scale_cuda` はいずれも
// "Unknown filter" を返す。確認日 2026-08-15）。確認できていない綴りを黙って
// 許すより、系統ごと除外するほうが安全（不変条件 11: 書き手のいない形は決めない）。
var AllowedScalers = []Scaler{ScalerSoftware, ScalerVAAPI}

func (s Scaler) normalized() Scaler {
	if s == "" {
		return ScalerSoftware
	}
	return s
}

func (s Scaler) allowed() bool {
	for _, a := range AllowedScalers {
		if s.normalized() == a {
			return true
		}
	}
	return false
}

// ScaleArgs は height にスケールする `-vf` の filter 値と、そもそも filter を
// 出すべきか（height<=0 なら出さない）を返す。
//
// **返るのは常に filter 1 個。** 「ソフトの scale とハードの scale_vaapi を両方
// append する」コードが書ける形にしないことで、HW スケールがソフトの scale を
// 置き換えることが検査ではなく構造で保証される（issue #321 決定コメント §2-3）。
func ScaleArgs(scaler Scaler, height int) (filter string, ok bool) {
	if height <= 0 {
		return "", false
	}
	switch scaler.normalized() {
	case ScalerVAAPI:
		return fmt.Sprintf("scale_vaapi=w=-2:h=%d", height), true
	default:
		return fmt.Sprintf("scale=-2:%d", height), true
	}
}

// QualityArgs は crf / qp のどちらか一方から `-crf` / `-qp` の argv を返す。
// 両方 nil なら nil を返す。crf と qp が両方非 nil のケースは呼ばない
// （ValidateVideo が起動時に弾く。優先順位を実行時に決めさせない）。
func QualityArgs(crf, qp *int) []string {
	switch {
	case crf != nil:
		return []string{"-crf", strconv.Itoa(*crf)}
	case qp != nil:
		return []string{"-qp", strconv.Itoa(*qp)}
	default:
		return nil
	}
}

// ValidateVideo は scaler/height/crf/qp の組み合わせを検査する。VOD
// （encode.profiles）と live（live.profiles）の両方がこの 1 つの関数を通る ---
// 片側だけ直る事故を型と関数で塞ぐ（issue #321 決定コメント §5）。
func ValidateVideo(scaler Scaler, height int, crf, qp *int) error {
	var errs []string
	if crf != nil && qp != nil {
		errs = append(errs, "crf and qp must not both be set (priority between them is not defined)")
	}
	if crf != nil && *crf < 0 {
		errs = append(errs, fmt.Sprintf("crf must be >= 0, got %d", *crf))
	}
	if qp != nil && *qp < 0 {
		errs = append(errs, fmt.Sprintf("qp must be >= 0, got %d", *qp))
	}
	if scaler != "" {
		if !scaler.allowed() {
			names := make([]string, len(AllowedScalers))
			for i, a := range AllowedScalers {
				names[i] = string(a)
			}
			errs = append(errs, fmt.Sprintf("scaler must be one of [%s], got %q", strings.Join(names, ", "), scaler))
		} else if height <= 0 {
			errs = append(errs, fmt.Sprintf("scaler %q requires height > 0", scaler))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// HWAccel は `-i` より前に出す唯一のブロック（issue #321 決定コメント §1）。
//
// **ネストしたブロックにし、`hwaccel_kind` のようなフラット 3 本にしない。**
// ブロックの存在そのものが「`-i` の前に出す」という主張になる（不変条件 10:
// 意味を持たない行を作らない、と同じ形）。フラットな 3 本だと「device だけ書いた」
// 状態が「何も書いていない」と区別できず、掃除する規則が要る。ポインタなので
// `hwaccel:`（値なし）は nil、`hwaccel: {}` は「書いた」ことになり Validate が
// `kind is required` で落とす（goccy/go-yaml が null をポインタの nil に
// デコードすることに乗った挙動。config.TestLoad_EncodeProfileHWAccel が固定している）。
//
// **device の存在は Validate で検査しない。** 公式イメージや device の無い CI が
// 落ちる。無い device を書いたプロファイルはジョブ / セッションの失敗として現れる
// （マウントは k8s `resources.limits` / Docker `--device` の話でこの型の外）。
type HWAccel struct {
	// Kind は `-hwaccel` に渡す値（例: vaapi）。ブロックがあれば必須。
	Kind string `yaml:"kind"`

	// Device は `-hwaccel_device` に渡すパス（例: /dev/dri/renderD128）。任意。
	Device string `yaml:"device"`

	// OutputFormat は `-hwaccel_output_format` に渡す値。任意。
	OutputFormat string `yaml:"output_format"`
}

// Args は `-hwaccel`/`-hwaccel_device`/`-hwaccel_output_format` を kind → device →
// output_format の順で返す。h が nil なら何も返さない（ブロックを書かなければ
// 何も出ない、が構造で保証される）。
func (h *HWAccel) Args() []string {
	if h == nil {
		return nil
	}
	args := []string{"-hwaccel", h.Kind}
	if h.Device != "" {
		args = append(args, "-hwaccel_device", h.Device)
	}
	if h.OutputFormat != "" {
		args = append(args, "-hwaccel_output_format", h.OutputFormat)
	}
	return args
}

// Validate はブロック自体の妥当性を検査する。h が nil なら nil（ブロックを
// 書いていない設定は検査対象外）。
func (h *HWAccel) Validate() error {
	if h == nil {
		return nil
	}
	var errs []string
	if h.Kind == "" {
		errs = append(errs, "hwaccel.kind is required")
	}
	// ブロック経由でフラグを密輸させない（例: kind: "-y"）。
	for _, f := range []struct {
		name, val string
	}{
		{"kind", h.Kind},
		{"device", h.Device},
		{"output_format", h.OutputFormat},
	} {
		if strings.HasPrefix(f.val, "-") {
			errs = append(errs, fmt.Sprintf("hwaccel.%s must not start with '-', got %q", f.name, f.val))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// PreInput は `-i` より前に出す argv 断片を、hwaccel ブロック（kind → device →
// output_format）に続けて input 側の extra args を並べた順で返す。
func PreInput(hw *HWAccel, inputExtraArgs []string) []string {
	args := hw.Args()
	args = append(args, inputExtraArgs...)
	return args
}

// extraArgTakesValue は extra_args / input_extra_args に書ける ffmpeg
// オプションと、そのオプションが直後の 1 トークンを値として消費するかを表す。
// 値を取らないフラグも明示することで、`-an /tmp/evil.mp4` のように 2 本目の
// 出力パスを boolean フラグの値に見せかけることを防ぐ。
var extraArgTakesValue = map[string]bool{
	"-an": false, "-vn": false, "-sn": false, "-dn": false,
	"-shortest": false, "-nostdin": false, "-re": false,
	"-movflags": true, "-map": true,
	"-global_quality": true, "-cq": true, "-q:v": true,
	"-b:v": true, "-b:a": true,
	"-probesize": true, "-analyzeduration": true, "-extra_hw_frames": true,
}

// ValidateExtraArgs は args が許可済みオプションと、そのオプションが要求する値の
// 組だけで構成されていることを検査する。未知のオプションと裸の位置引数は拒否する。
// 見つかった問題は全件 1 つのエラーにまとめて返す（規約 4）。label はエラー
// メッセージの接頭辞（"extra_args" 等）。
func ValidateExtraArgs(label string, args []string) error {
	var errs []string
	for i := 0; i < len(args); {
		takesValue, ok := extraArgTakesValue[args[i]]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s[%d]: %q is not an allowed option", label, i, args[i]))
			i++
			continue
		}
		if !takesValue {
			i++
			continue
		}
		if i+1 == len(args) {
			errs = append(errs, fmt.Sprintf("%s[%d]: %q requires a value", label, i, args[i]))
			i++
			continue
		}
		i += 2
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
