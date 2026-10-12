package riveradapter

import (
	"testing"
	"time"
)

// RecoveryGrace は River の救済閾値（0 なら JobTimeout + 1h か既定 1h）に 2 分を足した値を返す。
func TestConfigRecoveryGrace(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want time.Duration
	}{
		{"explicit threshold", Config{RescueStuckJobsAfter: 6 * time.Minute}, 8 * time.Minute},
		{"zero means River default 1h", Config{}, 62 * time.Minute},
		{"zero with long JobTimeout means JobTimeout+1h", Config{JobTimeout: 90 * time.Minute}, 152 * time.Minute},
		{"zero with JobTimeout at 1h is JobTimeout+1h", Config{JobTimeout: time.Hour}, 122 * time.Minute},
		{"zero with short JobTimeout is JobTimeout+1h", Config{JobTimeout: 30 * time.Minute}, 92 * time.Minute},
		{"explicit threshold wins over JobTimeout", Config{RescueStuckJobsAfter: 6 * time.Minute, JobTimeout: time.Minute}, 8 * time.Minute},
	} {
		if got := tc.cfg.RecoveryGrace(); got != tc.want {
			t.Errorf("%s: RecoveryGrace() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
