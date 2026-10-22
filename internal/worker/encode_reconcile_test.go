package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
)

// encodeConfigWith は名前だけが意味を持つ encode 設定を作る（このパスは
// プロファイル名しか見ない --- ffmpeg は起動しない）。
func encodeConfigWith(names ...string) config.EncodeConfig {
	profiles := make([]config.EncodeProfile, 0, len(names))
	for _, n := range names {
		profiles = append(profiles, config.EncodeProfile{
			Name: n, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
		})
	}
	return config.EncodeConfig{Profiles: profiles}
}

// runEncodeReconcilePass runs one pass and records the Args it asks the queue to deliver.
func runEncodeReconcilePass(t *testing.T, w *EncodeReconcileWorker) *recordingEnqueuer {
	return runEncodeReconcilePassArgs(t, w, EncodeReconcileArgs{})
}

func runEncodeReconcilePassArgs(t *testing.T, w *EncodeReconcileWorker, args EncodeReconcileArgs) *recordingEnqueuer {
	t.Helper()
	if w.Media == nil {
		t.Fatal("EncodeReconcileWorker.Media is nil")
	}
	enqueuer, ok := w.Enqueuer.(*recordingEnqueuer)
	if !ok {
		enqueuer = newRecordingEnqueuer()
		w.Enqueuer = enqueuer
	}
	if err := runWorkerViaJobQFake(context.Background(), w, args); err != nil {
		t.Fatalf("EncodeReconcileWorker.Work: %v", err)
	}
	return enqueuer
}

func findEncodeJob(t *testing.T, enqueuer *recordingEnqueuer, recordingID int64, profile string) jobs.EncodeJobArgs {
	t.Helper()
	for _, args := range enqueuedArgs[jobs.EncodeJobArgs](enqueuer, jobs.EncodeJobArgs{}.Kind()) {
		if args.RecordingID == recordingID && args.Profile == profile {
			return args
		}
	}
	t.Fatalf("no encode job for recording %d profile %q", recordingID, profile)
	return jobs.EncodeJobArgs{}
}

func TestEncodeReconcile_TargetedPassOnlyQueuesSelectedRecordingAndLeavesFullStateAlone(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	first := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "targeted/first.m2ts", []string{"h265"}, []byte("x"))
	target := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "targeted/target.m2ts", []string{"h265"}, []byte("x"))
	third := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "targeted/third.m2ts", []string{"h265"}, []byte("x"))

	metrics.EncodeReconcileCandidates.Set(17)
	metrics.EncodeReconcileLastPass.Set(23)
	metrics.EncodeReconcileUnsatisfiable.WithLabelValues("removed-profile").Set(31)
	metrics.CutAwaitingReview.WithLabelValues("cut").Set(37)
	t.Cleanup(func() {
		metrics.EncodeReconcileCandidates.Set(0)
		metrics.EncodeReconcileLastPass.Set(0)
		metrics.EncodeReconcileUnsatisfiable.WithLabelValues("removed-profile").Set(0)
		metrics.CutAwaitingReview.WithLabelValues("cut").Set(0)
	})

	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), RowLimit: 1, Media: newTestMediaStore(pool)}
	runEncodeReconcilePassArgs(t, w, EncodeReconcileArgs{RecordingID: target})

	for _, tc := range []struct {
		id   int64
		want int
	}{{first, 0}, {target, 1}, {third, 0}} {
		if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), tc.id, "h265"); got != tc.want {
			t.Errorf("encode jobs for recording %d = %d, want %d", tc.id, got, tc.want)
		}
	}
	if got := len(enqueuedArgs[EncodeReconcileArgs](w.Enqueuer.(*recordingEnqueuer), "encode_reconcile")); got != 0 {
		t.Errorf("targeted pass continuation jobs = %d, want 0", got)
	}
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileCandidates); got != 17 {
		t.Errorf("candidate gauge = %v, want unchanged 17", got)
	}
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileLastPass); got != 23 {
		t.Errorf("last-pass gauge = %v, want unchanged 23", got)
	}
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileUnsatisfiable.WithLabelValues("removed-profile")); got != 31 {
		t.Errorf("unsatisfiable gauge = %v, want unchanged 31", got)
	}
	if got := promtestutil.ToFloat64(metrics.CutAwaitingReview.WithLabelValues("cut")); got != 37 {
		t.Errorf("awaiting-review gauge = %v, want unchanged 37", got)
	}
}

// TestEncodeReconcile_QueuesUnsatisfiedRebuildsAndKeepsEveryRow は、reconcile が
// 満たされていない作り直しだけに押した時点の keep のジョブを投入し、どの要求行も
// 消さないことを固定する。チャプターの編集・所有の欠落・満たされた状態のどれでも
// 行は残る（消すのは api だけ）。
func TestEncodeReconcile_QueuesUnsatisfiedRebuildsAndKeepsEveryRow(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	staleFrozen := []chapters.Range{{StartMs: 0, EndMs: 1800000}}
	seedCut := func(name string, frozen []chapters.Range) (int64, []chapters.Range, int64) {
		t.Helper()
		id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, name, []string{"cut"}, []byte("original"))
		if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, id); err != nil {
			t.Fatalf("adopting chapters for %d: %v", id, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(600000, 900000), 'CM', true)`, id); err != nil {
			t.Fatalf("seeding chapters for %d: %v", id, err)
		}
		keep, owned, err := newTestMediaStore(pool).GetCurrentCutKeep(ctx, id)
		if err != nil || !owned || len(keep) == 0 {
			t.Fatalf("currentCutKeep(%d) = %v, owned %v, err %v", id, keep, owned, err)
		}
		assetID := seedEncodedAsset(t, pool, id, "cut", name+"_cut.mp4")
		if err := setFrozenCuts(t, pool, assetID, frozen); err != nil {
			t.Fatalf("seeding frozen cut for %d: %v", id, err)
		}
		return id, keep, assetID
	}
	unsatisfied, unsatisfiedKeep, _ := seedCut("rebuild/unsatisfied.m2ts", staleFrozen)
	seedEncodedAssetRebuildRequest(t, pool, unsatisfied, unsatisfiedKeep)

	// 押した後にチャプターを編集した（requested_keep は今のチャプターと違う）。
	edited, _, _ := seedCut("rebuild/edited.m2ts", staleFrozen)
	editedRequested := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	seedEncodedAssetRebuildRequest(t, pool, edited, editedRequested)

	// 所有の行が無い。押した時点の keep が確認なので、所有は投入の条件にしない。
	unowned, unownedKeep, _ := seedCut("rebuild/unowned.m2ts", staleFrozen)
	seedEncodedAssetRebuildRequest(t, pool, unowned, unownedKeep)
	if _, err := pool.Exec(ctx, `DELETE FROM recording_chapter_ownership WHERE recording_id = $1`, unowned); err != nil {
		t.Fatalf("resetting chapter ownership: %v", err)
	}

	// 凍結区間が requested_keep と等しい = 満たされている。
	satisfied, satisfiedKeep, assetID := seedCut("rebuild/satisfied.m2ts", staleFrozen)
	if err := setFrozenCuts(t, pool, assetID, satisfiedKeep); err != nil {
		t.Fatalf("freezing satisfied cut: %v", err)
	}
	seedEncodedAssetRebuildRequest(t, pool, satisfied, satisfiedKeep)

	cutProfile := config.EncodeProfile{Name: "cut", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"}
	w := &EncodeReconcileWorker{Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{cutProfile}}, Media: newTestMediaStore(pool)}
	enqueuer := runEncodeReconcilePass(t, w)

	wantKeep := map[int64][]chapters.Range{unsatisfied: unsatisfiedKeep, edited: editedRequested, unowned: unownedKeep}
	for id, want := range wantKeep {
		if got := countEncodeJobs(t, enqueuer, id, "cut"); got != 1 {
			t.Fatalf("encode jobs for recording %d = %d, want 1", id, got)
		}
		for _, call := range enqueuedArgs[jobs.EncodeJobArgs](enqueuer, jobs.EncodeJobArgs{}.Kind()) {
			if call.RecordingID != id {
				continue
			}
			var queued []chapters.Range
			if err := json.Unmarshal(call.RequestedCutKeep, &queued); err != nil {
				t.Fatalf("decoding queued requested keep for %d: %v", id, err)
			}
			if !chapters.SameRanges(queued, want) {
				t.Errorf("recording %d queued keep = %v, want the requested %v", id, queued, want)
			}
		}
	}
	if got := countEncodeJobs(t, enqueuer, satisfied, "cut"); got != 0 {
		t.Errorf("encode jobs for satisfied request = %d, want 0", got)
	}
	for _, id := range []int64{unsatisfied, edited, unowned, satisfied} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM encoded_asset_rebuild_requests WHERE recording_id = $1`, id).Scan(&count); err != nil {
			t.Fatalf("counting rebuild requests for %d: %v", id, err)
		}
		if count != 1 {
			t.Errorf("rebuild request rows for recording %d = %d, want 1 (only api removes them)", id, count)
		}
	}
}

