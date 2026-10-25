package api

import (
	"fmt"

	"github.com/fetburner/rokuban/internal/store"
)

type recordingPlaybackConfig struct {
	enabled  bool
	store    string
	profiles []LiveProfileSummary
}

func (c recordingPlaybackConfig) urls(r store.RecordingQueryRow) []RecordingHLSPlayback {
	result := make([]RecordingHLSPlayback, 0)
	if !c.enabled || r.HLSGeneration == nil {
		return result
	}
	for _, profile := range c.profiles {
		result = append(result, RecordingHLSPlayback{
			Profile:   profile.Name,
			MasterUrl: fmt.Sprintf("/api/media/stores/%s/recordings/%d/hls/v%d/%s/master.m3u8", c.store, r.ID, *r.HLSGeneration, profile.Name),
		})
	}
	return result
}
