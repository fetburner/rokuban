package chapters

import (
	"encoding/json"
	"os"
	"testing"
)

type playbackPositionVectors struct {
	Ranges               []Range     `json:"ranges"`
	OriginalToCut        []mapVector `json:"originalToCut"`
	CutToOriginal        []mapVector `json:"cutToOriginal"`
	OutsideOriginalToCut []mapVector `json:"outsideOriginalToCut"`
}

type mapVector struct {
	From int64 `json:"fromMs"`
	To   int64 `json:"toMs"`
}

func TestPlaybackPositionSharedVectors(t *testing.T) {
	data, err := os.ReadFile("../../testdata/playback-position-vectors.json")
	if err != nil {
		t.Fatalf("reading shared vectors: %v", err)
	}
	var vectors playbackPositionVectors
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("decoding shared vectors: %v", err)
	}
	for _, vector := range vectors.OriginalToCut {
		if got := MapMs(vectors.Ranges, vector.From); got != vector.To {
			t.Errorf("MapMs(%d) = %d, want %d", vector.From, got, vector.To)
		}
	}
	for _, vector := range vectors.CutToOriginal {
		if got := UnmapMs(vectors.Ranges, vector.From); got != vector.To {
			t.Errorf("UnmapMs(%d) = %d, want %d", vector.From, got, vector.To)
		}
	}
	for _, vector := range vectors.OutsideOriginalToCut {
		if got := MapMs(vectors.Ranges, vector.From); got != vector.To {
			t.Errorf("MapMs outside keep(%d) = %d, want %d", vector.From, got, vector.To)
		}
	}
}