func TestEncodeReconcile_PendingRebuildWithoutAssetQueuesOnlyBoundJob(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "rebuild/missing-asset.m2ts", []string{"cut"}, []byte("original"))
	if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, recordingID); err != nil {
		t.Fatalf("adopting chapters: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(600000, 900000), 'CM', true)`, recordingID); err != nil {
		t.Fatalf("seeding chapter span: %v", err)
	}
	keep, owned, err := newTestMediaStore(pool).GetCurrentCutKeep(ctx, recordingID)
	if err != nil || !owned || len(keep) == 0 {
		t.Fatalf("currentCutKeep = %v, owned %v, err %v", keep, owned, err)
	}
	seedEncodedAssetRebuildRequest(t, pool, recordingID, keep)

	enqueuer := newRecordingEnqueuer()
	cutProfile := config.EncodeProfile{Name: "cut", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"}
	w := &EncodeReconcileWorker{
		Enqueuer: enqueuer,
		Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{cutProfile}},
		Media:    newTestMediaStore(pool),
	}
	runEncodeReconcilePassArgs(t, w, EncodeReconcileArgs{RecordingID: recordingID})

	var got []jobs.EncodeJobArgs
	for _, args := range enqueuedArgs[jobs.EncodeJobArgs](enqueuer, jobs.EncodeJobArgs{}.Kind()) {
		if args.RecordingID == recordingID && args.Profile == "cut" {
			got = append(got, args)
		}
	}
	if len(got) != 1 {
		t.Fatalf("queued cut jobs = %d, want exactly one request-bound job", len(got))
	}
	requestedKeep, present, err := decodeRequestedCutKeep(got[0].RequestedCutKeep)
	if err != nil || !present || !chapters.SameRanges(requestedKeep, keep) {
		t.Errorf("queued request keep = %v, present %v, err %v; want %v", requestedKeep, present, err, keep)
	}
}

func TestEncodeReconcile_RebuildsPrecedeMissingProfilesAndSortByProfile(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "rebuild/order.m2ts", []string{"h264", "cut-b", "cut-a"}, []byte("x"))
	for profile, keep := range map[string][]chapters.Range{
		"cut-b": {{StartMs: 2000, EndMs: 3000}},
		"cut-a": {{StartMs: 1000, EndMs: 2000}},
	} {
		if err := sqlcgen.New(pool).UpsertEncodedAssetRebuildRequest(ctx, sqlcgen.UpsertEncodedAssetRebuildRequestParams{
			RecordingID:   recordingID,
			Profile:       profile,
			RequestedKeep: mustKeepRanges(t, keep),
		}); err != nil {
			t.Fatalf("seeding %s rebuild request: %v", profile, err)
		}
	}
	profiles := config.EncodeConfig{Profiles: []config.EncodeProfile{
		{Name: "cut-b", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"},
		{Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"},
		{Name: "cut-a", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"},
	}}
	w := &EncodeReconcileWorker{Profiles: profiles, RowLimit: 1, Media: newTestMediaStore(pool)}
	enqueuer := runEncodeReconcilePass(t, w)
	got := enqueuedArgs[jobs.EncodeJobArgs](enqueuer, jobs.EncodeJobArgs{}.Kind())
	want := []string{"cut-a", "cut-b", "h264"}
	if len(got) != len(want) {
		t.Fatalf("encode jobs = %+v, want profile order %v", got, want)
	}
	for i := range got {
		if got[i].RecordingID != recordingID || got[i].Profile != want[i] {
			t.Errorf("encode job %d = (%d, %q), want (%d, %q)", i, got[i].RecordingID, got[i].Profile, recordingID, want[i])
		}
	}
}

func mustKeepRanges(t *testing.T, keep []chapters.Range) pgstore.KeepRanges {
	t.Helper()
	ranges, err := pgstore.KeepRangesParam(keep)
	if err != nil {
		t.Fatalf("encoding requested keep ranges: %v", err)
	}
	return ranges
}

// TestUnsatisfiedEncodeRebuilds_MatchesActiveAssetOfSameProfileOnly は、満たされた判定が
// 同じプロファイルの active な encoded だけを見ることを固定する。別の cut プロファイルの
// 版の凍結区間が requested_keep と同じでも、未充足のままである（満たされた扱いにすると
// until_encoded の原本が不可逆に消える）。
func TestUnsatisfiedEncodeRebuilds_MatchesActiveAssetOfSameProfileOnly(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "rebuild/other-profile.m2ts", []string{"cut", "cut2"}, []byte("original"))
	keep := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	seedEncodedAssetRebuildRequest(t, pool, id, keep)
	other := seedEncodedAsset(t, pool, id, "cut2", "rebuild/other-profile_cut2.mp4")
	if err := setFrozenCuts(t, pool, other, keep); err != nil {
		t.Fatal(err)
	}
	if got := unsatisfiedRebuildCount(t, pool, id); got != 1 {
		t.Fatalf("unsatisfied rebuilds with only another profile's matching asset = %d, want 1", got)
	}
	same := seedEncodedAsset(t, pool, id, "cut", "rebuild/other-profile_cut.mp4")
	if err := setFrozenCuts(t, pool, same, keep); err != nil {
		t.Fatal(err)
	}
	if got := unsatisfiedRebuildCount(t, pool, id); got != 0 {
		t.Fatalf("unsatisfied rebuilds with the same profile's matching asset = %d, want 0", got)
	}
}

// TestEncodeReconcile_TargetedPassSkipsTrashedUntilRestoredFullPass は、対象パスが
// ごみ箱の録画を除外し、復元後の全件パスが不足分を投入することを固定する。
func TestEncodeReconcile_TargetedPassSkipsTrashedUntilRestoredFullPass(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "targeted-trash/original.m2ts", []string{"h265"}, []byte("x"))
	if _, err := sqlcgen.New(pool).SoftDeleteRecording(ctx, recordingID); err != nil {
		t.Fatalf("soft deleting recording: %v", err)
	}

	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), Media: newTestMediaStore(pool)}
	runEncodeReconcilePassArgs(t, w, EncodeReconcileArgs{RecordingID: recordingID})
	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID, "h265"); got != 0 {
		t.Fatalf("targeted encode jobs for trashed recording = %d, want 0", got)
	}

	if _, err := sqlcgen.New(pool).RestoreRecording(ctx, recordingID); err != nil {
		t.Fatalf("restoring recording: %v", err)
	}
	runEncodeReconcilePass(t, w)
	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID, "h265"); got != 1 {
		t.Fatalf("full-pass encode jobs after restore = %d, want 1", got)
	}
}

