package ffargs

import (
	"slices"
	"strings"
	"testing"
)

// TestScaleArgs は期待値をリテラルで書く（実装の表と比較しない。テスト規律）。
func TestScaleArgs(t *testing.T) {
	cases := []struct {
		name       string
		scaler     Scaler
		height     int
		wantFilter string
		wantOK     bool
	}{
		{"software height 720", ScalerSoftware, 720, "scale=-2:720", true},
		{"empty scaler defaults to software", "", 360, "scale=-2:360", true},
		{"vaapi height 720", ScalerVAAPI, 720, "scale_vaapi=w=-2:h=720", true},
		{"height zero omits filter regardless of scaler", ScalerVAAPI, 0, "", false},
		{"negative height omits filter", ScalerSoftware, -1, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			filter, ok := ScaleArgs(c.scaler, c.height)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if filter != c.wantFilter {
				t.Errorf("filter = %q, want %q", filter, c.wantFilter)
			}
		})
	}
}

// TestQualityArgs_Literal は crf/qp の排他マッピングをリテラルで固定する。
func TestQualityArgs(t *testing.T) {
	crf := 23
	qp := 24
	if got := QualityArgs(&crf, nil); !slices.Equal(got, []string{"-crf", "23"}) {
		t.Errorf("crf only = %v, want [-crf 23]", got)
	}
	if got := QualityArgs(nil, &qp); !slices.Equal(got, []string{"-qp", "24"}) {
		t.Errorf("qp only = %v, want [-qp 24]", got)
	}
	if got := QualityArgs(nil, nil); got != nil {
		t.Errorf("neither = %v, want nil", got)
	}
}

