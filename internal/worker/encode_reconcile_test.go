package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
)

// runEncodeReconcilePass は EncodeReconcileWorker を River のジョブ実行と同じ
// コンテキスト（river.Client が載った ctx）で 1 パス回す。
func runEncodeReconcilePass(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	w := &EncodeReconcileWorker{Pool: pool}
	job := &river.Job[EncodeReconcileArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   EncodeReconcileArgs{},
	}
	if err := w.Work(riverWorkContext(t, pool), job); err != nil {
		t.Fatalf("EncodeReconcileWorker.Work: %v", err)
	}
}

// TestEncodeReconcile_ReenqueuesAfterLostHintAndDeletedEdgeRecord は issue #163 の
// 回帰テスト。塞いだ穴そのものを端から端まで再現する:
//
//  1. ingest は成功してコミットする（原本 media_asset + recording_encode_policy）
//  2. しかし encode 投入のヒントは飛ばない（enqueueMissingEncodesFromContext は
//     river.Client が取れないと黙って return する。ここでは Work を素の
//     context.Background() で呼んでその状態を作る。「ヒント投入に失敗して
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
	setReservationBase(t, pool, res.ID, `{"keepOriginal":"always","encodeProfiles":["h265"]}`)

	recordingID := insertTestRecordingForReservation(t, pool, programID)
	insertTestRecordSync(t, pool, recordingID, "rec-163-lost-hint")

	tsData := makeTSData(20)
	var deleteRequested atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tsData)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Options: mirakc.Options{ContentPath: strPtr("test/lost-hint.m2ts")},
				},
				Content: mirakc.ContentInfo{Path: "/recording/test/lost-hint.m2ts"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		case r.Method == http.MethodDelete:
			deleteRequested.Store(true)
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	iw := &IngestWorker{
		MirakcClient: mirakc.NewClient(srv.URL, nil),
		MediaDir:     t.TempDir(),
		Pool:         pool,
		StallTimeout: 5 * time.Second,
	}
	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-163-lost-hint"},
	}

	// river.Client を載せない ctx = ヒントが飛ばない。
	if err := iw.Work(context.Background(), job); err != nil {
		t.Fatalf("IngestWorker.Work: %v", err)
	}

	// 前提 1: desired は凍結された（この録画は h265 をエンコードすべき）。
	if _, profiles := encodePolicyOfRecording(t, pool, recordingID); !slices.Equal(profiles, []string{"h265"}) {
		t.Fatalf("encode_profiles = %v, want [h265]", profiles)
	}
	// 前提 2: エッジ record は消えた（ヒントを再送する経路が閉じた）。
	if !deleteRequested.Load() {
		t.Fatal("edge record was not deleted; the lost-hint scenario needs the re-hint path closed")
	}
	// 前提 3: ヒントは落ちた（encode ジョブが 1 件も入っていない）。
	if got := countEncodeJobs(t, pool, recordingID, "h265"); got != 0 {
		t.Fatalf("encode jobs before the periodic pass = %d, want 0 (the hint must be lost for this regression to mean anything)", got)
	}

	// 定期パスが差分を埋める。
	runEncodeReconcilePass(t, pool)

	if got := countEncodeJobs(t, pool, recordingID, "h265"); got != 1 {
		t.Errorf("encode jobs after the periodic pass = %d, want 1", got)
	}
}