// TestEncodeReconcile_ReenqueuesAfterLostHintAndDeletedEdgeRecord は issue #163 の
// 回帰テスト。塞いだ穴そのものを端から端まで再現する:
//
//  1. ingest は成功してコミットする（原本 media_asset + recording_encode_policy）
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
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	programID := int64(900000000163001)
	res := insertProgramSnapshotAndReservation(t, pool, programID, "ヒントを落とす番組")
	setReservationBase(t, pool, res.ProgramID, `{"keepOriginal":"always","encodeProfiles":["h265"]}`)

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *sqlcgen.CreateRecordingParams) {
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
		Publisher:     newTestMediaStore(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
		MediaDir:      t.TempDir(),
		RecordTarget:  pgstore.NewRecordings(pool),
		StallTimeout:  5 * time.Second,
		Precheck:      newTestMediaStore(pool),
	}
	iw.CMDetectionStore = pgstore.NewAnalysis(pool)
	job := IngestJobArgs{Site: "default", RecordID: "rec-163-lost-hint"}

	// Enqueuer を注入しない状態 = ヒントが飛ばない。
	if err := runWorkerViaJobQFake(context.Background(), iw, job); err != nil {
		t.Fatalf("IngestWorker.Work: %v", err)
	}

	// 前提 1: desired は凍結された（この録画は h265 をエンコードすべき）。
	if _, profiles := encodePolicyOfRecording(t, pool, recordingID); !slices.Equal(profiles, []string{"h265"}) {
		t.Fatalf("encode_profiles = %v, want [h265]", profiles)
	}
	// 前提 2: エッジ record は消えた（ヒントを再送する経路が閉じた）。
	if fake.CallCount("DELETE", "/api/recording/records/rec-163-lost-hint") == 0 {
		t.Fatal("edge record was not deleted; the lost-hint scenario needs the re-hint path closed")
	}
	// 定期パスが差分を埋める。
	w := &EncodeReconcileWorker{
		Enqueuer: newRecordingEnqueuer(),
		Profiles: encodeConfigWith("h265"),
		Media:    newTestMediaStore(pool),
	}
	// 前提 3: ヒントは落ちた（encode ジョブが 1 件も入っていない）。
	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID, "h265"); got != 0 {
		t.Fatalf("encode jobs before the periodic pass = %d, want 0 (the hint must be lost for this regression to mean anything)", got)
	}

	runEncodeReconcilePass(t, w)

	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID, "h265"); got != 1 {
		t.Errorf("encode jobs after the periodic pass = %d, want 1", got)
	}
}

