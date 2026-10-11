package jobs

import (
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
)

func TestEncodeSpecTimeoutUsesSavedDeadlineOrFallback(t *testing.T) {
	for _, tt := range []struct {
		name string
		args EncodeJobArgs
		want time.Duration
	}{
		{name: "saved deadline", args: EncodeJobArgs{Timeout: 90 * time.Minute}, want: 90 * time.Minute},
		{name: "old args without deadline", args: EncodeJobArgs{}, want: 12 * time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.args.Spec().Timeout(tt.args); got != tt.want {
				t.Errorf("Spec().Timeout(args) = %s, want literal %s", got, tt.want)
			}
		})
	}
}

func TestCMDetectionSpecsUseRecordingDuration(t *testing.T) {
	for _, tt := range []struct {
		name string
		args jobq.Args
		want time.Duration
	}{
		{"CM detection zero duration", CMDetectJobArgs{}, 30 * time.Minute},
		{"CM detection known duration", CMDetectJobArgs{RecordingDurationMs: 20 * 60 * 1000}, 40 * time.Minute},
		{"logo candidate zero duration", CMLogoCandidateJobArgs{}, 30 * time.Minute},
		{"logo candidate known duration", CMLogoCandidateJobArgs{RecordingDurationMs: 20 * 60 * 1000}, 40 * time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.args.Spec().Timeout(tt.args); got != tt.want {
				t.Errorf("Spec().Timeout(args) = %s, want literal %s", got, tt.want)
			}
		})
	}
}

func TestSeekTilesPriorityComesFromSpec(t *testing.T) {
	thumb := ThumbnailJobArgs{}.Spec()
	tiles := SeekTilesJobArgs{}.Spec()
	if tiles.Queue != ThumbnailQueue {
		t.Fatalf("seek_tiles queue = %q, want literal %q", tiles.Queue, "thumbnail")
	}
	if tiles.Priority != 4 || thumb.Priority != 1 {
		t.Fatalf("Spec priorities = tiles:%d thumbnail:%d, want literal 4 and 1", tiles.Priority, thumb.Priority)
	}
}