// TestEncodeReconcile_DoesNotDoubleEnqueue は「定期パスを繰り返し回しても
// encode ジョブが二重に入らない」ことと、その根拠である River の一意制約の
// 実際の挙動（EncodeReconcileArgs.InsertOpts の doc コメントが引用している
// 測定値）を固定する。
func TestEncodeReconcile_DoesNotDoubleEnqueue(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	recordingID := seedRecordingWithOriginal(t, pool, t.TempDir(), "dup/a.m2ts",
		[]string{"h265"}, []byte("payload"))

	runEncodeReconcilePass(t, pool)
	if got := countEncodeJobs(t, pool, recordingID, "h265"); got != 1 {
		t.Fatalf("encode jobs after first pass = %d, want 1", got)
	}

	// 2 パス目。まだ encoded は無いので候補には挙がり続けるが、投入は増えない。
	runEncodeReconcilePass(t, pool)
	if got := countEncodeJobs(t, pool, recordingID, "h265"); got != 1 {
		t.Errorf("encode jobs after second pass = %d, want 1", got)
	}

	// River の一意制約そのものの挙動（doc コメントが「実測」と書いている内容）。
	client, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("NewInsertOnlyClient: %v", err)
	}
	ctx := context.Background()
	first, err := client.Insert(ctx, EncodeJobArgs{RecordingID: recordingID, Profile: "dedupe"}, nil)
	if err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	if first.UniqueSkippedAsDuplicate {
		t.Fatal("first Insert was skipped as duplicate; the measurement below would be meaningless")
	}
	second, err := client.Insert(ctx, EncodeJobArgs{RecordingID: recordingID, Profile: "dedupe"}, nil)
	if err != nil {
		t.Fatalf("second Insert: %v", err)
	}
	if !second.UniqueSkippedAsDuplicate {
		t.Error("second Insert of the same (recording_id, profile) was not skipped as duplicate")
	}
	if second.Job.ID != first.Job.ID {
		t.Errorf("second Insert returned job id %d, want the first job's id %d", second.Job.ID, first.Job.ID)
	}
	if got := countEncodeJobs(t, pool, recordingID, "dedupe"); got != 1 {
		t.Errorf("river_job rows for the duplicated args = %d, want 1", got)
	}

	// パスのジョブ自身も pending 中は 1 本に合流する。
	firstPass, err := client.Insert(ctx, EncodeReconcileArgs{}, nil)
	if err != nil {
		t.Fatalf("first EncodeReconcileArgs Insert: %v", err)
	}
	secondPass, err := client.Insert(ctx, EncodeReconcileArgs{}, nil)
	if err != nil {
		t.Fatalf("second EncodeReconcileArgs Insert: %v", err)
	}
	if !secondPass.UniqueSkippedAsDuplicate || secondPass.Job.ID != firstPass.Job.ID {
		t.Errorf("second encode_reconcile insert: skipped=%v id=%d, want skipped=true id=%d",
			secondPass.UniqueSkippedAsDuplicate, secondPass.Job.ID, firstPass.Job.ID)
	}
}