// TestListMissingEncodeProfiles は不足プロファイルクエリを両方向で見る。
//
// 「投入されないこと」をジョブ数で見ても意味が無いケースがある --- 例えば
// 「原本が無い（ingest 未完了）」は ListMissingEncodeProfiles 側でも弾かれるので、
// クエリから条件を落としてもジョブ数のアサーションは通ってしまう。候補集合
// そのものを見る。
func TestListMissingEncodeProfiles(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)
	mediaDir := t.TempDir()

	// (a) desired があり encoded が無い → 候補。
	missing := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "q/missing.m2ts", []string{"h265"}, []byte("x"))

	// (b) desired が揃っている → 候補にしない。
	complete := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "q/complete.m2ts", []string{"h264"}, []byte("x"))
	h264 := "h264"
	dbtest.OriginalAsset(t, pool, complete, func(fixtureParams *sqlcgen.CreateMediaAssetParams) {
		fixtureParams.Kind = store.AssetKindEncoded
		fixtureParams.Profile = &h264
		fixtureParams.RelPath = "q/complete_h264.mp4"
		fixtureParams.SizeBytes = 1
	})

	// (c) desired が空 → 候補にしない。
	noProfiles := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "q/noprofiles.m2ts", nil, []byte("x"))

	// (d) ingest 未完了（policy はあるが原本 media_asset が無い）→ 候補にしない。
	incomplete := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "q/incomplete.m2ts", []string{"h265"}, []byte("x"))
	if _, err := pool.Exec(ctx, `DELETE FROM media_assets WHERE recording_id = $1`, incomplete); err != nil {
		t.Fatal(err)
	}

	// (e) 原本が物理削除済み（until_encoded の後始末）→ 候補にしない。
	originalGone := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "q/gone.m2ts", []string{"h265"}, []byte("x"))
	if _, err := pool.Exec(ctx,
		`UPDATE media_assets SET state = 'deleted' WHERE recording_id = $1 AND kind = 'original'`,
		originalGone); err != nil {
		t.Fatal(err)
	}

	// (f) ごみ箱の録画 → 候補にしない。
	trashed := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "q/trashed.m2ts", []string{"h265"}, []byte("x"))
	if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, trashed); err != nil {
		t.Fatal(err)
	}

	// (g) encoded の行はあるが state='deleted'（復元後など）→ 候補。
	// ListMissingEncodeProfiles が見るのは active な encoded だけ
	// （GetActiveEncodedMediaAssetID）なので、候補の定義もそこに揃える。
	encodedGone := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "q/encgone.m2ts", []string{"h264"}, []byte("x"))
	dbtest.OriginalAsset(t, pool, encodedGone, func(fixtureParams *sqlcgen.CreateMediaAssetParams) {
		fixtureParams.Kind = store.AssetKindEncoded
		fixtureParams.Profile = &h264
		fixtureParams.RelPath = "q/encgone_h264.mp4"
		fixtureParams.SizeBytes = 1
	})
	if _, err := pool.Exec(ctx,
		`UPDATE media_assets SET state = 'deleted' WHERE recording_id = $1 AND kind = 'encoded'`,
		encodedGone); err != nil {
		t.Fatal(err)
	}

	// (h) desired が現在の設定に無いプロファイル（改名 / 削除された）→ 候補に
	// しない。投入しても EncodeWorker が `unknown encode profile` で弾くだけで
	// 永久に満たされないため、定期パスの投入対象から外す。
	renamed := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "q/renamed.m2ts", []string{"gone"}, []byte("x"))

	// (i) 空文字列のプロファイル名 → 候補にしない。ListMissingEncodeProfiles が
	// 空文字列をスキップするので、候補に挙げると (h) と同じ「永久に満たされない
	// 候補」になる。クエリでも空文字列を明示的に落とす。
	emptyProfile := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "q/empty.m2ts", []string{""}, []byte("x"))

	got, err := q.ListMissingEncodeProfiles(ctx, sqlcgen.ListMissingEncodeProfilesParams{
		KnownProfiles: []string{"h264", "h265"},
		CutProfiles:   []string{},
		AttemptLimit:  jobs.EncodeAttemptLimit,
		RowLimit:      encodeReconcileRowLimit,
	})
	if err != nil {
		t.Fatalf("ListMissingEncodeProfiles: %v", err)
	}
	want := []sqlcgen.ListMissingEncodeProfilesRow{
		{RecordingID: missing, Profile: "h265"},
		{RecordingID: encodedGone, Profile: "h264"},
	}
	if !equalMissingEncodeProfileRows(got, want) {
		t.Errorf("missing encode profiles = %v, want %v (complete=%d noProfiles=%d incomplete=%d originalGone=%d trashed=%d renamed=%d emptyProfile=%d)",
			got, want, complete, noProfiles, incomplete, originalGone, trashed, renamed, emptyProfile)
	}

	// AfterRecordingID（#326 の窓の回転が使う keyset 述語）: missing の id より
	// 後ろだけを見ると、missing 自身は落ちて encodedGone だけが残る。
	if missing >= encodedGone {
		t.Fatalf("expected missing (%d) < encodedGone (%d) for this assertion to mean anything", missing, encodedGone)
	}
	gotAfter, err := q.ListMissingEncodeProfiles(ctx, sqlcgen.ListMissingEncodeProfilesParams{
		AfterRecordingID: missing,
		KnownProfiles:    []string{"h264", "h265"},
		CutProfiles:      []string{},
		AttemptLimit:     jobs.EncodeAttemptLimit,
		RowLimit:         encodeReconcileRowLimit,
	})
	if err != nil {
		t.Fatalf("ListMissingEncodeProfiles with AfterRecordingID: %v", err)
	}
	wantAfter := []sqlcgen.ListMissingEncodeProfilesRow{{RecordingID: encodedGone, Profile: "h264"}}
	if !equalMissingEncodeProfileRows(gotAfter, wantAfter) {
		t.Errorf("missing encode profiles with AfterRecordingID=%d = %v, want %v", missing, gotAfter, wantAfter)
	}

	// 落とした側（(h)/(i)）は数えて見せる --- 黙って落とすと「エンコードされない
	// 録画」が静かに増える。
	unsat, err := q.ListUnsatisfiableEncodeProfiles(ctx, []string{"h264", "h265"})
	if err != nil {
		t.Fatalf("ListUnsatisfiableEncodeProfiles: %v", err)
	}
	gotUnsat := map[string]int64{}
	for _, r := range unsat {
		gotUnsat[r.Profile] = r.Recordings
	}
	if len(gotUnsat) != 2 || gotUnsat["gone"] != 1 || gotUnsat[""] != 1 {
		t.Errorf("unsatisfiable = %v, want {gone:1, \"\":1}", gotUnsat)
	}
}

func equalMissingEncodeProfileRows(got, want []sqlcgen.ListMissingEncodeProfilesRow) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].RecordingID != want[i].RecordingID || got[i].Profile != want[i].Profile ||
			got[i].IsRebuild != want[i].IsRebuild || !slices.Equal(got[i].RequestedKeep, want[i].RequestedKeep) {
			return false
		}
	}
	return true
}

// TestEncodeReconcileWorker_BatchesCandidateReads は、reconcile パスが候補ごとの
// 原本・ポリシー・encoded の再取得をしないことを固定する。enqueue は不足
// プロファイルごとに残るが、読み取りは候補数に比例して増えない。
func TestEncodeReconcileWorker_BatchesCandidateReads(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "batch/1.m2ts", []string{"h264", "h265"}, []byte("x"))
	prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "batch/2.m2ts", []string{"h264", "h265"}, []byte("x"))

	counter := &encodeReconcileQueryCounter{}
	tracedPool := poolWithQueryTracer(t, pool, counter)
	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h264", "h265"), RowLimit: 2, Media: newTestMediaStore(tracedPool)}
	runEncodeReconcilePass(t, w)

	if got := counter.listMissing.Load(); got != 1 {
		t.Errorf("ListMissingEncodeProfiles calls = %d, want 1", got)
	}
	if got := counter.activeOriginal.Load(); got != 0 {
		t.Errorf("GetActiveOriginalMediaAsset calls = %d, want 0", got)
	}
	if got := counter.encodePolicy.Load(); got != 0 {
		t.Errorf("GetRecordingEncodePolicy calls = %d, want 0", got)
	}
	if got := counter.activeEncoded.Load(); got != 0 {
		t.Errorf("GetActiveEncodedMediaAssetID calls = %d, want 0", got)
	}
}

type encodeReconcileQueryCounter struct {
	listMissing    atomic.Int64
	activeOriginal atomic.Int64
	encodePolicy   atomic.Int64
	activeEncoded  atomic.Int64
}

func (c *encodeReconcileQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	switch {
	case strings.Contains(data.SQL, "-- name: ListMissingEncodeProfiles"):
		c.listMissing.Add(1)
	case strings.Contains(data.SQL, "-- name: GetActiveOriginalMediaAsset"):
		c.activeOriginal.Add(1)
	case strings.Contains(data.SQL, "-- name: GetRecordingEncodePolicy"):
		c.encodePolicy.Add(1)
	case strings.Contains(data.SQL, "-- name: GetActiveEncodedMediaAssetID"):
		c.activeEncoded.Add(1)
	}
	return ctx
}

