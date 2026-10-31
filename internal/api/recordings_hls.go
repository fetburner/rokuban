package api

import (
	"fmt"
	"net/url"

	"github.com/fetburner/rokuban/internal/store"
)

type recordingPlaybackConfig struct {
	enabled  bool
	profiles []LiveProfileSummary
}

func (c recordingPlaybackConfig) urls(r store.RecordingQueryRow) []RecordingHLSPlayback {
	result := make([]RecordingHLSPlayback, 0)
	if !c.enabled || r.HLSGeneration == nil || r.HLSStore == nil {
		return result
	}
	for _, profile := range c.profiles {
		result = append(result, RecordingHLSPlayback{
			Profile:   profile.Name,
			MasterUrl: fmt.Sprintf("/api/media/stores/%s/recordings/%d/hls/v%d/%s/master.m3u8", *r.HLSStore, r.ID, *r.HLSGeneration, profile.Name),
		})
	}
	return result
}

// storeMediaURL は置き場ごとの URL（原本・コマ）を返す。置き場が無ければ nil を返す。
func storeMediaURL(storeName *string, recordingID int64, name string) *string {
	if storeName == nil {
		return nil
	}
	u := fmt.Sprintf("/api/media/stores/%s/recordings/%d/%s", *storeName, recordingID, name)
	return &u
}

// recordingMediaURLs は録画のバイナリ配信の URL を組み立てる。URL の形はサーバーだけが持ち、
// クライアントは組み立てない。encodedProfiles は再生できる encoded のプロファイル。
func recordingMediaURLs(r store.RecordingQueryRow, encodedProfiles []string) RecordingMedia {
	encoded := make([]RecordingEncodedMedia, 0, len(encodedProfiles))
	for _, profile := range encodedProfiles {
		base := fmt.Sprintf("/api/media/recordings/%d/file?profile=%s", r.ID, url.QueryEscape(profile))
		encoded = append(encoded, RecordingEncodedMedia{Profile: profile, Url: base, SubtitlesUrl: base + "&track=subtitles"})
	}
	return RecordingMedia{
		OriginalUrl:  storeMediaURL(r.OriginalStore, r.ID, "file"),
		FrameUrl:     storeMediaURL(r.OriginalStore, r.ID, "frame"),
		ThumbnailUrl: fmt.Sprintf("/api/media/recordings/%d/thumbnail", r.ID),
		SeekTilesUrl: fmt.Sprintf("/api/media/recordings/%d/seek-tiles", r.ID),
		Encoded:      encoded,
	}
}
