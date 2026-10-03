package worker

import (
	"encoding/json"
	"math"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// thumbnailSource is one usable file from which a chapter-aware thumbnail can be cut.
// Cut sources carry the keep ranges frozen when that encoded asset was made.
type thumbnailSource struct {
	MediaAssetID int64            `json:"mediaAssetId"`
	Profile      string           `json:"profile"`
	RelPath      string           `json:"relPath"`
	Cut          bool             `json:"cut"`
	KeepRanges   []chapters.Range `json:"keepRanges"`
}

type thumbnailInputs struct {
	Original *thumbnailSource
	Encoded  []thumbnailSource
}

type thumbnailPlan struct {
	Source       thumbnailSource
	InputSeekMs  int64
	RecordedSeek int64
}

// thumbnailTimeline decodes the chapter timeline of a candidate row. The bool is
// false when the recording has neither a CM detection nor user-owned chapters.
func thumbnailTimeline(p sqlcgen.ListThumbnailReselectCandidatesRow) (chapters.Timeline, bool, error) {
	if !p.Detected && !p.Owned {
		return nil, false, nil
	}
	var cmRanges []chapters.Range
	if err := json.Unmarshal(p.CmRanges, &cmRanges); err != nil {
		return nil, true, err
	}
	var userSpans []chapters.Span
	if err := json.Unmarshal(p.UserSpans, &userSpans); err != nil {
		return nil, true, err
	}
	auto := chapters.AutoSpans(cmRanges, p.ProgramDurationMs)
	return chapters.Derive(p.Owned, userSpans, auto, p.ProgramDurationMs), true, nil
}

// thumbnailInputsOf decodes the usable input files of a candidate row.
func thumbnailInputsOf(p sqlcgen.ListThumbnailReselectCandidatesRow) (thumbnailInputs, error) {
	var inputs thumbnailInputs
	if p.OriginalMediaAssetID > 0 && p.OriginalRelPath != "" {
		inputs.Original = &thumbnailSource{
			MediaAssetID: p.OriginalMediaAssetID,
			RelPath:      p.OriginalRelPath,
		}
	}
	if err := json.Unmarshal(p.EncodedAssets, &inputs.Encoded); err != nil {
		return thumbnailInputs{}, err
	}
	return inputs, nil
}

// thumbnailPlanForInputs selects the preferred available input and maps the policy
// position onto its time axis. Uncut files share the chapter axis. A cut file uses
// the frozen axis that was used to produce it.
func thumbnailPlanForInputs(in thumbnailInputs, keep []chapters.Range) (thumbnailPlan, bool) {
	if len(keep) == 0 {
		return thumbnailPlan{}, false
	}
	source, ok := preferredThumbnailSource(in)
	if !ok {
		return thumbnailPlan{}, false
	}
	if source.Cut {
		totalMs := thumbnailKeepDurationMs(source.KeepRanges)
		if totalMs <= 0 {
			return thumbnailPlan{}, false
		}
		inputSeek := thumbnailSeekMs(totalMs)
		return thumbnailPlan{
			Source:       source,
			InputSeekMs:  inputSeek,
			RecordedSeek: chapters.UnmapMs(source.KeepRanges, inputSeek),
		}, true
	}
	seekInKeep := thumbnailSeekMs(thumbnailKeepDurationMs(keep))
	inputSeek := chapters.UnmapMs(keep, seekInKeep)
	return thumbnailPlan{
		Source:       source,
		InputSeekMs:  inputSeek,
		RecordedSeek: inputSeek,
	}, true
}

// thumbnailPlannedSeek returns the position that should be recorded in the
// original timeline for the currently preferred input.
func thumbnailPlannedSeek(in thumbnailInputs, keep []chapters.Range) (int64, bool) {
	plan, ok := thumbnailPlanForInputs(in, keep)
	if !ok {
		return 0, false
	}
	return plan.RecordedSeek, true
}

// thumbnailNeedsReselect is the single level-trigger predicate shared by the
// periodic candidate scanner and ThumbnailWorker. A different preferred position
// alone never triggers replacement: the current frame must be outside the keep set.
func thumbnailNeedsReselect(recorded *int64, in thumbnailInputs, timeline chapters.Timeline) bool {
	keep := chapters.KeepRanges(timeline)
	if len(keep) == 0 {
		return false
	}
	want, ok := thumbnailPlannedSeek(in, keep)
	if !ok {
		return false
	}
	if recorded == nil {
		return true
	}
	return !inThumbnailRanges(keep, *recorded) && want != *recorded
}

func preferredThumbnailSource(in thumbnailInputs) (thumbnailSource, bool) {
	if in.Original != nil && in.Original.MediaAssetID > 0 && in.Original.RelPath != "" {
		return *in.Original, true
	}
	var bestUncut *thumbnailSource
	var bestCut *thumbnailSource
	for i := range in.Encoded {
		source := &in.Encoded[i]
		if source.MediaAssetID <= 0 || source.RelPath == "" {
			continue
		}
		if source.Cut {
			if len(source.KeepRanges) == 0 {
				continue
			}
			if bestCut == nil || source.Profile < bestCut.Profile {
				bestCut = source
			}
		} else if bestUncut == nil || source.Profile < bestUncut.Profile {
			bestUncut = source
		}
	}
	if bestUncut != nil {
		return *bestUncut, true
	}
	if bestCut != nil {
		return *bestCut, true
	}
	return thumbnailSource{}, false
}

func thumbnailKeepDurationMs(keep []chapters.Range) int64 {
	var total int64
	for _, r := range keep {
		if r.EndMs > r.StartMs {
			total += r.EndMs - r.StartMs
		}
	}
	return total
}

func thumbnailSeekMs(durationMs int64) int64 {
	if durationMs <= 0 {
		return 0
	}
	seek := int64(math.Round(float64(durationMs) * thumbnailSeekFraction))
	if seek > int64(thumbnailSeekMax/time.Millisecond) {
		return int64(thumbnailSeekMax / time.Millisecond)
	}
	return seek
}

func inThumbnailRanges(ranges []chapters.Range, ms int64) bool {
	for _, r := range ranges {
		if r.StartMs <= ms && ms < r.EndMs {
			return true
		}
	}
	return false
}