func (*encodeReconcileQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func poolWithQueryTracer(t *testing.T, pool *pgxpool.Pool, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	config := pool.Config()
	config.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("creating traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	return traced
}

// TestUnknownDesiredProfile_PredicateAsymmetry は、encode の候補と削除側の
// until_encoded_deletable_originals が同じ view unencoded_desired_profiles を引きつつ、
// 現在の設定（known_profiles）で絞るのは encode 側（関数 encodable_profile）だけで
// あることを固定する（docs/storage/retention.md §保持ポリシー）。
//
// フィクスチャは 1 件の録画で共通: keep_original='until_encoded'、desired が
// 現在の設定に無いプロファイル（"gone"）だけ、原本・サムネイル・タイルは active。
//
//   - 方向 A: encode の候補（現在の設定で絞る）には含まれない --- 投入しても
//     EncodeWorker が弾くだけのゴミになるため
//   - 方向 B: 削除エンジンの候補（絞らない）には含まれず、原本は active のまま
//     残る --- 共有 view に設定の絞りを足すと、config の 1 行の編集で原本が
//     削除可能になってしまうため
func TestUnknownDesiredProfile_PredicateAsymmetry(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)

	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, "asym325/orig.m2ts", []byte("data"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, store.AssetKindThumbnail, nil, "asym325/thumb.jpg", []byte("jpg"))
	// 原本を残す理由を desired の不足 1 つに絞る（タイルが無いと別の理由で残り、
	// 方向 B が空虚に通る）。
	seedSeekTilesAsset(t, pool, mediaDir, recordingID, "asym325/tiles.jpg")
	markRecordingUntilEncoded(t, pool, recordingID, []string{"gone"})

	// 方向 A: 候補クエリ（known_profiles で絞る）に現れない。
	q := sqlcgen.New(pool)
	candidates, err := q.ListMissingEncodeProfiles(context.Background(), sqlcgen.ListMissingEncodeProfilesParams{
		KnownProfiles: []string{"h264"},
		AttemptLimit:  jobs.EncodeAttemptLimit,
		RowLimit:      encodeReconcileRowLimit,
	})
	if err != nil {
		t.Fatalf("ListMissingEncodeProfiles: %v", err)
	}
	if slices.IndexFunc(candidates, func(row sqlcgen.ListMissingEncodeProfilesRow) bool {
		return row.RecordingID == recordingID
	}) >= 0 {
		t.Errorf("ListMissingEncodeProfiles candidates = %v, recording %d (desired profile %q not in current config) must not be a candidate",
			candidates, recordingID, "gone")
	}

	// 方向 B: 削除エンジンを 1 パス回しても、この録画の原本は消えない
	// （until_encoded_deletable_originals は known_profiles で絞らないので、
	// 「desired が揃っている」という判定に永久に到達しない）。
	dw := &DeleteReconcileWorker{MediaDelete: pgstore.NewMedia(pool), Storage: newTestMediaStore(pool), RecordingPurge: pgstore.NewRecordings(pool), Breakers: pgstore.NewBreakers(pool), MediaDir: mediaDir}
	if err := runWorkerViaJobQFake(context.Background(), dw, DeleteReconcileArgs{}); err != nil {
		t.Fatalf("DeleteReconcileWorker.Work: %v", err)
	}
	if got := assetState(t, pool, originalID); got != "active" {
		t.Errorf("original state after delete reconcile = %q, want active "+
			"(a config typo/rename must not make this original deletable; it must stay until the profile is re-encoded or restored)", got)
	}
}

// 候補が 0 件のパスでも成功すること（通常運転はこちら）。
func TestEncodeReconcileWorker_NoCandidates(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "none/a.m2ts", nil, []byte("x"))

	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h264"), Media: newTestMediaStore(pool)}
	runEncodeReconcilePass(t, w)

	if jobs := len(enqueuedArgs[jobs.EncodeJobArgs](w.Enqueuer.(*recordingEnqueuer), jobs.EncodeJobArgs{}.Kind())); jobs != 0 {
		t.Errorf("encode jobs = %d, want 0", jobs)
	}
}

// TestEncodeReconcileWorker_SkipsProfilesMissingFromConfig は、設定から消えた
// プロファイルを投入しないこと・それでも設定に残っているプロファイルは投入する
// ことを両方向で見る。
//
// 投入してしまうと EncodeWorker が `unknown encode profile` で 25 回失敗 →
// discarded になり、pendingJobStates に discarded が無いので次パスがまた
// 投入する（15 分ごとに永久）。候補としても窓を恒久的に占有する。
func TestEncodeReconcileWorker_SkipsProfilesMissingFromConfig(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	// 同じ録画が「設定に残っている h265」と「設定から消えた gone」の両方を
	// 凍結している。候補には挙がるが、投入されるのは h265 だけ。
	mixed := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "cfg/mixed.m2ts",
		[]string{"gone", "h265"}, []byte("x"))
	// desired が消えたプロファイルだけの録画。候補にも挙がらない。
	onlyGone := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "cfg/gone.m2ts",
		[]string{"gone"}, []byte("x"))

	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), Media: newTestMediaStore(pool)}
	runEncodeReconcilePass(t, w)

	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), mixed, "h265"); got != 1 {
		t.Errorf("encode jobs for the configured profile = %d, want 1", got)
	}
	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), mixed, "gone"); got != 0 {
		t.Errorf("encode jobs for the removed profile (same recording) = %d, want 0", got)
	}
	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), onlyGone, "gone"); got != 0 {
		t.Errorf("encode jobs for the removed profile (only desired) = %d, want 0", got)
	}

	// 落としたことは黙らせない（プロファイル別の件数をゲージに出す）。
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileUnsatisfiable.WithLabelValues("gone")); got != 2 {
		t.Errorf("unsatisfiable gauge for %q = %v, want 2", "gone", got)
	}
}

// TestEncodeReconcileWorker_RowLimitCapsWorkPerPass は #326 の判定基準 C2
// （コスト）を固定する: 候補が RowLimit を超えていても、1 パスが examine するのは
// ちょうど RowLimit 件までであること。
func TestEncodeReconcileWorker_RowLimitCapsWorkPerPass(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	first := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "cap/1.m2ts", []string{"h265"}, []byte("x"))
	second := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "cap/2.m2ts", []string{"cut"}, []byte("x"))
	third := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "cap/3.m2ts", []string{"cut"}, []byte("x"))
	if first >= second || second >= third {
		t.Fatalf("expected ascending recording ids, got first=%d second=%d third=%d", first, second, third)
	}
	secondKeep := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	thirdKeep := []chapters.Range{{StartMs: 1000, EndMs: 2000}}
	secondRequested := seedEncodedAssetRebuildRequest(t, pool, second, secondKeep)
	thirdRequested := seedEncodedAssetRebuildRequest(t, pool, third, thirdKeep)

	profiles := config.EncodeConfig{Profiles: []config.EncodeProfile{
		{Name: "h265", Container: "mp4", VideoCodec: "libx265", AudioCodec: "aac"},
		{Name: "cut", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"},
	}}
	w := &EncodeReconcileWorker{Profiles: profiles, RowLimit: 2, Media: newTestMediaStore(pool)}
	runEncodeReconcilePass(t, w)

	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), first, "h265"); got != 1 {
		t.Errorf("encode jobs for first = %d, want 1", got)
	}
	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), second, "cut"); got != 1 {
		t.Errorf("encode jobs for second rebuild = %d, want 1", got)
	}
	// 3 件目は RowLimit=2 を超えるのでこのパスでは examine されない。
	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), third, "cut"); got != 0 {
		t.Errorf("encode jobs for third = %d, want 0 (a single pass must not examine more than RowLimit candidates)", got)
	}
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileCandidates); got != 2 {
		t.Errorf("candidates gauge = %v, want 2 (equal to RowLimit)", got)
	}
	continuations := enqueuedArgs[EncodeReconcileArgs](w.Enqueuer.(*recordingEnqueuer), "encode_reconcile")
	if len(continuations) != 1 || continuations[0].AfterRecordingID != second {
		t.Errorf("continuations = %+v, want one cursor after recording %d", continuations, second)
	}
	secondJob := findEncodeJob(t, w.Enqueuer.(*recordingEnqueuer), second, "cut")
	if !bytes.Equal(secondJob.RequestedCutKeep, secondRequested) {
		t.Errorf("second rebuild keep = %s, want %s", secondJob.RequestedCutKeep, secondRequested)
	}
	if len(continuations) == 1 {
		next := &EncodeReconcileWorker{Enqueuer: w.Enqueuer, Media: newTestMediaStore(pool), Profiles: profiles, RowLimit: 2}
		if err := runWorkerViaJobQFake(context.Background(), next, continuations[0]); err != nil {
			t.Fatalf("continuation after rebuild candidate: %v", err)
		}
		if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), third, "cut"); got != 1 {
			t.Errorf("encode jobs for rebuild behind cursor = %d, want 1", got)
		}
		thirdJob := findEncodeJob(t, w.Enqueuer.(*recordingEnqueuer), third, "cut")
		if !bytes.Equal(thirdJob.RequestedCutKeep, thirdRequested) {
			t.Errorf("third rebuild keep = %s, want %s", thirdJob.RequestedCutKeep, thirdRequested)
		}
	}
	// 最後に完走したパスの時刻が更新されること（TestEncodeReconcileWorker_
	// RowLimitLeavesLaterRecordingsUnreached が固定していたアサーションを移設。
	// これが無いと EncodeReconcileLastPass.SetToCurrentTime の呼び出しを消しても
	// パッケージ全体のテストが green のまま通ってしまい、停止したパスが
	// 検出できなくなる）。
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileLastPass); got == 0 {
		t.Error("last-pass gauge was not set; a stalled pass would be undetectable")
	}
}

