package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/ffargs"
)

// TestConvertLiveConfig_NoFieldLeftBehind は convertLiveConfig の取りこぼしを
// 検出する。config.LiveConfig / config.LiveProfile の全フィールドを非ゼロ値に
// 埋めて変換し、streamer.LiveConfig / streamer.LiveProfile 側にゼロ値の
// フィールドが 1 つも残っていないことを reflect で主張する。
//
// **フィールドを 1 本忘れても go build は通る**（streamer.LiveConfig は
// internal/config を import しない別構造体なので、変換の取りこぼしはコンパイル
// エラーにならず黙って消える）。壊し方: Scaler の代入行を消す。
func TestConvertLiveConfig_NoFieldLeftBehind(t *testing.T) {
	crf := 23
	qp := 24
	src := config.LiveConfig{
		Enabled:       true,
		FFmpeg:        "ffmpeg",
		SegmentDir:    "/dev/shm/rokuban-live",
		MaxSessions:   4,
		IdleTimeout:   30 * time.Second,
		TunerPriority: 1,
		HWAccel: &ffargs.HWAccel{
			Kind:         "vaapi",
			Device:       "/dev/dri/renderD128",
			OutputFormat: "vaapi",
		},
		InputExtraArgs: []string{"-re"},
		Profiles: []config.LiveProfile{
			{
				Name:           "h264_vaapi",
				VideoCodec:     "h264_vaapi",
				AudioCodec:     "aac",
				Height:         720,
				Scaler:         ffargs.ScalerVAAPI,
				CRF:            &crf,
				QP:             &qp,
				Preset:         "veryfast",
				SegmentSeconds: 2,
				PlaylistSize:   6,
				ExtraArgs:      []string{"-b:v", "2M"},
			},
		},
	}

	got := convertLiveConfig(src)

	assertNoZeroFields(t, "streamer.LiveConfig", reflect.ValueOf(got))
	if len(got.Profiles) != 1 {
		t.Fatalf("Profiles len = %d, want 1", len(got.Profiles))
	}
	assertNoZeroFields(t, "streamer.LiveProfile", reflect.ValueOf(got.Profiles[0]))
}

// assertNoZeroFields はエクスポートされた struct フィールドをすべて走査し、
// ゼロ値のままのフィールドがあれば t.Errorf で報告する。
func assertNoZeroFields(t *testing.T, label string, v reflect.Value) {
	t.Helper()
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		fv := v.Field(i)
		if fv.IsZero() {
			t.Errorf("%s.%s was left at its zero value (convertLiveConfig dropped this field)", label, field.Name)
		}
	}
}