func TestValidateVideo(t *testing.T) {
	crf := 23
	qp := 24
	neg := -1

	t.Run("crf and qp both set is an error", func(t *testing.T) {
		if err := ValidateVideo(ScalerSoftware, 720, &crf, &qp); err == nil {
			t.Fatal("expected error when both crf and qp are set")
		}
	})
	t.Run("crf alone is fine", func(t *testing.T) {
		if err := ValidateVideo("", 720, &crf, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("qp alone is fine", func(t *testing.T) {
		if err := ValidateVideo("", 720, nil, &qp); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("negative crf is an error", func(t *testing.T) {
		if err := ValidateVideo("", 720, &neg, nil); err == nil {
			t.Fatal("expected error for negative crf")
		}
	})
	t.Run("negative qp is an error", func(t *testing.T) {
		if err := ValidateVideo("", 720, nil, &neg); err == nil {
			t.Fatal("expected error for negative qp")
		}
	})
	t.Run("unknown scaler is an error naming the allowed set", func(t *testing.T) {
		err := ValidateVideo(Scaler("qsv"), 720, nil, nil)
		if err == nil {
			t.Fatal("expected error for unknown scaler")
		}
		if !strings.Contains(err.Error(), "software") || !strings.Contains(err.Error(), "vaapi") {
			t.Errorf("error = %v, want it to enumerate the allowed set", err)
		}
	})
	t.Run("scaler without height is an error", func(t *testing.T) {
		if err := ValidateVideo(ScalerVAAPI, 0, nil, nil); err == nil {
			t.Fatal("expected error: scaler set but height is 0")
		}
	})
	t.Run("explicit software scaler without height is also an error", func(t *testing.T) {
		if err := ValidateVideo(ScalerSoftware, 0, nil, nil); err == nil {
			t.Fatal("expected error: scaler set (even to software) but height is 0")
		}
	})
	t.Run("no scaler and no height is fine", func(t *testing.T) {
		if err := ValidateVideo("", 0, nil, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestHWAccel_ArgsAndValidate(t *testing.T) {
	t.Run("nil block emits nothing and validates clean", func(t *testing.T) {
		var h *HWAccel
		if got := h.Args(); got != nil {
			t.Errorf("Args() = %v, want nil", got)
		}
		if err := h.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})
	t.Run("kind only", func(t *testing.T) {
		h := &HWAccel{Kind: "vaapi"}
		want := []string{"-hwaccel", "vaapi"}
		if got := h.Args(); !slices.Equal(got, want) {
			t.Errorf("Args() = %v, want %v", got, want)
		}
		if err := h.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})
	t.Run("kind, device, output_format all set, in order", func(t *testing.T) {
		h := &HWAccel{Kind: "vaapi", Device: "/dev/dri/renderD128", OutputFormat: "vaapi"}
		want := []string{"-hwaccel", "vaapi", "-hwaccel_device", "/dev/dri/renderD128", "-hwaccel_output_format", "vaapi"}
		if got := h.Args(); !slices.Equal(got, want) {
			t.Errorf("Args() = %v, want %v", got, want)
		}
	})
	t.Run("empty block (kind missing) is an error", func(t *testing.T) {
		h := &HWAccel{}
		if err := h.Validate(); err == nil {
			t.Fatal("expected error: kind is required")
		}
	})
	t.Run("device without kind is still an error", func(t *testing.T) {
		h := &HWAccel{Device: "/dev/dri/renderD128"}
		if err := h.Validate(); err == nil {
			t.Fatal("expected error: kind is required even if device is set")
		}
	})
	t.Run("kind smuggling a flag is an error", func(t *testing.T) {
		h := &HWAccel{Kind: "-y"}
		if err := h.Validate(); err == nil {
			t.Fatal("expected error: kind must not start with '-'")
		}
	})
}

// TestPreInput_Order は「kind → device → output_format → input extra args」の順を
// 固定する。壊し方: 順序を入れ替える / 期待値を変える。
func TestPreInput_Order(t *testing.T) {
	hw := &HWAccel{Kind: "vaapi", Device: "/dev/dri/renderD128", OutputFormat: "vaapi"}
	got := PreInput(hw, []string{"-extra_hw_frames", "4"})
	want := []string{
		"-hwaccel", "vaapi",
		"-hwaccel_device", "/dev/dri/renderD128",
		"-hwaccel_output_format", "vaapi",
		"-extra_hw_frames", "4",
	}
	if !slices.Equal(got, want) {
		t.Errorf("PreInput = %v, want %v", got, want)
	}
}

func TestPreInput_NilHWAccel(t *testing.T) {
	got := PreInput(nil, []string{"-foo", "bar"})
	want := []string{"-foo", "bar"}
	if !slices.Equal(got, want) {
		t.Errorf("PreInput = %v, want %v", got, want)
	}
}

func TestValidateExtraArgs(t *testing.T) {
	t.Run("app-owned flags are individually rejected", func(t *testing.T) {
		for _, flag := range []string{
			"-i", "-y", "-n", "-f", "-c:v", "-c:a", "-codec:v", "-codec:a",
			"-vf", "-filter:v", "-filter_complex", "-crf", "-qp", "-preset",
			"-progress", "-loglevel", "-hwaccel", "-hwaccel_device", "-hwaccel_output_format",
			"-force_key_frames", "-hls_time", "-hls_list_size", "-hls_flags",
			"-hls_segment_filename", "-hls_base_url",
		} {
			t.Run(flag, func(t *testing.T) {
				if err := ValidateExtraArgs("extra_args", []string{flag}); err == nil {
					t.Fatalf("expected %q to be rejected", flag)
				}
			})
		}
	})

	t.Run("bare positional argument alone is rejected", func(t *testing.T) {
		if err := ValidateExtraArgs("extra_args", []string{"/tmp/evil.mp4"}); err == nil {
			t.Fatal("expected bare positional argument to be rejected")
		}
	})
	t.Run("bare positional argument after a flag value is rejected", func(t *testing.T) {
		if err := ValidateExtraArgs("extra_args", []string{"-movflags", "+faststart", "/tmp/evil.mp4"}); err == nil {
			t.Fatal("expected trailing bare positional argument to be rejected")
		}
	})
	t.Run("flag with a value is allowed", func(t *testing.T) {
		if err := ValidateExtraArgs("extra_args", []string{"-movflags", "+faststart"}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("bare boolean flag is allowed", func(t *testing.T) {
		if err := ValidateExtraArgs("extra_args", []string{"-an"}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("map with a stream selector value is allowed", func(t *testing.T) {
		if err := ValidateExtraArgs("extra_args", []string{"-map", "0:a:1"}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("boolean flag cannot consume an output path", func(t *testing.T) {
		for _, flag := range []string{"-an", "-vn", "-shortest", "-nostdin", "--"} {
			t.Run(flag, func(t *testing.T) {
				if err := ValidateExtraArgs("extra_args", []string{flag, "/tmp/evil.mp4"}); err == nil {
					t.Fatalf("expected output path after %q to be rejected", flag)
				}
			})
		}
	})
	t.Run("filtergraph aliases and app-owned aliases are rejected", func(t *testing.T) {
		for _, args := range [][]string{
			{"-filter:v:0", "scale=-2:100"},
			{"-lavfi", "movie=/etc/passwd[v]"},
			{"-vcodec", "libx264"},
			{"-acodec", "aac"},
			{"-v", "debug"},
		} {
			t.Run(args[0], func(t *testing.T) {
				if err := ValidateExtraArgs("extra_args", args); err == nil {
					t.Fatalf("expected %q to be rejected", args[0])
				}
			})
		}
	})
	t.Run("unknown option is rejected", func(t *testing.T) {
		if err := ValidateExtraArgs("extra_args", []string{"-unknown", "value"}); err == nil {
			t.Fatal("expected unknown option to be rejected")
		}
	})
	t.Run("all violations are listed in one error", func(t *testing.T) {
		err := ValidateExtraArgs("extra_args", []string{"-i", "in", "-y"})
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "-i") || !strings.Contains(err.Error(), "-y") {
			t.Errorf("error = %v, want both -i and -y mentioned", err)
		}
	})
	t.Run("app-owned hls flag is rejected", func(t *testing.T) {
		if err := ValidateExtraArgs("extra_args", []string{"-hls_time", "2"}); err == nil {
			t.Fatal("expected -hls_time to be rejected")
		}
	})
	t.Run("allowed option requires its value", func(t *testing.T) {
		if err := ValidateExtraArgs("extra_args", []string{"-movflags"}); err == nil {
			t.Fatal("expected -movflags without a value to be rejected")
		}
	})
}
