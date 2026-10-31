package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRecordingsHLSPlayback(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			pool := testutil.SetupDB(t)
			srv := httptest.NewServer(NewRouter(RouterConfig{
				RecordingQuery:      pgstoretest.Recordings(pool),
				RecordingHLSEnabled: enabled, StoreNames: []string{dbtest.StoreName},
				LiveProfiles: []LiveProfileSummary{{Name: "sd"}, {Name: "hd"}},
			}))
			defer srv.Close()
			for _, tc := range []struct {
				name, asset, status, removal string
				generation                   *int64
				progress                     bool
				wantGen                      int64
				playable                     bool
			}{
				{name: "legacy original", asset: "active", playable: true},
				{name: "recording with committed original", status: "recording", asset: "active", generation: new(int64(37)), wantGen: 37, playable: true},
				{name: "original generation", asset: "active", generation: new(int64(37)), wantGen: 37, playable: true},
				{name: "original wins over temp", asset: "active", generation: new(int64(37)), progress: true, wantGen: 37, playable: true},
				{name: "legacy original wins over temp", asset: "active", progress: true, playable: true},
				{name: "recording temp", status: "recording", progress: true, wantGen: 91, playable: true},
				{name: "finished temp", progress: true, wantGen: 91, playable: true},
				{name: "before ingest", status: "recording"},
				{name: "encoded only", asset: "encoded"},
				{name: "deleting original", asset: "deleting"},
				{name: "deleted original", asset: "deleted"},
				{name: "inactive original with temp", asset: "deleting", progress: true, wantGen: 91, playable: true},
				{name: "trash original", asset: "active", removal: "deleted_at"},
				{name: "trash temp", progress: true, removal: "deleted_at"},
				{name: "purged", asset: "active", progress: true, removal: "purged_at"},
				{name: "superseded", asset: "active", progress: true, removal: "superseded_at"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
						if tc.status != "" {
							p.Status = tc.status
						}
					})
					if tc.asset == "encoded" {
						dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) { p.Kind = "encoded"; p.Profile = new("hd") })
					} else if tc.asset != "" {
						asset := seedIngested(t, pool, id, 123, nil)
						dbtest.SetMediaAssetState(t, pool, asset, tc.asset)
						if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET ingest_generation=$2 WHERE id=$1`, asset, tc.generation); err != nil {
							t.Fatal(err)
						}
					}
					if tc.progress {
						seedIngestProgress(t, pool, id, 123, nil, time.Now())
						if _, err := pool.Exec(context.Background(), `UPDATE recording_ingest_progress SET temp_generation=91, temp_store='test-store' WHERE recording_id=$1`, id); err != nil {
							t.Fatal(err)
						}
					}
					if tc.removal == "purged_at" {
						if _, err := pool.Exec(context.Background(), `UPDATE recordings SET deleted_at=now() WHERE id=$1`, id); err != nil {
							t.Fatal(err)
						}
					}
					if tc.removal != "" {
						if _, err := pool.Exec(context.Background(), `UPDATE recordings SET `+tc.removal+`=now() WHERE id=$1`, id); err != nil {
							t.Fatal(err)
						}
					}
					want := make([]RecordingHLSPlayback, 0)
					if enabled && tc.playable {
						for _, profile := range []string{"sd", "hd"} {
							want = append(want, RecordingHLSPlayback{Profile: profile, MasterUrl: fmt.Sprintf("/api/media/stores/%s/recordings/%d/hls/v%d/%s/master.m3u8", dbtest.StoreName, id, tc.wantGen, profile)})
						}
					}
					hidden := tc.removal == "purged_at"
					var detail Recording
					resp := getJSON(t, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id), &detail)
					if hidden {
						if resp.StatusCode != http.StatusNotFound {
							t.Fatalf("detail status=%d, want 404", resp.StatusCode)
						}
					} else {
						if resp.StatusCode != http.StatusOK {
							t.Fatalf("detail status=%d", resp.StatusCode)
						}
						if !reflect.DeepEqual(detail.HlsPlayback, want) {
							t.Errorf("detail hlsPlayback=%#v, want %#v", detail.HlsPlayback, want)
						}
					}
					var list []Recording
					listURL := srv.URL + "/api/recordings"
					if tc.removal == "deleted_at" || tc.removal == "purged_at" {
						listURL += "?trash=true"
					}
					if resp := getJSON(t, listURL, &list); resp.StatusCode != http.StatusOK {
						t.Fatalf("list status=%d", resp.StatusCode)
					}
					found := false
					for _, rec := range list {
						if rec.Id == id {
							found = true
							if !reflect.DeepEqual(rec.HlsPlayback, want) {
								t.Errorf("list hlsPlayback=%#v, want %#v", rec.HlsPlayback, want)
							}
						}
					}
					if found == hidden {
						t.Errorf("list contains recording=%t, hidden=%t", found, hidden)
					}
				})
			}
		})
	}
}
