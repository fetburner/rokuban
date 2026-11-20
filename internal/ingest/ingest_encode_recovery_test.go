package ingest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/encode"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestEncodeReconcile_ReenqueuesAfterLostHintAndDeletedEdgeRecord は issue #163 の
// 回帰テスト。塞いだ穴そのものを端から端まで再現する:
//
//  1. ingest は成功してコミットする（原本 media_asset + recording_processing_policy）
//  2. しかし encode 投入のヒントは飛ばない（enqueueEncodeReconcileFromContext は
//     Enqueuer が無いと黙って return する。ここでは worker に Enqueuer を注入せず
//     その状態を作る。「ヒント投入に失敗して
//     ログだけ出た」場合と DB から見た結果は同じ ---どちらも encode ジョブが
//     1 件も入っていない）
//  3. エッジ record の削除は成功する（＝ヒントを再送する唯一の経路だった
//     record_sweep → processRecord → ingest ジョブ再投入がもう起きない。
//     mirakc 側にその record が無いため）
//
// この状態でコミット済みの録画が黙ってエンコードされないままになるのが
// issue #163 の穴で、定期パスがそれを埋めることを最後に確認する。
func TestEncodeReconcile_ReenqueuesAfterLostHintAndDeletedEdgeRecord(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	programID := int64(900000000163001)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "ヒントを落とす番組")
	setReservationBase(t, pool, res.ProgramID, `{"keepOriginal":"always","encodeProfiles":["h265"]}`)

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Source, fixtureParams.NetworkID, fixtureParams.ServiceID, fixtureParams.EventID = "rule", 32736, 1024, int32(programID%100000)
	})
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-163-lost-hint", programID)

	tsData := makeTSData(20)
	fake := mirakctest.New(t)
	record := mirakc.Record{
		ID: "rec-163-lost-hint",
		Recording: mirakc.RecordInfo{
			Status:  "finished",
			Options: mirakc.Options{ContentPath: strPtr("test/lost-hint.m2ts")},
		},
		Content: mirakc.ContentInfo{Path: "/recording/test/lost-hint.m2ts", Sha256: strPtr(sha256Hex(tsData))},
	}
	fake.SetRecords([]mirakc.Record{record})
	fake.SetRecordStream(record.ID, tsData)

	iw := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
		MediaDir:      t.TempDir(),
		Config:        Config{StallTimeout: 5 * time.Second},
		Stores: Stores{
			RecordTarget: pgstoretest.Recordings(pool),
			Precheck:     pgstoretest.Media(pool),
		},
	}
	iw.CMDetectionStore = pgstoretest.Analysis(pool)
	job := jobs.IngestJobArgs{Site: "default", RecordID: "rec-163-lost-hint"}

	// Enqueuer を注入しない状態 = ヒントが飛ばない。
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), iw, job), "IngestWorker.Work")

	// 前提 1: desired は凍結された（この録画は h265 をエンコードすべき）。
	if _, profiles := encodePolicyOfRecording(t, pool, recordingID); !slices.Equal(profiles, []string{"h265"}) {
		t.Fatalf("encode_profiles = %v, want [h265]", profiles)
	}
	// 前提 2: エッジ record は消えた（ヒントを再送する経路が閉じた）。
	if fake.CallCount("DELETE", "/api/recording/records/rec-163-lost-hint") == 0 {
		t.Fatal("edge record was not deleted; the lost-hint scenario needs the re-hint path closed")
	}
	// 定期パスが差分を埋める。
	w := &encode.EncodeReconcileWorker{
		Enqueuer: jobqfaketest.NewRecordingEnqueuer(),
		Profiles: encodeConfigWith("h265"),
		Media:    pgstoretest.Media(pool),
	}
	// 前提 3: ヒントは落ちた（encode ジョブが 1 件も入っていない）。
	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), recordingID, "h265"); got != 0 {
		t.Fatalf("encode jobs before the periodic pass = %d, want 0 (the hint must be lost for this regression to mean anything)", got)
	}

	runEncodeReconcilePass(t, w)

	if got := countEncodeJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), recordingID, "h265"); got != 1 {
		t.Errorf("encode jobs after the periodic pass = %d, want 1", got)
	}
}
