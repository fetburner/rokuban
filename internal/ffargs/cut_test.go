package ffargs

import (
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
)

// TestCutFilterComplex_OrderAndBoundaries は filtergraph の順序（trim → concat →
// deinterlace → scale → hwupload）と、音声の境界が映像と同じフレーム番号から
// 出ていることを固定する。**音声を ms で切ると区間ごとに半フレームぶんずれ、
// 5 区間で 1 フレームを超える**（受け入れの「A/V のずれの累積が 1 フレーム以内」）。
func TestCutFilterComplex_OrderAndBoundaries(t *testing.T) {
	keep := []chapters.Range{{StartMs: 0, EndMs: 1000}, {StartMs: 2000, EndMs: 3000}}
	got, err := CutFilterComplex(keep, 0, 1, ScalerSoftware, 720, true, true)
	if err != nil {
		t.Fatalf("CutFilterComplex: %v", err)
	}
	graph := got.FilterComplex
	t.Logf("filtergraph: %s", graph)

	start1 := chapters.MsToFrame(2000)
	end1 := chapters.MsToFrame(3000)
	// 映像は frames、音声は同じフレーム番号から換算した秒。
	if !strings.Contains(graph, "[0:0]trim=start_frame=0:end_frame=") {
		t.Errorf("first video segment is not a frame trim: %s", graph)
	}
	wantAudio := "atrim=start=" + frameSeconds(start1) + ":end=" + frameSeconds(end1)
	if !strings.Contains(graph, wantAudio) {
		t.Errorf("audio boundaries are not derived from the same frames (%q missing): %s", wantAudio, graph)
	}
	// concat のあとに deinterlace → scale → hwupload が 1 本の連鎖で続く。
	if !strings.Contains(graph, "[vcat]yadif,scale=-2:720,format=nv12,hwupload[vout]") {
		t.Errorf("post-concat chain is not deinterlace,scale,hwupload in that order: %s", graph)
	}
	if got.VideoMap != "[vout]" || got.AudioMap != "[aout]" {
		t.Errorf("maps = %q/%q, want [vout]/[aout]", got.VideoMap, got.AudioMap)
	}

	// deinterlace 無し・hwupload 無しなら scale だけ。
	plain, err := CutFilterComplex(keep, 0, 1, ScalerSoftware, 0, false, false)
	if err != nil {
		t.Fatalf("CutFilterComplex (plain): %v", err)
	}
	if !strings.Contains(plain.FilterComplex, "[vcat]null[vout]") {
		t.Errorf("no post filters should still terminate the video chain: %s", plain.FilterComplex)
	}
}

// TestCutFilterComplex_RejectsUnusableInput は filtergraph を作れない入力を
// 落とすことを固定する（keep が空 = 全部カット、1 フレーム未満の区間）。
func TestCutFilterComplex_RejectsUnusableInput(t *testing.T) {
	if _, err := CutFilterComplex(nil, 0, 1, ScalerSoftware, 0, false, false); err == nil {
		t.Error("empty keep ranges must be rejected")
	}
	if _, err := CutFilterComplex([]chapters.Range{{StartMs: 0, EndMs: 1}}, 0, 1, ScalerSoftware, 0, false, false); err == nil {
		t.Error("a keep range shorter than one frame must be rejected")
	}
}

// TestSelectDefaultStreams は ffmpeg の既定のストリーム選択の再現を固定する。
// **カット版とカットでない版で選ばれるストリームが一致する**ことが目的なので、
// 規則そのもの（映像は最大解像度、音声は最大チャンネル数、同点は若い番号）を
// リテラルで押さえる。
func TestSelectDefaultStreams(t *testing.T) {
	streams := []StreamInfo{
		{Index: 0, CodecType: "video", Width: 1440, Height: 1080},
		{Index: 1, CodecType: "audio", Channels: 2},
		{Index: 2, CodecType: "audio", Channels: 1},
		{Index: 3, CodecType: "video", Width: 720, Height: 480},
	}
	video, audio, ok := SelectDefaultStreams(streams)
	if !ok || video != 0 || audio != 1 {
		t.Errorf("SelectDefaultStreams = (%d, %d, %v), want (0, 1, true)", video, audio, ok)
	}

	// 同点は若い番号（ffmpeg の既定と同じ）。
	tie := []StreamInfo{
		{Index: 0, CodecType: "video", Width: 1920, Height: 1080},
		{Index: 1, CodecType: "audio", Channels: 2},
		{Index: 2, CodecType: "audio", Channels: 2},
	}
	if _, audio, ok := SelectDefaultStreams(tie); !ok || audio != 1 {
		t.Errorf("tie on channel count picked %d, want the lowest index (1)", audio)
	}

	// 字幕だけ、映像だけでは足りない。
	if _, _, ok := SelectDefaultStreams([]StreamInfo{{Index: 0, CodecType: "subtitle"}}); ok {
		t.Error("a stream list without video and audio must not report ok")
	}
}

// TestVAAPIDeviceArgs は cut プロファイルの入力側が `-hwaccel` ではなく
// `-vaapi_device` になることを固定する。**HW デコードしたフレームは trim に
// 通せない**ので、ここが `-hwaccel vaapi` に戻ると filtergraph が壊れる。
func TestVAAPIDeviceArgs(t *testing.T) {
	got := VAAPIDeviceArgs(&HWAccel{Kind: "vaapi", Device: "/dev/dri/renderD128"})
	want := []string{"-vaapi_device", "/dev/dri/renderD128"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("VAAPIDeviceArgs = %v, want %v", got, want)
	}
	if got := VAAPIDeviceArgs(nil); got != nil {
		t.Errorf("VAAPIDeviceArgs(nil) = %v, want nil", got)
	}
	if got := VAAPIDeviceArgs(&HWAccel{Kind: "cuda"}); got != nil {
		t.Errorf("VAAPIDeviceArgs(cuda) = %v, want nil (config rejects that combination)", got)
	}
}
