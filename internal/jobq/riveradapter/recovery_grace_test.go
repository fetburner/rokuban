package riveradapter

import (
	"testing"
	"time"
)

// RecoveryGrace は River の救済閾値（0 なら公開ドキュメントの既定）に 2 分を足した値を返す。
func TestConfigRecoveryGrace(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want time.Duration
	}{
		{"explicit threshold", Config{RescueStuckJobsAfter: 6 * time.Minute}, 8 * time.Minute},
		{"zero means River default 1h", Config{}, 62 * time.Minute},
		{"zero with long JobTimeout means JobTimeout+1h", Config{JobTimeout: 90 * time.Minute}, 152 * time.Minute},
		{"zero with JobTimeout at 1h stays 1h", Config{JobTimeout: time.Hour}, 62 * time.Minute},
	} {
		if got := tc.cfg.RecoveryGrace(); got != tc.want {
			t.Errorf("%s: RecoveryGrace() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
