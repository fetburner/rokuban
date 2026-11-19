package ingest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestIngestWorker_ClampsCutOnlyProfileSelection は受け入れの「マージ結果が cut
// のみになったら cut が落ちる」を、4 経路のうちの ingest の凍結で固定する。
//
// ルール単独（cut + h264）と override 単独（h264 を明示的に打ち消す）はどちらも
// 禁則を満たしているが、マージ結果は cut だけになる。**書く前に安全側へ倒さないと
// cut だけの録画が凍結され、確認に再生が要り・再生に encode が要り・encode に確認が
// 要る循環に落ちる。**
func TestIngestWorker_ClampsCutOnlyProfileSelection(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	programID := int64(900000000000007)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "cut だけになる予約番組")
	setReservationBase(t, pool, res.ProgramID, `{"keepOriginal":"always","encodeProfiles":["cut","h264"]}`)

	overrides, err := json.Marshal(map[string]any{
		// h264 を明示的に打ち消す → 実効値は cut だけ。
		"encodeProfiles": []string{"cut"},
	})
	testutil.RequireNoError(t, err, "json.Marshal")
	_, err = q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID, Overrides: overrides,
	})
	testutil.RequireNoError(t, err, "setting override")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM program_overrides WHERE site = $1 AND program_id = $2", "default", programID)
	})

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Source, fixtureParams.NetworkID, fixtureParams.ServiceID, fixtureParams.EventID = "rule", 32736, 1024, int32(programID%100000)
	})
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-cut-only", programID)

	fake := newFullTransferFake(t, "rec-policy-cut-only", makeTSData(20), "test/policy-cut-only.m2ts")
	mc := mirakc.NewClient(fake.URL(), nil)

	w := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mc),
		MediaDir:      t.TempDir(),
		RecordTarget:  pgstoretest.Recordings(pool),
		StallTimeout:  5 * time.Second,
		// cut: true のプロファイル名（本番では config から注入される）。
		CutProfiles: map[string]struct{}{"cut": {}},
		Precheck:    pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	job := jobs.IngestJobArgs{Site: "default", RecordID: "rec-policy-cut-only"}
	w.Enqueuer = jobqfaketest.NewRecordingEnqueuer()
	workCtx := context.Background()
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(workCtx, w, job), "Work() error")

	keepOriginal, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if keepOriginal != "always" {
		t.Errorf("keep_original = %q, want always", keepOriginal)
	}
	// cut が落ちて空になる（cut でないプロファイルが 1 つも無いので、安全側は
	// 「エンコードしない」）。空のまま凍結されることが要点で、cut が残っては
	// ならない。
	for _, p := range profiles {
		if p == "cut" {
			t.Errorf("encode_profiles = %v, want cut dropped", profiles)
		}
	}
}

// TestIngestWorker_PreservesCutOnlyProfileSelectionWithLive は原本 HLS が使える構成で
// 凍結時のクランプが cut profile を落とさないことを固定する。
func TestIngestWorker_PreservesCutOnlyProfileSelectionWithLive(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)

	programID := int64(900000000000008)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "live 有効で cut だけになる予約番組")
	setReservationBase(t, pool, res.ProgramID, `{"keepOriginal":"always","encodeProfiles":["cut","h264"]}`)

	overrides, err := json.Marshal(map[string]any{"encodeProfiles": []string{"cut"}})
	testutil.RequireNoError(t, err, "json.Marshal")
	_, err = q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: "default", ProgramID: programID, Overrides: overrides,
	})
	testutil.RequireNoError(t, err, "setting override")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM program_overrides WHERE site = $1 AND program_id = $2", "default", programID)
	})

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Source, fixtureParams.NetworkID, fixtureParams.ServiceID, fixtureParams.EventID = "rule", 32736, 1024, int32(programID%100000)
	})
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-policy-cut-only-live", programID)

	fake := newFullTransferFake(t, "rec-policy-cut-only-live", makeTSData(20), "test/policy-cut-only-live.m2ts")
	mc := mirakc.NewClient(fake.URL(), nil)
	w := &IngestWorker{
		Publisher:           pgstoretest.Media(pool),
		MirakcClients:       singleSiteClients("", mc),
		MediaDir:            t.TempDir(),
		RecordTarget:        pgstoretest.Recordings(pool),
		StallTimeout:        5 * time.Second,
		CutProfiles:         map[string]struct{}{"cut": {}},
		RecordingHLSEnabled: true,
		Precheck:            pgstoretest.Media(pool),
	}
	w.CMDetectionStore = pgstoretest.Analysis(pool)

	job := jobs.IngestJobArgs{Site: "default", RecordID: "rec-policy-cut-only-live"}
	w.Enqueuer = jobqfaketest.NewRecordingEnqueuer()
	workCtx := context.Background()
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(workCtx, w, job), "Work() error")

	_, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if len(profiles) != 1 || profiles[0] != "cut" {
		t.Errorf("encode_profiles = %v, want [cut] with live enabled", profiles)
	}
}
