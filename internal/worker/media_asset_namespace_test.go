package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
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
			kind:    db.AssetKindOriginal,
			relPath: "sites/home/archive/show.m2ts",
			state:   "active",
		},
		{
			name:    "site-prefixed encoded is accepted",
			kind:    db.AssetKindEncoded,
			profile: stringPtr("h264"),
			relPath: "sites/home/archive/show_h264.mp4",
			state:   "active",
		},
		{
			name:    "deleted bare original is ignored",
			kind:    db.AssetKindOriginal,
			relPath: "archive/deleted.m2ts",
			state:   "deleted",
		},
		{
			name:    "active bare original is rejected",
			kind:    db.AssetKindOriginal,
			relPath: "archive/old.m2ts",
			state:   "active",
			wantErr: true,
		},
		{
			name:    "deleting bare encoded is rejected",
			kind:    db.AssetKindEncoded,
			profile: stringPtr("h264"),
			relPath: "archive/old_h264.mp4",
			state:   "deleting",
			wantErr: true,
		},
		{
			name:    "bare thumbnail is ignored",
			kind:    db.AssetKindThumbnail,
			relPath: "thumbnails/1.jpg",
			state:   "active",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx := context.Background()
			q := sqlcgen.New(pool)
			recordingID, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
				Source:            "manual",
				Site:              "home",
				NetworkID:         1,
				ServiceID:         1,
				EventID:           1,
				ServiceName:       "test",
				ChannelType:       "GR",
				Channel:           "1",
				Title:             "test",
				IsFree:            true,
				ProgramStartAt:    time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
				ProgramDurationMs: 60_000,
				Status:            "finished",
			})
			if err != nil {
				t.Fatalf("CreateRecording: %v", err)
			}
			assetID, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
				RecordingID: recordingID,
				Kind:        tt.kind,
				Profile:     tt.profile,
				RelPath:     tt.relPath,
				SizeBytes:   1,
			})
			if err != nil {
				t.Fatalf("CreateMediaAsset: %v", err)
			}
			if tt.state != "active" {
				if _, err := pool.Exec(ctx, `UPDATE media_assets SET state = $1 WHERE id = $2`, tt.state, assetID); err != nil {
					t.Fatalf("updating media asset state: %v", err)
				}
			}

			err = ValidateMediaAssetRelPathNamespace(ctx, pool)
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
