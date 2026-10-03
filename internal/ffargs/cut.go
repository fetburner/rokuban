package ffargs

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/fetburner/rokuban/internal/chapters"
)

// VAAPIDeviceArgs はソフトウェア decode を使う cut プロファイルの VAAPI 用入力側引数を返す。
//
// `hwaccel.output_format` を省略した救済経路では `-hwaccel vaapi` を使わず、
// `-vaapi_device` でデバイスを用意する。CPU の trim / filter の後ろに
// `format=nv12,hwupload` を付けて、エンコード時に HW へ上げる。
//
// h が nil または kind が vaapi でなければ nil（kind の制約は config の起動時検査が
// 持つ。ここは「vaapi ならどう出すか」だけを決める）。
func VAAPIDeviceArgs(h *HWAccel) []string {
	if h == nil || h.Kind != "vaapi" {
		return nil
	}
	return []string{"-vaapi_device", h.Device}
}

// CutFilterResult は cut プロファイルの filter_complex と、その出力を選ぶ -map 値。
type CutFilterResult struct {
	// FilterComplex は `-filter_complex` に渡す 1 本の filtergraph。
	FilterComplex string

	// VideoMap / AudioMap は `-map` に渡す出力ラベル（例: "[vout]"）。
	VideoMap string
	AudioMap string
}

// CutFilterComplex は確認済みの keep 区間でカットする filtergraph を組み立てる。
//
// 順序は trim / atrim → concat → scaler から導出した deinterlace → scale。
// ソフトウェア filter の救済経路ではその後ろに format=nv12,hwupload を付ける。
// HW decode と VAAPI scaler を使う経路では VAAPI filter が HW フレームをそのまま
// 処理するので upload は付けない。**連結した後に 1 本の連鎖へ通す**ため、区間ごとに
// フィルタが重複しない。
//
// 映像・音声とも**時刻（秒）で切る**。時刻の原点は入力の最早 start_time
// （ffmpeg が入力の先頭を 0 に揃える。チャプターの原点 = プレイヤーの currentTime と
// 同じ）。映像を trim=start_frame（最初の映像フレームから数えた番号）で切ると、
// 放送 TS のように音声が映像より先に始まる入力で、開始差ぶんだけ映像だけがずれる。
// 境界はどちらも同じフレーム番号から秒へ換算する（frame / fps）ので、区間ごとの
// A/V のずれが蓄積しない。映像の窓は半フレーム手前へずらす（[f-0.5, g-0.5) フレーム）。
// フレームがちょうど境界に載る入力（開始差が 0）でも浮動小数の丸めで 1 枚
// 出入りしないよう、窓の中に必ずフレームの中心が来る形にするため。trim の後に setpts / asetpts で
// PTS から**区間の開始時刻**を引くのは concat が各区間の先頭を 0 とみなすため
// （引かないと 2 区間目以降が元の PTS のぶん後ろへずれる）。PTS-STARTPTS（区間内の
// 最初のフレームを 0 にする）ではなく開始時刻を引くのは、音声が映像より先に
// 始まる入力で映像の最初のフレームが区間の頭より遅れているとき、その遅れを
// 区間の長さに残すため（STARTPTS だと映像の区間だけ短くなり、後続の区間が
// 音声より早く始まる）。
//
// videoStream / audioStream は入力 0 の**絶対ストリーム番号**（SelectDefaultStreams
// が返すもの）。区間は chapters.Range（原本時間軸の ms 半開区間）で、空なら nil を
// 返す（keep が空 = 全部カットは表現できない。呼び出し側が先に落とす）。
func CutFilterComplex(keep []chapters.Range, videoStream, audioStream int, scaler Scaler, height int, deinterlace, hwUpload bool) (CutFilterResult, error) {
	if len(keep) == 0 {
		return CutFilterResult{}, fmt.Errorf("cut filtergraph needs at least one keep range")
	}
	var graph strings.Builder
	var vLabels, aLabels []string
	for i, r := range keep {
		startFrame := chapters.MsToFrame(r.StartMs)
		endFrame := chapters.MsToFrame(r.EndMs)
		if r.SubFrame() {
			return CutFilterResult{}, fmt.Errorf("keep range [%d,%d) is shorter than one frame", r.StartMs, r.EndMs)
		}
		v := fmt.Sprintf("v%d", i)
		a := fmt.Sprintf("a%d", i)
		fmt.Fprintf(&graph, "[0:%d]trim=start=%s:end=%s,setpts=PTS-%s/TB[%s];",
			videoStream, frameSecondsHalfEarlier(startFrame), frameSecondsHalfEarlier(endFrame), frameSeconds(startFrame), v)
		fmt.Fprintf(&graph, "[0:%d]atrim=start=%s:end=%s,asetpts=PTS-%s/TB[%s];",
			audioStream, frameSeconds(startFrame), frameSeconds(endFrame), frameSeconds(startFrame), a)
		vLabels = append(vLabels, "["+v+"]")
		aLabels = append(aLabels, "["+a+"]")
	}
	n := strconv.Itoa(len(keep))
	fmt.Fprintf(&graph, "%sconcat=n=%s:v=1:a=0[vcat];", strings.Join(vLabels, ""), n)
	fmt.Fprintf(&graph, "%sconcat=n=%s:v=0:a=1[acat];", strings.Join(aLabels, ""), n)

	var post []string
	if filter, ok := VideoFilterArgs(scaler, height, deinterlace); ok {
		post = append(post, filter)
	}
	if hwUpload {
		post = append(post, "format=nv12", "hwupload")
	}
	if len(post) > 0 {
		fmt.Fprintf(&graph, "[vcat]%s[vout];", strings.Join(post, ","))
	} else {
		graph.WriteString("[vcat]null[vout];")
	}
	graph.WriteString("[acat]anull[aout]")

	return CutFilterResult{FilterComplex: graph.String(), VideoMap: "[vout]", AudioMap: "[aout]"}, nil
}

