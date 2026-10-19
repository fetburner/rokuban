package pgstore

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var mediaContractEventID atomic.Int32

func TestMediaStoreContract(t *testing.T) {
	contracttest.RunMedia(t, func(t *testing.T) contracttest.MediaFixture {
		pool := testutil.SetupDB(t)
		newRecording := func(t *testing.T) int64 {
			t.Helper()
			id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
				Source:            "manual",
				Site:              db.DefaultSite,
				NetworkID:         32678,
				ServiceID:         5168,
				EventID:           mediaContractEventID.Add(1),
				ServiceName:       "store contract fixture",
				ChannelType:       "GR",
				Channel:           "27",
				Title:             "store media contract fixture",
				ProgramStartAt:    time.Now().UTC().Truncate(time.Second),
				ProgramDurationMs: 60_000,
				Status:            "finished",
			})
			if err != nil {
				t.Fatalf("seeding recording: %v", err)
			}
			return id
		}
		newAsset := func(t *testing.T, recordingID int64, kind string, profile *string, relPath, state string) int64 {
			t.Helper()
			id, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
				RecordingID: recordingID,
				Kind:        kind,
				Profile:     profile,
				RelPath:     relPath,
				SizeBytes:   1,
			})
			if err != nil {
				t.Fatalf("seeding %s media asset: %v", kind, err)
			}
			if state != "active" {
				if _, err := pool.Exec(context.Background(), "UPDATE media_assets SET state = $1 WHERE id = $2", state, id); err != nil {
					t.Fatalf("setting media asset state to %s: %v", state, err)
				}
			}
			return id
		}
		return contracttest.MediaFixture{
			Serving:      NewMedia(pool),
			Reconcile:    NewMedia(pool),
			Storage:      NewMedia(pool),
			NewRecording: newRecording,
			NewAsset:     newAsset,
			SetAssetState: func(t *testing.T, assetID int64, state string) {
				t.Helper()
				if _, err := pool.Exec(context.Background(), "UPDATE media_assets SET state = $1 WHERE id = $2", state, assetID); err != nil {
					t.Fatalf("setting media asset state to %s: %v", state, err)
				}
			},
			SetMediaAssetKeepRanges: func(t *testing.T, assetID int64, keep []chapters.Range) {
				t.Helper()
				param, err := db.KeepRangesParam(keep)
				if err != nil {
					t.Fatalf("encoding frozen keep ranges: %v", err)
				}
				if err := sqlcgen.New(pool).InsertMediaAssetCuts(context.Background(), sqlcgen.InsertMediaAssetCutsParams{
					MediaAssetID: assetID,
					KeepRanges:   param,
				}); err != nil {
					t.Fatalf("seeding frozen keep ranges: %v", err)
				}
			},
			SetEncodedAssetRebuildRequest: func(t *testing.T, recordingID int64, profile string, keep []chapters.Range) {
				t.Helper()
				param, err := db.KeepRangesParam(keep)
				if err != nil {
					t.Fatalf("encoding rebuild request keep ranges: %v", err)
				}
				if err := sqlcgen.New(pool).UpsertEncodedAssetRebuildRequest(context.Background(), sqlcgen.UpsertEncodedAssetRebuildRequestParams{
					RecordingID:   recordingID,
					Profile:       profile,
					RequestedKeep: param,
				}); err != nil {
					t.Fatalf("seeding encoded asset rebuild request: %v", err)
				}
			},
		}
	})
}
