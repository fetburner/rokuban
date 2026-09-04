//go:build conformance

package fixture

import (
	"testing"
	"time"
)

func TestPathologyEvents(t *testing.T) {
	base := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	start := base.Truncate(30 * time.Second).Add(10 * time.Second)
	before := start.Add(-5 * time.Second)
	after := start.Add(5 * time.Second)
	cfg := NewConfigForCase(CasePrecedingExtension)

	tests := []struct {
		name              string
		mode              string
		at                time.Time
		presentID         uint16
		followingID       uint16
		presentDuration   time.Duration
		presentStatus     byte
		followingDuration time.Duration
		scheduleID        uint16
	}{
		{name: "preceding-before", mode: CasePrecedingExtension, at: before, presentID: PrecedingEventID, followingID: EventID, presentDuration: UndefinedDuration, presentStatus: 4, followingDuration: pathologyDuration, scheduleID: EventID},
		{name: "preceding-after", mode: CasePrecedingExtension, at: after, presentID: EventID, followingID: FollowingEventID, presentDuration: pathologyDuration, presentStatus: 4, followingDuration: time.Hour, scheduleID: EventID},
		{name: "running-before", mode: CaseRunningStatus, at: before, presentID: EventID, followingID: FollowingEventID, presentDuration: pathologyDuration, presentStatus: 2, followingDuration: time.Hour, scheduleID: EventID},
		{name: "running-after", mode: CaseRunningStatus, at: after, presentID: EventID, followingID: FollowingEventID, presentDuration: pathologyDuration, presentStatus: 4, followingDuration: time.Hour, scheduleID: EventID},
		{name: "following", mode: CaseFollowing, at: before, presentID: PrecedingEventID, followingID: EventID, presentDuration: 60 * time.Second, presentStatus: 4, followingDuration: pathologyDuration, scheduleID: EventID},
		{name: "reset-before", mode: CaseEventIDReset, at: before, presentID: PrecedingEventID, followingID: EventID, presentDuration: 30 * time.Second, presentStatus: 4, followingDuration: pathologyDuration, scheduleID: EventID},
		{name: "reset-after", mode: CaseEventIDReset, at: after, presentID: ReplacementEventID, followingID: FollowingEventID, presentDuration: pathologyDuration, presentStatus: 4, followingDuration: time.Hour, scheduleID: ReplacementEventID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg.Case = tt.mode
			present, following := currentEvents(cfg, tt.at)
			if present.EventID != tt.presentID || following.EventID != tt.followingID {
				t.Fatalf("p/f event ids = %d/%d, want %d/%d", present.EventID, following.EventID, tt.presentID, tt.followingID)
			}
			if present.Duration != tt.presentDuration || following.Duration != tt.followingDuration {
				t.Fatalf("p/f durations = %s/%s, want %s/%s", present.Duration, following.Duration, tt.presentDuration, tt.followingDuration)
			}
			if present.RunningStatus != tt.presentStatus {
				t.Fatalf("present running_status = %d, want %d", present.RunningStatus, tt.presentStatus)
			}
			events := scheduleEvents(cfg, tt.at)
			if len(events) == 0 || events[len(events)-1].EventID != tt.scheduleID {
				t.Fatalf("schedule event ids = %+v, want last event id %d", events, tt.scheduleID)
			}
			// EIT p/f の present が前番組（PrecedingEventID）を指すケースは、EIT schedule
			// にも同じ event_id が載る（events[0]）。同一 event の尺が p/f と schedule で
			// 食い違うと、どちらを採用するかが mirakc の EPG マージ上未規定になるので、
			// 両方が同じ start/duration を書いていることを固定する。
			if present.EventID == PrecedingEventID {
				if events[0].EventID != PrecedingEventID {
					t.Fatalf("schedule の先頭 event id = %d, want %d（EIT p/f の present と揃うはず）", events[0].EventID, PrecedingEventID)
				}
				if events[0].Start != present.Start || events[0].Duration != present.Duration {
					t.Fatalf("schedule の preceding = start=%s duration=%s、EIT p/f の present = start=%s duration=%s と食い違う",
						events[0].Start, events[0].Duration, present.Start, present.Duration)
				}
			}
		})
	}
}
