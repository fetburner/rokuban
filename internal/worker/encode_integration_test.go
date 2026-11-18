package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/encode"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/storagemaint"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
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
	pool := setupTestPool(t)
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

	job := IngestJobArgs{Site: "default", RecordID: "rec-policy-cut-only"}
	w.Enqueuer = newRecordingEnqueuer()
	workCtx := context.Background()
	testutil.RequireNoError(t, runWorkerViaJobQFake(workCtx, w, job), "Work() error")

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
	pool := setupTestPool(t)
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

	job := IngestJobArgs{Site: "default", RecordID: "rec-policy-cut-only-live"}
	w.Enqueuer = newRecordingEnqueuer()
	workCtx := context.Background()
	testutil.RequireNoError(t, runWorkerViaJobQFake(workCtx, w, job), "Work() error")

	_, profiles := encodePolicyOfRecording(t, pool, recordingID)
	if len(profiles) != 1 || profiles[0] != "cut" {
		t.Errorf("encode_profiles = %v, want [cut] with live enabled", profiles)
	}
}

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
	pool := setupTestPool(t)
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
		RecordTarget:  pgstoretest.Recordings(pool),
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	iw.CMDetectionStore = pgstoretest.Analysis(pool)
	job := IngestJobArgs{Site: "default", RecordID: "rec-163-lost-hint"}

	// Enqueuer を注入しない状態 = ヒントが飛ばない。
	testutil.RequireNoError(t, runWorkerViaJobQFake(context.Background(), iw, job), "IngestWorker.Work")

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
		Enqueuer: newRecordingEnqueuer(),
		Profiles: encodeConfigWith("h265"),
		Media:    pgstoretest.Media(pool),
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

func prepareEncodeInput(t *testing.T, pool *pgxpool.Pool, recordingID int64, mediaDir, relPath string, profiles []string, content []byte) int64 {
	t.Helper()
	seedEncodePolicy(t, pool, recordingID, profiles)
	seedOriginalAsset(t, pool, mediaDir, recordingID, relPath, content)
	return recordingID
}

func seedEncodePolicy(t *testing.T, pool *pgxpool.Pool, recordingID int64, profiles []string) {
	t.Helper()
	if len(profiles) == 0 {
		return
	}
	// recording_processing_policy 衛星表（issue #159）。keep_original は
	// 定期 reconcile 候補 query が active encoded を完了扱いにするので
	// 'always' で十分 --- desired プロファイルの有無だけがテストの関心。
	dbtest.ProcessingPolicy(t, pool, recordingID, func(p *dbtest.ProcessingPolicyParams) { p.EncodeProfiles = profiles })
}

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

func runEncodeReconcilePassArgs(t *testing.T, w *encode.EncodeReconcileWorker, args EncodeReconcileArgs) *recordingEnqueuer {
	t.Helper()
	if w.Media == nil {
		t.Fatal("EncodeReconcileWorker.Media is nil")
	}
	enqueuer, ok := w.Enqueuer.(*recordingEnqueuer)
	if !ok {
		enqueuer = newRecordingEnqueuer()
		w.Enqueuer = enqueuer
	}
	testutil.RequireNoError(t, runWorkerViaJobQFake(context.Background(), w, args), "EncodeReconcileWorker.Work")
	return enqueuer
}

// runEncodeReconcilePass runs one pass and records the Args it asks the queue to deliver.
func runEncodeReconcilePass(t *testing.T, w *encode.EncodeReconcileWorker) *recordingEnqueuer {
	return runEncodeReconcilePassArgs(t, w, EncodeReconcileArgs{})
}

// cutFFmpegProfile は実 ffmpeg の統合テストが使う最小の cut プロファイル。
// Scaler/Height を 0 にしてあるので、filtergraph は trim → concat → null だけに
// なり、測っているものが「切り取り」だけであることが保証される。
func cutFFmpegProfile() config.EncodeProfile {
	return config.EncodeProfile{
		Name:       "cut",
		Container:  "mp4",
		VideoCodec: "libx264",
		AudioCodec: "aac",
		Cut:        true,
	}
}

