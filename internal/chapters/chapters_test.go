package chapters

import (
	"errors"
	"reflect"
	"testing"
)

func TestReviewCandidates_LogoThreshold(t *testing.T) {
	auto := []Span{{StartMs: 0, EndMs: 10000, Label: LabelCM, Cut: true}}
	for _, tt := range []struct {
		name string
		end  int64
		want []ReviewCandidate
	}{
		{name: "below", end: 4999, want: []ReviewCandidate{}},
		{name: "at threshold", end: 5000, want: []ReviewCandidate{{Kind: "overcut_logo", StartMs: 0, EndMs: 10000}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := ReviewCandidates(auto, []Range{{StartMs: 0, EndMs: tt.end}}, nil)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ReviewCandidates() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestReviewCandidates_GridErrorThresholdBothSigns(t *testing.T) {
	cut := []Span{{StartMs: 0, EndMs: FrameToMs(300), Label: LabelCM, Cut: true}}
	for _, tt := range []struct {
		name        string
		errorFrames int64
		want        []ReviewCandidate
	}{
		{name: "positive boundary", errorFrames: 5, want: []ReviewCandidate{}},
		{name: "positive outside", errorFrames: 6, want: []ReviewCandidate{{Kind: "overcut_grid", StartMs: 0, EndMs: 4004}}},
		{name: "negative outside", errorFrames: -6, want: []ReviewCandidate{{Kind: "overcut_grid", StartMs: 0, EndMs: 4004}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := ReviewCandidates(cut, nil, []CMElement{{StartMs: 0, EndMs: FrameToMs(120), ErrorFrames: tt.errorFrames, Label: ":CM"}})
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ReviewCandidates() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestReviewCandidates_GridRequiresCMLabelAndCutOverlap は、15 秒格子の候補が
// 「:CM に限る」「カット区間に重なる」の両方を満たす要素だけから出ることを固定する。
func TestReviewCandidates_GridRequiresCMLabelAndCutOverlap(t *testing.T) {
	cut := []Span{{StartMs: 0, EndMs: FrameToMs(300), Label: LabelCM, Cut: true}}
	for _, tt := range []struct {
		name     string
		elements []CMElement
	}{
		{name: "non-CM label inside cut", elements: []CMElement{{StartMs: 0, EndMs: FrameToMs(120), ErrorFrames: 6, Label: ":Sponsor(cut)"}}},
		{name: "empty label inside cut", elements: []CMElement{{StartMs: 0, EndMs: FrameToMs(120), ErrorFrames: 6, Label: ""}}},
		// カット区間の外の :CM。カットと重ならないので既知長も立たず、missed_cm も出ない。
		{name: "CM label outside cut", elements: []CMElement{{StartMs: FrameToMs(500), EndMs: FrameToMs(620), ErrorFrames: 6, Label: ":CM"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := ReviewCandidates(cut, nil, tt.elements)
			if want := []ReviewCandidate{}; !reflect.DeepEqual(got, want) {
				t.Fatalf("ReviewCandidates() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestReviewCandidates_KnownLengthMatchTolerance(t *testing.T) {
	cut := []Span{
		{StartMs: 0, EndMs: FrameToMs(400), Label: LabelCM, Cut: true},
		{StartMs: FrameToMs(1000), EndMs: FrameToMs(1400), Label: LabelCM, Cut: true},
	}
	elements := []CMElement{
		{StartMs: FrameToMs(20), EndMs: FrameToMs(320), Label: ":CM"},
		{StartMs: FrameToMs(1020), EndMs: FrameToMs(1322), Label: ":CM"},
		{StartMs: FrameToMs(2000), EndMs: FrameToMs(2305), Label: ":Sponsor(cut)"},
		{StartMs: FrameToMs(3000), EndMs: FrameToMs(3306), Label: ""},
	}
	want := []ReviewCandidate{{Kind: "missed_cm", StartMs: 66733, EndMs: 76910}}
	got := ReviewCandidates(cut, nil, elements)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReviewCandidates() = %#v, want %#v", got, want)
	}
}

func TestReviewCandidates_KnownLengthClusterTolerance(t *testing.T) {
	for _, tt := range []struct {
		name         string
		secondLength int64
		want         []ReviewCandidate
	}{
		{name: "within cluster tolerance", secondLength: 303, want: []ReviewCandidate{{Kind: "missed_cm", StartMs: 66733, EndMs: 76743}}},
		{name: "outside cluster tolerance", secondLength: 304, want: []ReviewCandidate{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			auto := []Span{
				{StartMs: 0, EndMs: FrameToMs(400), Label: LabelCM, Cut: true},
				{StartMs: FrameToMs(1000), EndMs: FrameToMs(1400), Label: LabelCM, Cut: true},
			}
			elements := []CMElement{
				{StartMs: FrameToMs(20), EndMs: FrameToMs(320), Label: ":CM"},
				{StartMs: FrameToMs(1020), EndMs: FrameToMs(1020 + tt.secondLength), Label: ":CM"},
				{StartMs: FrameToMs(2000), EndMs: FrameToMs(2300), Label: ":Nologo"},
			}
			got := ReviewCandidates(auto, nil, elements)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ReviewCandidates() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestReviewCandidates_NoMaterialsAndNoCandidatesAreDistinctAtCallerBoundary(t *testing.T) {
	// The pure function's empty result means no evidence matched; the API's separately stored
	// materialsAvailable bit distinguishes this from an older detection with no evidence.
	got := ReviewCandidates(nil, []Range{}, []CMElement{})
	if got == nil || len(got) != 0 {
		t.Fatalf("ReviewCandidates() = %#v, want a non-nil empty result", got)
	}
}

func TestQuantizeMs_SnapsToFrameBoundaries(t *testing.T) {
	// 1 フレーム = 1001/30 ms ≈ 33.367ms。1001ms がちょうど 30 フレーム。
	cases := []struct{ in, want int64 }{
		{0, 0},
		{33, 33},
		{34, 33},
		{50, 33},
		{1000, 1001},
		{1001, 1001},
		{2000, 2002},
		{3000, 3003},
		{4000, 4004},
	}
	for _, c := range cases {
		if got := QuantizeMs(c.in); got != c.want {
			t.Errorf("QuantizeMs(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestQuantizeMs_RoundTripsThroughFrames(t *testing.T) {
	for frame := int64(0); frame < 20000; frame++ {
		ms := FrameToMs(frame)
		if got := MsToFrame(ms); got != frame {
			t.Fatalf("MsToFrame(FrameToMs(%d)=%d) = %d, want %d", frame, ms, got, frame)
		}
	}
}

func TestAutoSpans_KeepsDetectionBelowHalf(t *testing.T) {
	// 4000ms のうち 1000ms ぶんの CM（25%）。境界はフレーム境界へ丸める。
	got := AutoSpans([]Range{{0, 1000}, {2000, 3000}}, 4000)
	want := []Span{
		{StartMs: 0, EndMs: 1001, Label: LabelCM, Cut: true},
		{StartMs: 2002, EndMs: 3003, Label: LabelCM, Cut: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AutoSpans = %+v, want %+v", got, want)
	}
}

func TestAutoSpans_DropsDetectionAboveHalf(t *testing.T) {
	// 1000ms のうち 600ms が CM（60%）。壊れた検出として CM 無しに倒す。
	if got := AutoSpans([]Range{{0, 600}}, 1000); got != nil {
		t.Fatalf("AutoSpans(60%% CM) = %+v, want nil", got)
	}
	// ちょうど 50% は残す（境界は「超えたら」落とす側）。
	if got := AutoSpans([]Range{{0, 500}}, 1000); len(got) != 1 {
		t.Fatalf("AutoSpans(50%% CM) = %+v, want 1 span", got)
	}
}

func TestAutoSpans_MeasuresAgainstExtendedRecording(t *testing.T) {
	// EPG の尺が 1000ms でも、録画が EIT 追従で 4000ms まで延びていれば
	// 分母は検出区間の終端を採る。1000ms で割ると 100% に見えて落ちてしまう。
	got := AutoSpans([]Range{{3000, 4000}}, 1000)
	if len(got) != 1 {
		t.Fatalf("AutoSpans(extended recording) = %+v, want 1 span", got)
	}
}

func TestAutoSpans_UnknownLengthClaimsNothing(t *testing.T) {
	if got := AutoSpans([]Range{{0, 100}}, 0); got != nil {
		t.Fatalf("AutoSpans with unknown length = %+v, want nil", got)
	}
}

func TestDerive_UsesAutoLayerWhenNotOwned(t *testing.T) {
	auto := []Span{
		{StartMs: 1000, EndMs: 2000, Label: LabelCM, Cut: true},
		{StartMs: 3000, EndMs: 4000, Label: LabelCM, Cut: true},
	}
	got := Derive(false, nil, auto, 5000)
	want := Timeline{
		{StartMs: 0, EndMs: 1001},
		{StartMs: 1001, EndMs: 2002, Label: LabelCM, Cut: true},
		{StartMs: 2002, EndMs: 3003},
		{StartMs: 3003, EndMs: 4004, Label: LabelCM, Cut: true},
		{StartMs: 4004, EndMs: 5000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Derive(auto) = %+v, want %+v", got, want)
	}
}

func TestDerive_UsesUserLayerWhenOwned(t *testing.T) {
	// 所有している録画では自動層を読まない。再検出で CM 区間が変わっても
	// ユーザーの修正は影響を受けない。
	auto := []Span{{StartMs: 1000, EndMs: 2000, Label: LabelCM, Cut: true}}
	user := []Span{{StartMs: 3000, EndMs: 3500, Label: "OP", Cut: true}}
	got := Derive(true, user, auto, 4000)
	want := Timeline{
		{StartMs: 0, EndMs: 3003},
		{StartMs: 3003, EndMs: 3504, Label: "OP", Cut: true},
		{StartMs: 3504, EndMs: 4000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Derive(user) = %+v, want %+v", got, want)
	}
}

func TestDerive_UserSpanBeyondProgramDurationExtendsTimeline(t *testing.T) {
	// EIT 追従で延長された録画では、EPG の尺より後ろにも区間を置ける。
	user := []Span{{StartMs: 4000, EndMs: 5000, Label: "ED", Cut: true}}
	got := Derive(true, user, nil, 1000)
	// 5000ms は 150 フレームへ丸まり、ms へ戻すと 5005ms になる。
	if got[len(got)-1].EndMs != 5005 {
		t.Fatalf("timeline end = %d, want 5005 (%+v)", got[len(got)-1].EndMs, got)
	}
}

func TestDerive_NoSpansIsAllMainContent(t *testing.T) {
	got := Derive(true, nil, nil, 4000)
	want := Timeline{{StartMs: 0, EndMs: 4000, Cut: false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Derive(empty) = %+v, want %+v", got, want)
	}
}

func TestValidate_QuantizesAndSorts(t *testing.T) {
	got, err := Validate([]Span{
		{StartMs: 3000, EndMs: 3500, Cut: true},
		{StartMs: 1000, EndMs: 2000, Label: LabelCM, Cut: true},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	want := []Span{
		{StartMs: 1001, EndMs: 2002, Label: LabelCM, Cut: true},
		{StartMs: 3003, EndMs: 3504, Cut: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Validate = %+v, want %+v", got, want)
	}
}

func TestValidate_RejectsEmptySpan(t *testing.T) {
	// 量子化で潰れる区間（34 - 33 = 1ms 未満の差）もここで落ちる。
	_, err := Validate([]Span{{StartMs: 1000, EndMs: 1000, Cut: true}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate(empty) error = %v, want ErrInvalid", err)
	}
	if _, err := Validate([]Span{{StartMs: 36, EndMs: 37, Cut: true}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate(quantized away) error = %v, want ErrInvalid", err)
	}
}

func TestValidate_RejectsOverlap(t *testing.T) {
	// 量子化で生まれる重なりも含めて拒否する。ちょうど接する区間は許す。
	if _, err := Validate([]Span{
		{StartMs: 0, EndMs: 2000, Cut: true},
		{StartMs: 1900, EndMs: 3000, Cut: true},
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate(overlap) error = %v, want ErrInvalid", err)
	}
	if _, err := Validate([]Span{
		{StartMs: 0, EndMs: 2002, Cut: true},
		{StartMs: 2002, EndMs: 3000, Cut: true},
	}); err != nil {
		t.Fatalf("Validate(adjacent) error = %v, want nil", err)
	}
}

func TestValidate_RejectsSpanWithoutLabelOrCut(t *testing.T) {
	// 本編は行を持たない（隙間が本編）。ラベルも無く cut でもない行は意味を
	// 持たないので表現不可能にする（DB の CHECK と同じ規則）。
	if _, err := Validate([]Span{{StartMs: 0, EndMs: 2000}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate(label-less main) error = %v, want ErrInvalid", err)
	}
	if _, err := Validate([]Span{{StartMs: 0, EndMs: 2000, Label: "OP"}}); err != nil {
		t.Fatalf("Validate(labelled main) error = %v, want nil", err)
	}
}