func TestEncodeReconcileWorker_ExhaustedRebuildRequestReturnsAfterRetryRequest(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "retry-exhausted-rebuild/1.m2ts", []string{"cut"}, []byte("x"))
	assetID := seedEncodedAsset(t, pool, id, "cut", "retry-exhausted-rebuild/cut.mp4")
	if err := setFrozenCuts(t, pool, assetID, []chapters.Range{{StartMs: 0, EndMs: 1800000}}); err != nil {
		t.Fatalf("seeding previous cut keep: %v", err)
	}
	requestedKeep := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	requestedJSON := seedEncodedAssetRebuildRequest(t, pool, id, requestedKeep)
	if _, err := pool.Exec(ctx, `INSERT INTO recording_encode_attempts
		(recording_id, profile, state, attempted_at, attempt_count)
		VALUES ($1, 'cut', 'failed', now(), $2)`, id, jobs.EncodeAttemptLimit); err != nil {
		t.Fatalf("seeding an exhausted rebuild attempt: %v", err)
	}

	enqueuer := newRecordingEnqueuer()
	profiles := config.EncodeConfig{Profiles: []config.EncodeProfile{{
		Name: "cut", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
	}}}
	firstWorker := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), Profiles: profiles, RowLimit: 1}
	if err := runWorkerViaJobQFake(ctx, firstWorker, EncodeReconcileArgs{}); err != nil {
		t.Fatalf("reconcile with an exhausted rebuild attempt: %v", err)
	}
	if got := countEncodeJobs(t, enqueuer, id, "cut"); got != 0 {
		t.Fatalf("encode jobs for exhausted rebuild candidate = %d, want 0", got)
	}

	deleted, err := sqlcgen.New(pool).DeleteFailedRecordingEncodeAttempts(ctx, sqlcgen.DeleteFailedRecordingEncodeAttemptsParams{
		RecordingID: id,
		Profiles:    []string{"cut"},
	})
	if err != nil {
		t.Fatalf("resetting failed rebuild attempt: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("failed rebuild attempts deleted = %d, want 1", deleted)
	}
	enqueuer.Reset()
	secondWorker := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), Profiles: profiles, RowLimit: 1}
	if err := runWorkerViaJobQFake(ctx, secondWorker, EncodeReconcileArgs{}); err != nil {
		t.Fatalf("reconcile after retry request: %v", err)
	}
	if got := countEncodeJobs(t, enqueuer, id, "cut"); got != 1 {
		t.Fatalf("encode jobs after rebuild retry request = %d, want 1", got)
	}
	if got := findEncodeJob(t, enqueuer, id, "cut").RequestedCutKeep; !bytes.Equal(got, requestedJSON) {
		t.Errorf("requested keep after retry = %s, want %s", got, requestedJSON)
	}
}