// seedEncodedAsset は active な encoded media_assets 行を 1 つ作って id を返す。
func seedEncodedAsset(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile, relPath string, generations ...int64) int64 {
	t.Helper()
	ctx := context.Background()
	profileName := profile
	generation := testLegacyDerivedGeneration(relPath)
	if len(generations) > 0 {
		generation = generations[0]
	}
	assetID, err := sqlcgen.New(pool).UpsertDerivedMediaAsset(ctx, sqlcgen.UpsertDerivedMediaAssetParams{Kind: "encoded",
		RecordingID:       recordingID,
		Profile:           &profileName,
		SizeBytes:         1,
		DerivedGeneration: generation,
	})
	testutil.RequireNoError(t, err, "seeding encoded media_asset")
	_, err = sqlcgen.New(pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: assetID, Store: dbtest.StoreName, RelPath: relPath,
		Generation: generation, State: "active",
	})
	testutil.RequireNoError(t, err, "seeding encoded media_asset copy")
	return assetID
}

func testLegacyDerivedGeneration(relPath string) int64 {
	stem := strings.TrimSuffix(filepath.Base(relPath), filepath.Ext(relPath))
	_, generation, ok := config.SplitCutGeneration(stem)
	if !ok {
		_, generation, ok = config.SplitUncutGeneration(stem)
	}
	if !ok {
		return 0
	}
	return int64(generation)
}

// setFrozenCuts は media_asset_cuts を直接書く（commitCutEncoded がやることの
// うち、行の upsert を除いた部分。テストが前状態を作るために使う）。
func setFrozenCuts(t *testing.T, pool *pgxpool.Pool, assetID int64, keep []chapters.Range) error {
	t.Helper()
	ranges, err := pgstore.KeepRangesParam(keep)
	if err != nil {
		return err
	}
	q := sqlcgen.New(pool)
	if err := q.DeleteMediaAssetCuts(context.Background(), assetID); err != nil {
		return err
	}
	return q.InsertMediaAssetCuts(context.Background(), sqlcgen.InsertMediaAssetCutsParams{
		MediaAssetID: assetID,
		KeepRanges:   ranges,
	})
}

func assertOriginalAssetSHA256(t *testing.T, pool *pgxpool.Pool, recordingID int64, content []byte) {
	t.Helper()
	const query = `SELECT sha256 FROM media_assets WHERE recording_id = $1 AND kind = $2 AND EXISTS (SELECT 1 FROM media_asset_copies c WHERE c.media_asset_id = media_assets.id AND c.state = 'active') AND profile IS NULL`
	var got []byte
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), query, recordingID, store.AssetKindOriginal).Scan(&got), fmt.Sprintf("reading active original media asset SHA-256 for recording %d", recordingID))
	want := sha256.Sum256(content)
	if !bytes.Equal(got, want[:]) {
		t.Fatalf("active original SHA-256 = %x, want %x", got, want)
	}
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
	t.Parallel()
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
		RowLimit:      1000,
	})
	testutil.RequireNoError(t, err, "ListMissingEncodeProfiles")
	if slices.IndexFunc(candidates, func(row sqlcgen.ListMissingEncodeProfilesRow) bool {
		return row.RecordingID == recordingID
	}) >= 0 {
		t.Errorf("ListMissingEncodeProfiles candidates = %v, recording %d (desired profile %q not in current config) must not be a candidate",
			candidates, recordingID, "gone")
	}

	// 方向 B: 削除エンジンを 1 パス回しても、この録画の原本は消えない
	// （until_encoded_deletable_originals は known_profiles で絞らないので、
	// 「desired が揃っている」という判定に永久に到達しない）。
	dw := &storagemaint.DeleteReconcileWorker{MediaDelete: pgstoretest.Media(pool), Storage: pgstoretest.Media(pool), RecordingPurge: pgstoretest.Recordings(pool), Breakers: pgstoretest.Breakers(pool), MediaDir: mediaDir, StorageObjects: testStorageObjects(t, "", mediaDir),
		DerivedObjects: testFilesystemObjectStore(t, mediaDir)}
	testutil.RequireNoError(t, runWorkerViaJobQFake(context.Background(), dw, DeleteReconcileArgs{}), "DeleteReconcileWorker.Work")
	if got := singleCopyState(t, pool, originalID); got != "active" {
		t.Errorf("original copy state after delete reconcile = %q, want active "+
			"(a config typo/rename must not make this original deletable; it must stay until the profile is re-encoded or restored)", got)
	}
}
