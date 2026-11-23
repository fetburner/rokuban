package cmdetect

import (
	"reflect"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
)

func TestReviewMaterialParsersUseChapterFrameAxis(t *testing.T) {
	const durationMs = int64(10010)
	logos, err := parseLogoFrameRanges("    30 S 0 ALL    30    90\n    89 E 0 ALL    89    90\n", durationMs)
	if err != nil {
		t.Fatal(err)
	}
	structure, err := parseStructureOutput("    30     89    2   6    1 :CM\n    90    119    1   0    0\n   120    149    1   0    0 :StationSpecific\n", durationMs)
	if err != nil {
		t.Fatal(err)
	}
	want := []chapters.Range{{StartMs: 1001, EndMs: 3003}}
	if !reflect.DeepEqual(logos, want) {
		t.Fatalf("logo ranges = %#v, want %#v", logos, want)
	}
	if len(structure) != 3 || structure[0].StartMs != want[0].StartMs || structure[0].EndMs != want[0].EndMs ||
		structure[0].ErrorFrames != 6 || structure[0].Label != ":CM" {
		t.Fatalf("structure = %#v, want interval %#v, error 6, label CM", structure, want)
	}
	if structure[1].Label != "" || structure[2].Label != ":StationSpecific" {
		t.Fatalf("optional labels = %#v, want empty and unmodified tool labels", []string{structure[1].Label, structure[2].Label})
	}
	cm, err := cmRangesFromCutAVS("Trim(0,29) ++ Trim(90,299)", durationMs)
	if err != nil {
		t.Fatal(err)
	}
	cmRanges := chapterRangesFromFrames(cm, durationMs)
	if len(cmRanges) != 1 || cmRanges[0].StartMs != want[0].StartMs || cmRanges[0].EndMs != want[0].EndMs {
		t.Fatalf("cm ranges = %#v, want same frame axis as %#v", cmRanges, want)
	}
}

func TestReviewMaterialParsersRejectUnpairedLogoEvents(t *testing.T) {
	if _, err := parseLogoFrameRanges("12 S 0 ALL 12 20\n", 10000); err == nil {
		t.Fatal("unpaired start event accepted")
	}
}