// TestEncodeReconcileWorker_ContinuationSurvivesFreshWorker は、--once のように
// パスごとに worker を作り直しても、候補窓の続きが次のジョブに渡ることを固定する。
func TestEncodeReconcileWorker_ContinuationSurvivesFreshWorker(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	first := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "continuation/1.m2ts", []string{"h265"}, []byte("x"))
	second := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "continuation/2.m2ts", []string{"h265"}, []byte("x"))
	if first >= second {
		t.Fatalf("expected ascending recording IDs, got first=%d second=%d", first, second)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO recording_encode_attempts
		(recording_id, profile, state, attempted_at, attempt_count)
		VALUES ($1, 'h265', 'failed', now(), 1)`, first); err != nil {
		t.Fatalf("seeding a retryable failed encode attempt: %v", err)
	}

	enqueuer := newRecordingEnqueuer()
	profiles := encodeConfigWith("h265")
	firstWorker := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), Profiles: profiles, RowLimit: 1}
	if err := runWorkerViaJobQFake(ctx, firstWorker, EncodeReconcileArgs{}); err != nil {
		t.Fatalf("first reconcile page: %v", err)
	}
	if got := countEncodeJobs(t, enqueuer, first, "h265"); got != 1 {
		t.Fatalf("first recording jobs after first page = %d, want 1", got)
	}
	if got := countEncodeJobs(t, enqueuer, second, "h265"); got != 0 {
		t.Fatalf("second recording jobs after first page = %d, want 0", got)
	}
	continuations := enqueuedArgs[EncodeReconcileArgs](enqueuer, "encode_reconcile")
	if len(continuations) != 1 {
		t.Fatalf("continuation jobs after first page = %d, want 1", len(continuations))
	}
	if continuations[0].AfterRecordingID != first {
		t.Fatalf("first continuation cursor = %d, want first recording ID %d", continuations[0].AfterRecordingID, first)
	}
	wire, err := json.Marshal(continuations[0])
	if err != nil {
		t.Fatalf("marshaling continuation args: %v", err)
	}
	var nextPage EncodeReconcileArgs
	if err := json.Unmarshal(wire, &nextPage); err != nil {
		t.Fatalf("unmarshaling continuation args: %v", err)
	}
	if nextPage.AfterRecordingID != first {
		t.Fatalf("continuation cursor after JSON round trip = %d, want %d", nextPage.AfterRecordingID, first)
	}

	// --once は各ジョブの後にプロセスを終了させる。次ページは新しい worker が引き継ぐ。
	secondWorker := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), Profiles: profiles, RowLimit: 1}
	if err := runWorkerViaJobQFake(ctx, secondWorker, nextPage); err != nil {
		t.Fatalf("second reconcile page: %v", err)
	}
	if got := countEncodeJobs(t, enqueuer, second, "h265"); got != 1 {
		t.Fatalf("second recording jobs after continuation = %d, want 1", got)
	}
	continuations = enqueuedArgs[EncodeReconcileArgs](enqueuer, "encode_reconcile")
	if len(continuations) != 2 || continuations[1].AfterRecordingID != second {
		t.Fatalf("continuations after second page = %+v, want cursors [%d %d]", continuations, first, second)
	}

	// 末尾を越えたページはもう投入せず、継続の連鎖を止める。
	lastWorker := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), Profiles: profiles, RowLimit: 1}
	if err := runWorkerViaJobQFake(ctx, lastWorker, continuations[1]); err != nil {
		t.Fatalf("page after the last candidate: %v", err)
	}
	if got := len(enqueuedArgs[EncodeReconcileArgs](enqueuer, "encode_reconcile")); got != 2 {
		t.Errorf("continuation jobs after the last page = %d, want 2", got)
	}
}

type continuationFailingEnqueuer struct {
	*recordingEnqueuer
}

func (e *continuationFailingEnqueuer) Enqueue(ctx context.Context, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	if args.Kind() == "encode_reconcile" {
		return jobq.Result{}, fmt.Errorf("continuation unavailable")
	}
	return e.recordingEnqueuer.Enqueue(ctx, args, opts...)
}

func TestEncodeReconcileWorker_ShortPageDoesNotContinue(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "short-page/1.m2ts", []string{"h265"}, []byte("x"))
	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), RowLimit: 2, Media: newTestMediaStore(pool)}
	enqueuer := runEncodeReconcilePass(t, w)
	if got := len(enqueuedArgs[EncodeReconcileArgs](enqueuer, "encode_reconcile")); got != 0 {
		t.Errorf("continuation jobs for a short page = %d, want 0", got)
	}
	if got := countEncodeJobs(t, enqueuer, id, "h265"); got != 1 {
		t.Errorf("encode jobs for the only candidate = %d, want 1", got)
	}
}

func TestEncodeReconcileWorker_ContinuationEnqueueFailureReturnsSuccess(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "continuation-error/1.m2ts", []string{"h265"}, []byte("x"))
	enqueuer := &continuationFailingEnqueuer{recordingEnqueuer: newRecordingEnqueuer()}
	w := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), Profiles: encodeConfigWith("h265"), RowLimit: 1}
	if err := runWorkerViaJobQFake(context.Background(), w, EncodeReconcileArgs{}); err != nil {
		t.Fatalf("Work after continuation enqueue failure = %v, want nil", err)
	}
	if got := countEncodeJobs(t, enqueuer.recordingEnqueuer, id, "h265"); got != 1 {
		t.Errorf("encode jobs for candidate = %d, want 1", got)
	}
	if got := len(enqueuedArgs[EncodeReconcileArgs](enqueuer.recordingEnqueuer, "encode_reconcile")); got != 0 {
		t.Errorf("continuation jobs after injected enqueue failure = %d, want 0", got)
	}
}

func TestEncodeReconcileWorker_RejectsTargetAndContinuationTogether(t *testing.T) {
	w := &EncodeReconcileWorker{Enqueuer: newRecordingEnqueuer()}
	err := w.Work(context.Background(), EncodeReconcileArgs{RecordingID: 10, AfterRecordingID: 20})
	if !errors.Is(err, jobq.ErrDiscard) {
		t.Fatalf("Work error = %v, want jobq.ErrDiscard for mixed target and continuation arguments", err)
	}
}

func TestEncodeReconcileWorker_ExhaustedAttemptReturnsAfterRetryRequest(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "retry-exhausted/1.m2ts", []string{"h265"}, []byte("x"))
	if _, err := pool.Exec(ctx, `INSERT INTO recording_encode_attempts
		(recording_id, profile, state, attempted_at, attempt_count)
		VALUES ($1, 'h265', 'failed', now(), $2)`, id, jobs.EncodeAttemptLimit); err != nil {
		t.Fatalf("seeding an exhausted encode attempt: %v", err)
	}
	enqueuer := newRecordingEnqueuer()
	profiles := encodeConfigWith("h265")
	firstWorker := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), Profiles: profiles, RowLimit: 1}
	if err := runWorkerViaJobQFake(ctx, firstWorker, EncodeReconcileArgs{}); err != nil {
		t.Fatalf("reconcile with an exhausted attempt: %v", err)
	}
	if got := countEncodeJobs(t, enqueuer, id, "h265"); got != 0 {
		t.Fatalf("encode jobs for exhausted candidate = %d, want 0", got)
	}

	deleted, err := sqlcgen.New(pool).DeleteFailedRecordingEncodeAttempts(ctx, sqlcgen.DeleteFailedRecordingEncodeAttemptsParams{
		RecordingID: id,
		Profiles:    []string{"h265"},
	})
	if err != nil {
		t.Fatalf("resetting failed encode attempts: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("failed encode attempts deleted = %d, want 1", deleted)
	}
	enqueuer.Reset()
	secondWorker := &EncodeReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), Profiles: profiles, RowLimit: 1}
	if err := runWorkerViaJobQFake(ctx, secondWorker, EncodeReconcileArgs{}); err != nil {
		t.Fatalf("reconcile after retry request: %v", err)
	}
	if got := countEncodeJobs(t, enqueuer, id, "h265"); got != 1 {
		t.Errorf("encode jobs after retry request = %d, want 1", got)
	}
}

// TestEncodeReconcileWorker_RowLimitCountsRecordings は、1 録画の複数 profile が
// RowLimit を消費せず、継続カーソルが recording_id 単位で進むことを固定する。
func TestEncodeReconcileWorker_RowLimitCountsRecordings(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	first := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "row-limit/1.m2ts", []string{"h264", "h265"}, []byte("x"))
	second := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "row-limit/2.m2ts", []string{"h265"}, []byte("x"))
	if first >= second {
		t.Fatalf("expected ascending recording ids, got first=%d second=%d", first, second)
	}

	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h264", "h265"), RowLimit: 1, Media: newTestMediaStore(pool)}
	runEncodeReconcilePass(t, w)
	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), first, "h264"); got != 1 {
		t.Errorf("pass 1: encode jobs for first/h264 = %d, want 1", got)
	}
	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), first, "h265"); got != 1 {
		t.Errorf("pass 1: encode jobs for first/h265 = %d, want 1 (all profiles of one recording must be returned)", got)
	}
	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), second, "h265"); got != 0 {
		t.Errorf("pass 1: encode jobs for second/h265 = %d, want 0 (RowLimit is in recordings)", got)
	}

	continuations := enqueuedArgs[EncodeReconcileArgs](w.Enqueuer.(*recordingEnqueuer), "encode_reconcile")
	if len(continuations) != 1 || continuations[0].AfterRecordingID != first {
		t.Fatalf("continuations after first recording = %+v, want cursor %d", continuations, first)
	}
	// 1 profile ではなく 1 録画が RowLimit を消費する。次ページを新しい worker で処理する。
	secondWorker := &EncodeReconcileWorker{Enqueuer: w.Enqueuer, Media: newTestMediaStore(pool), Profiles: w.Profiles, RowLimit: 1}
	if err := runWorkerViaJobQFake(context.Background(), secondWorker, continuations[0]); err != nil {
		t.Fatalf("second reconcile page: %v", err)
	}
	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), second, "h265"); got != 1 {
		t.Errorf("pass 2: encode jobs for second/h265 = %d, want 1 (cursor must be a recording ID)", got)
	}
}

// TestEncodeReconcileWorker_EmptyProfileConfigIsVisibleNotSilent は、
// encode.profiles が空の構成でパスが**空振りしていることが見える**ことを固定する。
//
// このパスは known_profiles を SQL に渡すが、Postgres の
// `x = ANY(NULL::text[])` は false ではなく NULL になるため、**nil を渡すと候補も
// 検出も同時に落ちてバックストップが無症状で死ぬ**。config.EncodeConfig.ProfileNames()
// は空設定でも non-nil を返す契約（TestEncodeConfig_ProfileNames_EmptyIsNonNil が
// 固定）なので、ここでは「空 = 候補ゼロだが unsatisfiable に出る」を確認する
// --- ProfileNames が nil を返すようになるとこのテストが落ちる。
func TestEncodeReconcileWorker_EmptyProfileConfigIsVisibleNotSilent(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "empty/a.m2ts",
		[]string{"h265"}, []byte("x"))

	// プロファイルが 1 つも無い設定。
	w := &EncodeReconcileWorker{Profiles: config.EncodeConfig{}, Media: newTestMediaStore(pool)}
	runEncodeReconcilePass(t, w)

	if got := countEncodeJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID, "h265"); got != 0 {
		t.Errorf("encode jobs = %d, want 0 (nothing can be encoded without profiles)", got)
	}
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileCandidates); got != 0 {
		t.Errorf("candidates gauge = %v, want 0", got)
	}
	// 空振りが見えること。ここが 0 だと「候補ゼロ」と「クエリが NULL で全部
	// 落ちた」を区別できない。
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileUnsatisfiable.WithLabelValues("h265")); got != 1 {
		t.Errorf("unsatisfiable gauge for %q = %v, want 1 (an empty profile set must be visible, not silent)", "h265", got)
	}
}

// Enqueuer が無いときはエラーを返す（黙って no-op にすると取りこぼしの回復そのものが消える）。
func TestEncodeReconcileWorker_Work_WithoutEnqueuer_Errors(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w := &EncodeReconcileWorker{Media: newTestMediaStore(pool)}
	if err := runWorkerViaJobQFake(context.Background(), w, EncodeReconcileArgs{}); err == nil {
		t.Fatal("expected error when worker has no enqueuer, got nil")
	}
}

// ジョブ種別名とキュー・一意化の形。期待値はリテラルで書く（実装の定数と
// 比べると定数を変えたときに何も主張しなくなる）。
func TestEncodeReconcileArgs_KindAndSpec(t *testing.T) {
	if kind := (EncodeReconcileArgs{}).Kind(); kind != "encode_reconcile" {
		t.Errorf("Kind() = %q, want encode_reconcile", kind)
	}
	spec := EncodeReconcileArgs{}.Spec()
	if spec.Queue != "encode" {
		t.Errorf("Spec().Queue = %q, want encode", spec.Queue)
	}
	if !spec.Coalesce {
		t.Error("Spec().Coalesce should be true")
	}
}

// TestEncodeReconcile_RebuildRequestOnTrashedRecordingIsNotQueued は、ごみ箱の録画に
// 作り直し要求が残っていても定期パスが encode を投入せず、復元後の全件パスで
// 投入することを両方向で固定する。要求行は消さない。
func TestEncodeReconcile_RebuildRequestOnTrashedRecordingIsNotQueued(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "review/trash.m2ts", []string{"cut"}, []byte("original"))
	if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(600000, 900000), 'CM', true)`, id); err != nil {
		t.Fatal(err)
	}
	keep, owned, err := newTestMediaStore(pool).GetCurrentCutKeep(ctx, id)
	if err != nil || !owned || len(keep) == 0 {
		t.Fatalf("currentCutKeep = %v, owned %v, err %v", keep, owned, err)
	}
	assetID := seedEncodedAsset(t, pool, id, "cut", "review/trash_cut.mp4")
	if err := setFrozenCuts(t, pool, assetID, []chapters.Range{{StartMs: 0, EndMs: 1800000}}); err != nil {
		t.Fatal(err)
	}
	seedEncodedAssetRebuildRequest(t, pool, id, keep)
	if _, err := sqlcgen.New(pool).SoftDeleteRecording(ctx, id); err != nil {
		t.Fatal(err)
	}

	cutProfile := config.EncodeProfile{Name: "cut", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"}
	w := &EncodeReconcileWorker{Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{cutProfile}}, Media: newTestMediaStore(pool)}
	enq := runEncodeReconcilePass(t, w)
	if got := countEncodeJobs(t, enq, id, "cut"); got != 0 {
		t.Fatalf("encode jobs for trashed recording's rebuild request = %d, want 0", got)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM encoded_asset_rebuild_requests WHERE recording_id = $1`, id).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("request rows after trashed pass = %d, err %v, want 1", rows, err)
	}

	if _, err := sqlcgen.New(pool).RestoreRecording(ctx, id); err != nil {
		t.Fatal(err)
	}
	runEncodeReconcilePass(t, w)
	if got := countEncodeJobs(t, enq, id, "cut"); got != 1 {
		t.Fatalf("encode jobs after restore = %d, want 1", got)
	}
}

// TestEncodeReconcile_RebuildRequestWithoutActiveOriginalIsNotQueued は、原本が active でない
// 録画の作り直し要求から encode を投入せず、原本が active に戻れば投入することを固定する。
func TestEncodeReconcile_RebuildRequestWithoutActiveOriginalIsNotQueued(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "review/noorig.m2ts", []string{"cut"}, []byte("original"))
	if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(600000, 900000), 'CM', true)`, id); err != nil {
		t.Fatal(err)
	}
	keep, _, err := newTestMediaStore(pool).GetCurrentCutKeep(ctx, id)
	if err != nil || len(keep) == 0 {
		t.Fatalf("currentCutKeep = %v, err %v", keep, err)
	}
	assetID := seedEncodedAsset(t, pool, id, "cut", "review/noorig_cut.mp4")
	if err := setFrozenCuts(t, pool, assetID, []chapters.Range{{StartMs: 0, EndMs: 1800000}}); err != nil {
		t.Fatal(err)
	}
	seedEncodedAssetRebuildRequest(t, pool, id, keep)
	setOriginal := func(state string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE media_assets SET state = $2 WHERE recording_id = $1 AND kind = 'original'`, id, state); err != nil {
			t.Fatal(err)
		}
	}
	setOriginal("deleting")

	cutProfile := config.EncodeProfile{Name: "cut", Cut: true, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"}
	w := &EncodeReconcileWorker{Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{cutProfile}}, Media: newTestMediaStore(pool)}
	enq := runEncodeReconcilePass(t, w)
	if got := countEncodeJobs(t, enq, id, "cut"); got != 0 {
		t.Fatalf("encode jobs without active original = %d, want 0", got)
	}
	setOriginal("active")
	runEncodeReconcilePass(t, w)
	if got := countEncodeJobs(t, enq, id, "cut"); got != 1 {
		t.Fatalf("encode jobs with active original = %d, want 1", got)
	}
}
