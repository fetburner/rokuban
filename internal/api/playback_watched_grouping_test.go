package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
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
			srv := newAPIServer(t, pool)
			ctx := context.Background()
			q := sqlcgen.New(pool)
			start := time.Date(2026, 8, 2, 1, 0, 0, 0, time.UTC)
			marked := createPlaybackRecording(t, pool, start, 400, "印の行")
			other := createPlaybackRecording(t, pool, start, 401, "もう片方")
			if _, err := q.UpsertRecordingPlaybackPosition(ctx, sqlcgen.UpsertRecordingPlaybackPositionParams{RecordingID: other, PositionMs: 9_000}); err != nil {
				t.Fatalf("seed position: %v", err)
			}
			if _, err := q.UpsertRecordingWatched(ctx, marked); err != nil {
				t.Fatalf("mark watched: %v", err)
			}
			if _, err := pool.Exec(ctx, `UPDATE recordings SET `+tc.set+` WHERE id = $1`, marked); err != nil {
				t.Fatalf("move marked row out of live set: %v", err)
			}

			var list []Recording
			if resp := getJSON(t, srv.URL+"/api/recordings", &list); resp.StatusCode != http.StatusOK {
				t.Fatalf("list status = %d", resp.StatusCode)
			}
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
			var single Recording
			if resp := getJSON(t, fmt.Sprintf("%s/api/recordings/%d", srv.URL, other), &single); resp.StatusCode != http.StatusOK {
				t.Fatalf("get status = %d", resp.StatusCode)
			}
			if single.WatchedAt == nil {
				t.Errorf("single GET watchedAt = nil after marked sibling left live set, want non-nil")
			}
			var cont []Recording
			if resp := getJSON(t, srv.URL+"/api/recordings/continue-watching", &cont); resp.StatusCode != http.StatusOK {
				t.Fatalf("continue-watching status = %d", resp.StatusCode)
			}
			for _, row := range cont {
				if row.Id == other {
					t.Errorf("continue-watching still lists recording %d whose event is watched", other)
				}
			}
		})
	}
}
