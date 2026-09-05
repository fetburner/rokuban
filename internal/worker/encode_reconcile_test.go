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
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
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

// runEncodeReconcilePass は EncodeReconcileWorker を River のジョブ実行と同じ
// コンテキスト（river.Client が載った ctx）で 1 パス回す。
func runEncodeReconcilePass(t *testing.T, pool *pgxpool.Pool, w *EncodeReconcileWorker) {
	t.Helper()
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
	setReservationBase(t, pool, res.ProgramID, `{"keepOriginal":"always","encodeProfiles":["h265"]}`)

	recordingID := insertTestRecordingForReservation(t, pool, programID)
	insertTestRecordSyncForSite(t, pool, "default", recordingID, "rec-163-lost-hint", programID)

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
		MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
		MediaDir:      t.TempDir(),
		Pool:          pool,
		StallTimeout:  5 * time.Second,
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
	runEncodeReconcilePass(t, pool, &EncodeReconcileWorker{Pool: pool, Profiles: encodeConfigWith("h265")})

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

	w := &EncodeReconcileWorker{Pool: pool, Profiles: encodeConfigWith("h265")}
	runEncodeReconcilePass(t, pool, w)
	if got := countEncodeJobs(t, pool, recordingID, "h265"); got != 1 {
		t.Fatalf("encode jobs after first pass = %d, want 1", got)
	}

	// 2 パス目。まだ encoded は無いので候補には挙がり続けるが、投入は増えない。
	runEncodeReconcilePass(t, pool, w)
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

	// (h) desired が現在の設定に無いプロファイル（改名 / 削除された）→ 候補に
	// しない。投入しても EncodeWorker が `unknown encode profile` で弾くだけで
	// 永久に満たされず、recording_id 昇順の窓を恒久的に占有する
	// （EncodeReconcileWorker の doc コメント「窓は回らない」）。
	renamed := seedRecordingWithOriginal(t, pool, mediaDir, "q/renamed.m2ts", []string{"gone"}, []byte("x"))

	// (i) 空文字列のプロファイル名 → 候補にしない。EnqueueMissingEncodes が
	// 空文字列をスキップするので、候補に挙げると (h) と同じ「永久に満たされない
	// 候補」になる。設定側の名前は必須検証済みなので known_profiles には
	// 空文字列が入らず、(h) と同じ仕組みで落ちる。
	emptyProfile := seedRecordingWithOriginal(t, pool, mediaDir, "q/empty.m2ts", []string{""}, []byte("x"))

	got, err := q.ListRecordingsMissingEncodes(ctx, sqlcgen.ListRecordingsMissingEncodesParams{
		KnownProfiles: []string{"h264", "h265"},
		RowLimit:      encodeReconcileRowLimit,
	})
	if err != nil {
		t.Fatalf("ListRecordingsMissingEncodes: %v", err)
	}
	if !slices.Equal(got, []int64{missing, encodedGone}) {
		t.Errorf("candidates = %v, want [%d %d] (complete=%d noProfiles=%d incomplete=%d originalGone=%d trashed=%d renamed=%d emptyProfile=%d)",
			got, missing, encodedGone, complete, noProfiles, incomplete, originalGone, trashed, renamed, emptyProfile)
	}

	// AfterRecordingID（#326 の窓の回転が使う keyset 述語）: missing の id より
	// 後ろだけを見ると、missing 自身は落ちて encodedGone だけが残る。
	if missing >= encodedGone {
		t.Fatalf("expected missing (%d) < encodedGone (%d) for this assertion to mean anything", missing, encodedGone)
	}
	gotAfter, err := q.ListRecordingsMissingEncodes(ctx, sqlcgen.ListRecordingsMissingEncodesParams{
		AfterRecordingID: missing,
		KnownProfiles:    []string{"h264", "h265"},
		RowLimit:         encodeReconcileRowLimit,
	})
	if err != nil {
		t.Fatalf("ListRecordingsMissingEncodes with AfterRecordingID: %v", err)
	}
	if !slices.Equal(gotAfter, []int64{encodedGone}) {
		t.Errorf("candidates with AfterRecordingID=%d = %v, want [%d]", missing, gotAfter, encodedGone)
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

// TestUnknownDesiredProfile_PredicateAsymmetry は「desired が全部揃っているか」
// という同じ形の述語が 3 箇所（このパスの ListRecordingsMissingEncodes /
// ListUnsatisfiableEncodeProfiles、削除エンジンの
// until_encoded_deletable_originals view）にあり、known_profiles で絞るかどうかで
// 意図的に食い違っていることを固定する回帰テスト（決定は
// internal/db/queries/encode_reconcile.sql のヘッダと
// docs/storage/retention.md §保持ポリシー）。
//
// フィクスチャは 1 件の録画で共通: keep_original='until_encoded'、desired が
// 現在の設定に無いプロファイル（"gone"）だけ、原本・サムネイルは active。
//
//   - 方向 A: このパスの候補（現在の設定で絞る）には含まれない --- 投入しても
//     EncodeWorker が弾くだけのゴミになるため
//   - 方向 B: 削除エンジンの候補（絞らない）には含まれず、原本は active のまま
//     残る --- 「設定から消えたプロファイルを凍結している録画の原本は永久に
//     保持する」が安全側の仕様であり、絞り込みを追加すると config の 1 行の
//     編集で原本が削除可能になってしまうため
//
// この 2 つのアサーションが将来「述語を揃えよう」とした変更を両側から検出する
// 装置になっている。
func TestUnknownDesiredProfile_PredicateAsymmetry(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, "asym325/orig.m2ts", []byte("data"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindThumbnail, nil, "asym325/thumb.jpg", []byte("jpg"))
	markRecordingUntilEncoded(t, pool, recordingID, []string{"gone"})

	// 方向 A: 候補クエリ（known_profiles で絞る）に現れない。
	q := sqlcgen.New(pool)
	candidates, err := q.ListRecordingsMissingEncodes(context.Background(), sqlcgen.ListRecordingsMissingEncodesParams{
		KnownProfiles: []string{"h264"},
		RowLimit:      encodeReconcileRowLimit,
	})
	if err != nil {
		t.Fatalf("ListRecordingsMissingEncodes: %v", err)
	}
	if slices.Contains(candidates, recordingID) {
		t.Errorf("ListRecordingsMissingEncodes candidates = %v, recording %d (desired profile %q not in current config) must not be a candidate",
			candidates, recordingID, "gone")
	}

	// 方向 B: 削除エンジンを 1 パス回しても、この録画の原本は消えない
	// （until_encoded_deletable_originals は known_profiles で絞らないので、
	// 「desired が揃っている」という判定に永久に到達しない）。
	dw := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := dw.Work(context.Background(), nil); err != nil {
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
	seedRecordingWithOriginal(t, pool, t.TempDir(), "none/a.m2ts", nil, []byte("x"))

	runEncodeReconcilePass(t, pool, &EncodeReconcileWorker{Pool: pool, Profiles: encodeConfigWith("h264")})

	var jobs int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job WHERE kind = 'encode'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
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
	mixed := seedRecordingWithOriginal(t, pool, mediaDir, "cfg/mixed.m2ts",
		[]string{"gone", "h265"}, []byte("x"))
	// desired が消えたプロファイルだけの録画。候補にも挙がらない。
	onlyGone := seedRecordingWithOriginal(t, pool, mediaDir, "cfg/gone.m2ts",
		[]string{"gone"}, []byte("x"))

	w := &EncodeReconcileWorker{Pool: pool, Profiles: encodeConfigWith("h265")}
	runEncodeReconcilePass(t, pool, w)

	if got := countEncodeJobs(t, pool, mixed, "h265"); got != 1 {
		t.Errorf("encode jobs for the configured profile = %d, want 1", got)
	}
	if got := countEncodeJobs(t, pool, mixed, "gone"); got != 0 {
		t.Errorf("encode jobs for the removed profile (same recording) = %d, want 0", got)
	}
	if got := countEncodeJobs(t, pool, onlyGone, "gone"); got != 0 {
		t.Errorf("encode jobs for the removed profile (only desired) = %d, want 0", got)
	}

	// 落としたことは黙らせない（プロファイル別の件数をゲージに出す）。
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileUnsatisfiable.WithLabelValues("gone")); got != 2 {
		t.Errorf("unsatisfiable gauge for %q = %v, want 2", "gone", got)
	}
}

// TestEncodeReconcileWorker_WindowRotatesPastStuckCandidates は #326 の判定基準
// C1（被覆）を固定する: 候補集合が一度も減らない（＝どの候補も永久に満たせない
// 恒久失敗と等価）最悪条件下でも、窓が前パスの続きから開くことで、有限パス数の
// うちに全候補が examine されること。
//
// 以前はここで「窓は回らない」という既知の限界（1 パスだけ回すと 2 件目に届かない）
// を固定していたが、#326 でその限界を解消したので反転させた --- 削除ではなく
// 反転して名前も主張に合わせる（テスト規律: 落としたテストではなく直したテスト
// として残す）。
//
// **同一ワーカーインスタンスを全パスで使い回す。** resumeAfter は
// EncodeReconcileWorker のフィールド（プロセスローカル）なので、パスごとに
// 新しいインスタンスを作るとテストが空虚に通る --- 作り直しは常に resumeAfter
// がゼロ値の新しいワーカーを使うことになり、「毎パス先頭から」= 修正前の挙動に
// 戻ってしまう。
func TestEncodeReconcileWorker_WindowRotatesPastStuckCandidates(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	first := seedRecordingWithOriginal(t, pool, mediaDir, "rot/1.m2ts", []string{"h265"}, []byte("x"))
	second := seedRecordingWithOriginal(t, pool, mediaDir, "rot/2.m2ts", []string{"h265"}, []byte("x"))
	third := seedRecordingWithOriginal(t, pool, mediaDir, "rot/3.m2ts", []string{"h265"}, []byte("x"))
	if first >= second || second >= third {
		t.Fatalf("expected ascending recording ids, got first=%d second=%d third=%d", first, second, third)
	}

	// encode は一切走らせない。候補は一度も減らない = 恒久失敗と同じ状況。
	w := &EncodeReconcileWorker{Pool: pool, Profiles: encodeConfigWith("h265"), RowLimit: 1}

	// パス 1: 1 件目にだけ投入される。
	runEncodeReconcilePass(t, pool, w)
	if got := countEncodeJobs(t, pool, first, "h265"); got != 1 {
		t.Fatalf("pass 1: encode jobs for first = %d, want 1", got)
	}
	if got := countEncodeJobs(t, pool, second, "h265"); got != 0 {
		t.Fatalf("pass 1: encode jobs for second = %d, want 0", got)
	}

	// パス 2: 窓が回って 2 件目に届く。
	runEncodeReconcilePass(t, pool, w)
	if got := countEncodeJobs(t, pool, second, "h265"); got != 1 {
		t.Fatalf("pass 2: encode jobs for second = %d, want 1 (the window must rotate past the stuck first candidate)", got)
	}
	if got := countEncodeJobs(t, pool, third, "h265"); got != 0 {
		t.Fatalf("pass 2: encode jobs for third = %d, want 0", got)
	}

	// パス 3: 3 件目に届く。
	runEncodeReconcilePass(t, pool, w)
	if got := countEncodeJobs(t, pool, third, "h265"); got != 1 {
		t.Fatalf("pass 3: encode jobs for third = %d, want 1", got)
	}

	// 巻き戻りの観測は river_job の件数では見えない（UniqueOpts が pending 中の
	// 1 本に合流させるため、1 件目に再投入してもジョブ数は増えない）。
	// 1 件目の river_job 行を削除してから巻き戻りを起こし、復活することで
	// 「もう一度 examine された」ことを見る。
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM river_job WHERE kind = 'encode' AND (args->>'recording_id')::bigint = $1 AND args->>'profile' = 'h265'`,
		first); err != nil {
		t.Fatalf("deleting first recording's river_job row: %v", err)
	}
	if got := countEncodeJobs(t, pool, first, "h265"); got != 0 {
		t.Fatalf("after delete: encode jobs for first = %d, want 0 (setup for the rewind check)", got)
	}

	// パス 4: 3 件目より後ろに候補は無い。窓は末尾を越えて空になる
	// （まだ先頭には戻っていない）。1 件目は resumeAfter=third より手前なので
	// このパスでは見ていない。
	runEncodeReconcilePass(t, pool, w)
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileCandidates); got != 0 {
		t.Fatalf("pass 4: candidates gauge = %v, want 0 (window ran past the end)", got)
	}
	if got := countEncodeJobs(t, pool, first, "h265"); got != 0 {
		t.Fatalf("pass 4: encode jobs for first = %d, want 0 (not yet rewound)", got)
	}

	// パス 5: 先頭へ巻き戻り、1 件目がもう一度 examine されて復活する。
	runEncodeReconcilePass(t, pool, w)
	if got := countEncodeJobs(t, pool, first, "h265"); got != 1 {
		t.Errorf("pass 5: encode jobs for first = %d, want 1 (the window must wrap around to the beginning)", got)
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
	first := seedRecordingWithOriginal(t, pool, mediaDir, "cap/1.m2ts", []string{"h265"}, []byte("x"))
	second := seedRecordingWithOriginal(t, pool, mediaDir, "cap/2.m2ts", []string{"h265"}, []byte("x"))
	third := seedRecordingWithOriginal(t, pool, mediaDir, "cap/3.m2ts", []string{"h265"}, []byte("x"))
	if first >= second || second >= third {
		t.Fatalf("expected ascending recording ids, got first=%d second=%d third=%d", first, second, third)
	}

	w := &EncodeReconcileWorker{Pool: pool, Profiles: encodeConfigWith("h265"), RowLimit: 2}
	runEncodeReconcilePass(t, pool, w)

	if got := countEncodeJobs(t, pool, first, "h265"); got != 1 {
		t.Errorf("encode jobs for first = %d, want 1", got)
	}
	if got := countEncodeJobs(t, pool, second, "h265"); got != 1 {
		t.Errorf("encode jobs for second = %d, want 1", got)
	}
	// 3 件目は RowLimit=2 を超えるのでこのパスでは examine されない。
	if got := countEncodeJobs(t, pool, third, "h265"); got != 0 {
		t.Errorf("encode jobs for third = %d, want 0 (a single pass must not examine more than RowLimit candidates)", got)
	}
	if got := promtestutil.ToFloat64(metrics.EncodeReconcileCandidates); got != 2 {
		t.Errorf("candidates gauge = %v, want 2 (equal to RowLimit)", got)
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
	recordingID := seedRecordingWithOriginal(t, pool, t.TempDir(), "empty/a.m2ts",
		[]string{"h265"}, []byte("x"))

	// プロファイルが 1 つも無い設定。
	w := &EncodeReconcileWorker{Pool: pool, Profiles: config.EncodeConfig{}}
	runEncodeReconcilePass(t, pool, w)

	if got := countEncodeJobs(t, pool, recordingID, "h265"); got != 0 {
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
