package config

import (
	"fmt"
	"math"
	"time"
)

const (
	// RecordingHLSMaxSegmentBytes はキャッシュ下限の計算に使う 1 レンディションの容量見積もり。
	// 実際のセグメントサイズの上限ではない。
	RecordingHLSMaxSegmentBytes    int64 = 16 << 20
	recordingHLSPrefetchSegments         = 5
	recordingHLSRenditionCount           = 5 // video, three audio tracks, and optional subtitles
	defaultRecordingHLSCacheBytes        = 1 << 30
	defaultRecordingHLSConcurrency       = 2
	defaultRecordingHLSWaitTimeout       = 30 * time.Second
)

// RecordingHLSConfig は世代付き録画 HLS URL の公開設定。
type RecordingHLSConfig struct {
	Enabled       bool          `yaml:"enabled"`
	SegmentDir    string        `yaml:"segment_dir"`
	CacheBytes    int64         `yaml:"cache_bytes"`
	MaxConcurrent int           `yaml:"max_concurrent"`
	WaitTimeout   time.Duration `yaml:"wait_timeout"`
}

func (c *RecordingHLSConfig) applyDefaults() {
	if c.CacheBytes == 0 {
		c.CacheBytes = defaultRecordingHLSCacheBytes
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = defaultRecordingHLSConcurrency
	}
	if c.WaitTimeout == 0 {
		c.WaitTimeout = defaultRecordingHLSWaitTimeout
	}
}

func (c Config) validateRecordingHLS() error {
	name := c.Storage.MediaStoreName
	if name == "" && c.RecordingHLS.Enabled {
		return fmt.Errorf("storage.stores must contain exactly one fs store when recording_hls.enabled is true")
	}
	if c.RecordingHLS.MaxConcurrent < 1 {
		return fmt.Errorf("recording_hls.max_concurrent must be >= 1, got %d", c.RecordingHLS.MaxConcurrent)
	}
	if c.RecordingHLS.CacheBytes <= 0 {
		return fmt.Errorf("recording_hls.cache_bytes must be > 0, got %d", c.RecordingHLS.CacheBytes)
	}
	if c.RecordingHLS.WaitTimeout <= 0 {
		return fmt.Errorf("recording_hls.wait_timeout must be > 0, got %v", c.RecordingHLS.WaitTimeout)
	}
	if c.RecordingHLS.Enabled {
		if c.RecordingHLS.SegmentDir == "" {
			return fmt.Errorf("recording_hls.segment_dir is required when recording_hls.enabled is true")
		}
		factor := int64(recordingHLSPrefetchSegments + 1)
		if int64(c.RecordingHLS.MaxConcurrent) > math.MaxInt64/factor {
			return fmt.Errorf("recording_hls cache minimum overflows int64")
		}
		minimum := int64(c.RecordingHLS.MaxConcurrent) * factor
		if minimum > math.MaxInt64/int64(recordingHLSRenditionCount) {
			return fmt.Errorf("recording_hls cache minimum overflows int64")
		}
		minimum *= int64(recordingHLSRenditionCount)
		if minimum > math.MaxInt64/RecordingHLSMaxSegmentBytes {
			return fmt.Errorf("recording_hls cache minimum overflows int64")
		}
		minimum *= RecordingHLSMaxSegmentBytes
		if c.RecordingHLS.CacheBytes <= minimum {
			return fmt.Errorf("recording_hls.cache_bytes must be greater than %d", minimum)
		}
	}
	return nil
}
