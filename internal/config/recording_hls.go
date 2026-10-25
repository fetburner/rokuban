package config

import (
	"fmt"
	"regexp"
)

// RecordingHLSConfig は世代付き録画 HLS URL の公開設定。
type RecordingHLSConfig struct {
	Enabled bool `yaml:"enabled"`
}

var mediaStoreNamePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

func (c Config) validateRecordingHLS() error {
	name := c.Storage.MediaStoreName
	if name == "" {
		if c.RecordingHLS.Enabled {
			return fmt.Errorf("storage.media_store_name is required when recording_hls.enabled is true")
		}
		return nil
	}
	if !mediaStoreNamePattern.MatchString(name) {
		return fmt.Errorf("storage.media_store_name must contain only lowercase ASCII letters, digits and hyphens")
	}
	return nil
}