// frameSeconds はフレーム番号を秒へ換算する（frame / fps、fps は 30000/1001）。
// 6 桁あれば 1 フレーム（約 0.0334s）より十分細かい。
func frameSeconds(frame int64) string {
	return strconv.FormatFloat(float64(frame)*float64(chapters.FrameDenominator)/float64(chapters.FrameNumerator), 'f', 6, 64)
}

// StreamInfo はストリーム選択に要る属性だけを持つ ffprobe の観測結果。
type StreamInfo struct {
	// Index は入力 0 の中での絶対ストリーム番号。
	Index int
	// CodecType は "video" / "audio" など（ffprobe の codec_type）。
	CodecType string
	// Width / Height は映像の解像度（映像以外では 0）。
	Width, Height int
	// Channels は音声のチャンネル数（音声以外では 0）。
	Channels int
}

// SelectDefaultStreams は ffmpeg の既定のストリーム選択と同じ規則で映像 1 本・
// 音声 1 本を選ぶ。
//
// 規則（ffmpeg のドキュメントの既定）: 映像は解像度（幅×高さ）が最大のもの、
// 音声はチャンネル数が最大のもの。同点なら最も若い番号。
//
// **これを書く理由**: cut プロファイルは `-filter_complex` を使うため `-map` を
// 明示するしかない。今の encode は出力側に `-map` を指定しておらず ffmpeg の既定に
// 任せているので、同じ規則をここで再現しないとカット版だけ別のストリームが選ばれる。
// 二重音声は 1 本の音声ストリームの中にあるので、音声は 1 本で足りる。
//
// どちらかが 1 本も無ければ ok=false。
func SelectDefaultStreams(streams []StreamInfo) (video, audio int, ok bool) {
	video, audio = -1, -1
	bestPixels, bestChannels := -1, -1
	for _, s := range streams {
		switch s.CodecType {
		case "video":
			pixels := s.Width * s.Height
			if video < 0 || pixels > bestPixels {
				video, bestPixels = s.Index, pixels
			}
		case "audio":
			if audio < 0 || s.Channels > bestChannels {
				audio, bestChannels = s.Index, s.Channels
			}
		}
	}
	return video, audio, video >= 0 && audio >= 0
}

// frameSecondsHalfEarlier はフレーム番号の半フレーム手前の時刻（秒）。負にはしない。
func frameSecondsHalfEarlier(frame int64) string {
	sec := (float64(frame) - 0.5) * float64(chapters.FrameDenominator) / float64(chapters.FrameNumerator)
	return strconv.FormatFloat(max(sec, 0), 'f', 6, 64)
}