// TestListRecordingsMissingEncodes は候補クエリを両方向で見る。
//
// 「投入されないこと」をジョブ数で見ても意味が無いケースがある --- 例えば
// 「原本が無い（ingest 未完了）」は EnqueueMissingEncodes 側でも弾かれるので、
// クエリから条件を落としてもジョブ数のアサーションは通ってしまう。候補集合
// そのものを見る。
func TestListRecordingsMissingEncodes(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	q := sqlcgen.New(pool)
	mediaDir := t.TempDir()

	// (a) desired があり encoded が無い → 候補。
	missing := seedRecordingWithOriginal(t, pool, mediaDir, "q/missing.m2ts", []string{"h265"}, []byte("x"))

	// (b) desired が揃っている → 候補にしない。
	complete := seedRecordingWithOriginal(t, pool, mediaDir, "q/complete.m2ts", []string{"h264"}, []byte("x"))
	h264 := "h264"
	if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: complete, Kind: db.AssetKindEncoded, Profile: &h264,
		RelPath: "q/complete_h264.mp4", SizeBytes: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// (c) desired が空 → 候補にしない。
	noProfiles := seedRecordingWithOriginal(t, pool, mediaDir, "q/noprofiles.m2ts", nil, []byte("x"))

	// (d) ingest 未完了（policy はあるが原本 media_asset が無い）→ 候補にしない。
	incomplete := seedRecordingWithOriginal(t, pool, mediaDir, "q/incomplete.m2ts", []string{"h265"}, []byte("x"))
	if _, err := pool.Exec(ctx, `DELETE FROM media_assets WHERE recording_id = $1`, incomplete); err != nil {
		t.Fatal(err)
	}

	// (e) 原本が物理削除済み（until_encoded の後始末）→ 候補にしない。
	originalGone := seedRecordingWithOriginal(t, pool, mediaDir, "q/gone.m2ts", []string{"h265"}, []byte("x"))
	if _, err := pool.Exec(ctx,
		`UPDATE media_assets SET state = 'deleted' WHERE recording_id = $1 AND kind = 'original'`,
		originalGone); err != nil {
		t.Fatal(err)
	}

	// (f) ごみ箱の録画 → 候補にしない。
	trashed := seedRecordingWithOriginal(t, pool, mediaDir, "q/trashed.m2ts", []string{"h265"}, []byte("x"))
	if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, trashed); err != nil {
		t.Fatal(err)
	}

	// (g) encoded の行はあるが state='deleted'（復元後など）→ 候補。
	// EnqueueMissingEncodes が見るのは active な encoded だけ
	// （GetActiveEncodedMediaAssetID）なので、候補の定義もそこに揃える。
	encodedGone := seedRecordingWithOriginal(t, pool, mediaDir, "q/encgone.m2ts", []string{"h264"}, []byte("x"))
	if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: encodedGone, Kind: db.AssetKindEncoded, Profile: &h264,
		RelPath: "q/encgone_h264.mp4", SizeBytes: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE media_assets SET state = 'deleted' WHERE recording_id = $1 AND kind = 'encoded'`,
		encodedGone); err != nil {
		t.Fatal(err)
	}

	got, err := q.ListRecordingsMissingEncodes(ctx, encodeReconcileRowLimit)
	if err != nil {
		t.Fatalf("ListRecordingsMissingEncodes: %v", err)
	}
	if !slices.Equal(got, []int64{missing, encodedGone}) {
		t.Errorf("candidates = %v, want [%d %d] (complete=%d noProfiles=%d incomplete=%d originalGone=%d trashed=%d)",
			got, missing, encodedGone, complete, noProfiles, incomplete, originalGone, trashed)
	}
}

// 候補が 0 件のパスでも成功すること（通常運転はこちら）。
func TestEncodeReconcileWorker_NoCandidates(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	seedRecordingWithOriginal(t, pool, t.TempDir(), "none/a.m2ts", nil, []byte("x"))

	runEncodeReconcilePass(t, pool)

	var jobs int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job WHERE kind = 'encode'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Errorf("encode jobs = %d, want 0", jobs)
	}
}

// river.Client が取れない ctx ではエラーを返すこと（EncodeEnqueueHintWorker と
// 同じ判断 --- 黙って no-op にすると取りこぼしの回復そのものが消える）。
func TestEncodeReconcileWorker_Work_WithoutClient_Errors(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w := &EncodeReconcileWorker{Pool: pool}
	job := &river.Job[EncodeReconcileArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   EncodeReconcileArgs{},
	}
	if err := w.Work(context.Background(), job); err == nil {
		t.Fatal("expected error when no river client is attached to ctx, got nil")
	}
}

// ジョブ種別名とキュー・一意化の形。期待値はリテラルで書く（実装の定数と
// 比べると定数を変えたときに何も主張しなくなる）。
func TestEncodeReconcileArgs_KindAndInsertOpts(t *testing.T) {
	if kind := (EncodeReconcileArgs{}).Kind(); kind != "encode_reconcile" {
		t.Errorf("Kind() = %q, want encode_reconcile", kind)
	}
	opts := EncodeReconcileArgs{}.InsertOpts()
	if opts.Queue != "encode" {
		t.Errorf("Queue = %q, want encode", opts.Queue)
	}
	if !opts.UniqueOpts.ByArgs {
		t.Error("UniqueOpts.ByArgs should be true")
	}
	if slices.Contains(opts.UniqueOpts.ByState, rivertype.JobStateCompleted) {
		t.Error("UniqueOpts.ByState must not include completed (the periodic job would become one-shot)")
	}
}

// worker.periodic_jobs が有効なら encode_reconcile が定期ジョブとして登録される
// こと（登録漏れは「実装したのに永久に走らない」で終わる）。
func TestBuildRiverConfig_RegistersEncodeReconcilePeriodicJob(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		PeriodicJobs:            true,
		EncodeReconcile:         true,
		EncodeReconcileInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	if len(riverCfg.PeriodicJobs) != 1 {
		t.Fatalf("PeriodicJobs = %d, want 1", len(riverCfg.PeriodicJobs))
	}

	disabled, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		PeriodicJobs:    false,
		EncodeReconcile: true,
	})
	if err != nil {
		t.Fatalf("buildRiverConfig (periodic disabled): %v", err)
	}
	if len(disabled.PeriodicJobs) != 0 {
		t.Errorf("PeriodicJobs with periodic_jobs=false = %d, want 0", len(disabled.PeriodicJobs))
	}
}
