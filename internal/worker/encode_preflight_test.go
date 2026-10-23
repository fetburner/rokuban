package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
)

// TestEncodeWorker_PreflightSkipsWithoutRunningFFmpeg は、公開 tx だけが弾いていた古いジョブを、
// ffmpeg を起動する前に理由つきで飛ばすことを固定する。FFmpeg は存在しないパスなので、
// 起動すれば Work がエラーを返す。encoded 行も scratch も作られない。
func TestEncodeWorker_PreflightSkipsWithoutRunningFFmpeg(t *testing.T) {
	keepOld := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	keepNew := []chapters.Range{{StartMs: 0, EndMs: 2000}}
	tests := []struct {
		name       string
		profiles   []string // desired に入れる profile
		seed       func(t *testing.T, pool *pgxpool.Pool, recordingID int64) jobs.EncodeJobArgs
		wantReason string
	}{
		{
			name:     "rebuild request is gone",
			profiles: []string{"cut"},
			seed: func(t *testing.T, pool *pgxpool.Pool, id int64) jobs.EncodeJobArgs {
				return rebuildJob(t, id, keepOld)
			},
			wantReason: "rebuild_request_missing",
		},
		{
			name:     "rebuild request was overwritten by a later press",
			profiles: []string{"cut"},
			seed: func(t *testing.T, pool *pgxpool.Pool, id int64) jobs.EncodeJobArgs {
				seedEncodedAssetRebuildRequest(t, pool, id, keepNew)
				return rebuildJob(t, id, keepOld)
			},
			wantReason: "rebuild_request_overwritten",
		},
		{
			name:     "profile was removed from desired",
			profiles: []string{"h264"},
			seed: func(t *testing.T, pool *pgxpool.Pool, id int64) jobs.EncodeJobArgs {
				return newCutEncodeJob(id)
			},
			wantReason: "desired_profile_excluded",
		},
		{
			name:     "ordinary cut job must not replace an active cut asset",
			profiles: []string{"cut"},
			seed: func(t *testing.T, pool *pgxpool.Pool, id int64) jobs.EncodeJobArgs {
				adoptChapters(t, pool, id)
				assetID := seedEncodedAsset(t, pool, id, "cut", "preflight/old.g1.mp4")
				if err := setFrozenCuts(t, pool, assetID, keepOld); err != nil {
					t.Fatal(err)
				}
				return newCutEncodeJob(id)
			},
			wantReason: "active_cut_protected",
		},
		{
			// desired にも要求行にも h264 があり、判定を通れば全編を作り直してしまう組み合わせ。
			name:     "rebuild request on a profile that is not cut",
			profiles: []string{"h264"},
			seed: func(t *testing.T, pool *pgxpool.Pool, id int64) jobs.EncodeJobArgs {
				if err := sqlcgen.New(pool).UpsertEncodedAssetRebuildRequest(context.Background(), sqlcgen.UpsertEncodedAssetRebuildRequestParams{
					RecordingID:   id,
					Profile:       "h264",
					RequestedKeep: mustKeepRanges(t, keepOld),
				}); err != nil {
					t.Fatal(err)
				}
				job := rebuildJob(t, id, keepOld)
				job.Profile = "h264"
				return job
			},
			wantReason: "rebuild_request_on_non_cut_profile",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := setupTestPool(t)
			if pool == nil {
				return
			}
			ctx := context.Background()
			mediaDir, scratchDir := t.TempDir(), t.TempDir()
			recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "preflight/rec.m2ts", tt.profiles, []byte("original"))
			job := tt.seed(t, pool, recordingID)

			w := &EncodeWorker{
				Publisher:  newTestMediaStore(pool),
				Progress:   newTestMediaStore(pool),
				Attempts:   newTestMediaStore(pool),
				MediaDir:   mediaDir,
				ScratchDir: scratchDir,
				FFmpeg:     filepath.Join(t.TempDir(), "must-not-run-ffmpeg"),
				Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{
					cutFFmpegProfile(),
					{Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"},
				}},
			}
			w.Media = newTestMediaStore(pool)
			assetsBefore := countEncodedAssets(t, pool, recordingID)

			var logBuf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			if err := runWorkerViaJobQFake(ctx, w, job); err != nil {
				t.Fatalf("Work = %v, want nil (skip without running ffmpeg)", err)
			}
			if !strings.Contains(logBuf.String(), "reason="+tt.wantReason) {
				t.Errorf("log %q does not include reason=%s", logBuf.String(), tt.wantReason)
			}
			if got := countEncodedAssets(t, pool, recordingID); got != assetsBefore {
				t.Errorf("encoded assets = %d, want %d unchanged", got, assetsBefore)
			}
			// ffmpeg を起動する経路は scratch ディレクトリを作る。
			if entries, err := os.ReadDir(scratchDir); err != nil || len(entries) != 0 {
				t.Errorf("scratch dir = %v (err %v), want empty", entries, err)
			}
		})
	}
}

// TestEncodeWorker_OrdinaryCutJobWithoutOwnershipFails は、要求に由来しない cut ジョブが
// 章を所有していない録画に来たら、黙って飛ばさずジョブの失敗にすることを固定する。
// 投入側が所有していない録画を除くので、来るのは想定外の状態である。失敗として見えるようにする。
func TestEncodeWorker_OrdinaryCutJobWithoutOwnershipFails(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "preflight/unowned.m2ts", []string{"cut"}, []byte("original"))
	w := &EncodeWorker{
		Media:    newTestMediaStore(pool),
		Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{cutFFmpegProfile()}},
	}
	_, _, _, skip, err := w.observeEncodePlan(context.Background(), newCutEncodeJob(recordingID), slog.Default())
	if err == nil || skip || !strings.Contains(err.Error(), "requires adopted chapters") {
		t.Fatalf("observeEncodePlan = skip %v, err %v; want a requires-adopted-chapters failure", skip, err)
	}
}

func rebuildJob(t *testing.T, recordingID int64, keep []chapters.Range) jobs.EncodeJobArgs {
	t.Helper()
	raw, err := json.Marshal(keep)
	if err != nil {
		t.Fatal(err)
	}
	job := newCutEncodeJob(recordingID)
	job.RequestedCutKeep = raw
	return job
}

func adoptChapters(t *testing.T, pool *pgxpool.Pool, recordingID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, recordingID); err != nil {
		t.Fatal(err)
	}
}

func countEncodedAssets(t *testing.T, pool *pgxpool.Pool, recordingID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'encoded'`, recordingID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
