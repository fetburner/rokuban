package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestValidateMediaAssetRelPathNamespace(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		profile *string
		relPath string
		state   string
		wantErr bool
	}{
		{
			name:    "site-prefixed original is accepted",
			kind:    store.AssetKindOriginal,
			relPath: "sites/home/archive/show.m2ts",
			state:   "active",
		},
		{
			name:    "site-prefixed encoded is accepted",
			kind:    store.AssetKindEncoded,
			profile: stringPtr("h264"),
			relPath: "sites/home/archive/show_h264.mp4",
			state:   "active",
		},
		{
			name:    "deleted bare original is ignored",
			kind:    store.AssetKindOriginal,
			relPath: "archive/deleted.m2ts",
			state:   "deleted",
		},
		{
			name:    "active bare original is rejected",
			kind:    store.AssetKindOriginal,
			relPath: "archive/old.m2ts",
			state:   "active",
			wantErr: true,
		},
		{
			// classifySiteForRescuedFile (internal/catalog/rescue_scan.go) requires
			// a "/" after the site segment to extract a site name; a path that
			// starts with the literal "sites/" but has no site segment cannot be
			// rescued, so the startup guard must reject it too even though it
			// begins with "sites/%".
			name:    "sites-prefixed original without a site segment is rejected",
			kind:    store.AssetKindOriginal,
			relPath: "sites/show.m2ts",
			state:   "active",
			wantErr: true,
		},
		{
			// Same reason, with the empty site segment followed by more path:
			// LIKE 'sites/_%/%' lets this through because "_" also matches "/".
			name:    "empty site segment is rejected",
			kind:    store.AssetKindOriginal,
			relPath: "sites//archive/show.m2ts",
			state:   "active",
			wantErr: true,
		},
		{
			name:    "deleting bare encoded is rejected",
			kind:    store.AssetKindEncoded,
			profile: stringPtr("h264"),
			relPath: "archive/old_h264.mp4",
			state:   "deleting",
			wantErr: true,
		},
		{
			name:    "bare thumbnail is ignored",
			kind:    store.AssetKindThumbnail,
			relPath: "thumbnails/1.jpg",
			state:   "active",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx := context.Background()
			recordingID := dbtest.Recording(t, pool, func(fixtureParams *sqlcgen.CreateRecordingParams) {
				fixtureParams.Site = "home"
				fixtureParams.NetworkID = 1
				fixtureParams.ServiceID = 1
				fixtureParams.EventID = 1
				fixtureParams.ServiceName = "test"
				fixtureParams.Channel = "1"
				fixtureParams.Title = "test"
				fixtureParams.ProgramStartAt = time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
				fixtureParams.ProgramDurationMs = 60_000
			})

			assetID := dbtest.OriginalAsset(t, pool, recordingID, func(fixtureParams *sqlcgen.CreateMediaAssetParams) {
				fixtureParams.Kind = tt.kind
				fixtureParams.Profile = tt.profile
				fixtureParams.RelPath = tt.relPath
				fixtureParams.SizeBytes = 1
			})

			if tt.state != "active" {
				if _, err := pool.Exec(ctx, `UPDATE media_assets SET state = $1 WHERE id = $2`, tt.state, assetID); err != nil {
					t.Fatalf("updating media asset state: %v", err)
				}
			}

			err := ValidateMediaAssetRelPathNamespace(ctx, pgstore.NewMedia(pool))
			if tt.wantErr {
				if err == nil {
					t.Fatal("ValidateMediaAssetRelPathNamespace returned nil")
				}
				if !strings.Contains(err.Error(), tt.relPath) || !strings.Contains(err.Error(), "sites/{site}/") {
					t.Errorf("error = %v, want the offending path and required prefix", err)
				}
			} else if err != nil {
				t.Errorf("ValidateMediaAssetRelPathNamespace: %v", err)
			}
		})
	}
}

func stringPtr(s string) *string { return &s }
