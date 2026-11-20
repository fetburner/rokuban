package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 束ねの母集団は live な行に絞らない。印が付いた行をごみ箱 / supersede に入れても、
// 同じ放送イベントのもう 1 行は視聴済みのまま読め、「続きから」にも戻らない。
func TestWatchedGroupingSurvivesMarkedRowLeavingLiveSet(t *testing.T) {
	for _, tc := range []struct{ name, set string }{
		{"ごみ箱", "deleted_at = now()"},
		{"supersede", "superseded_at = now()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			srv := NewTestServer(t, pool)
			ctx := context.Background()
			playback := pgstoretest.Playback(pool)
			start := time.Date(2026, 8, 2, 1, 0, 0, 0, time.UTC)
			marked := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
				p.EventID = 400
				p.Title = "印の行"
				p.ProgramStartAt = start
				p.ProgramDurationMs = time.Hour.Milliseconds()
			})
			other := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
				p.EventID = 401
				p.Title = "もう片方"
				p.ProgramStartAt = start
				p.ProgramDurationMs = time.Hour.Milliseconds()
			})
			if err := playback.SavePosition(ctx, other, 9_000); err != nil {
				t.Fatalf("seed position: %v", err)
			}
			if err := playback.MarkWatched(ctx, marked); err != nil {
				t.Fatalf("mark watched: %v", err)
			}
			if _, err := pool.Exec(ctx, `UPDATE recordings SET `+tc.set+` WHERE id = $1`, marked); err != nil {
				t.Fatalf("move marked row out of live set: %v", err)
			}

			list := GetJSON[[]Recording](t, srv.URL+"/api/recordings")
			var listed *Recording
			for i := range list {
				if list[i].Id == other {
					listed = &list[i]
				}
			}
			if listed == nil {
				t.Fatalf("recording %d missing from list", other)
			}
			if listed.WatchedAt == nil {
				t.Errorf("list watchedAt = nil after marked sibling left live set, want non-nil")
			}
			single := GetJSON[Recording](t, fmt.Sprintf("%s/api/recordings/%d", srv.URL, other))
			if single.WatchedAt == nil {
				t.Errorf("single GET watchedAt = nil after marked sibling left live set, want non-nil")
			}
			cont := GetJSON[[]Recording](t, srv.URL+"/api/recordings/continue-watching")
			for _, row := range cont {
				if row.Id == other {
					t.Errorf("continue-watching still lists recording %d whose event is watched", other)
				}
			}
		})
	}
}
