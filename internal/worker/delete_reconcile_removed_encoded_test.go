package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// removedEncodedFixture は「原本 + h264 + h265」の録画で、h264 を外した状態の材料。
type removedEncodedFixture struct {
	recordingID int64
	originalID  int64
	h264ID      int64
	h265ID      int64
	h264Path    string
	h265Path    string
}

// seedRemovedEncodedRecording は desired を profiles にした録画に原本と h264 / h265 を置く。
// 要求行は入れない（呼び出し側が requestEncodedRemoval で入れる）。
func seedRemovedEncodedRecording(t *testing.T, pool *pgxpool.Pool, mediaDir string, eventID int32, profiles []string) removedEncodedFixture {
	t.Helper()
	recordingID := insertTestRecordingWithEventID(t, pool, eventID)
	dir := fmt.Sprintf("removed/%d", recordingID)
	f := removedEncodedFixture{recordingID: recordingID}
	f.originalID = seedOriginalAsset(t, pool, mediaDir, recordingID, dir+"/orig.m2ts", []byte("original"))
	h264, h265 := "h264", "h265"
	f.h264ID = seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindEncoded, &h264, dir+"/h264.mp4", []byte("h264"))
	f.h265ID = seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindEncoded, &h265, dir+"/h265.mp4", []byte("h265"))
	f.h264Path = filepath.Join(mediaDir, dir, "h264.mp4")
	f.h265Path = filepath.Join(mediaDir, dir, "h265.mp4")
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles)
		 VALUES ($1, 'always', $2)`, recordingID, profiles); err != nil {
		t.Fatalf("seeding recording_encode_policy: %v", err)
	}
	return f
}

func requestEncodedRemoval(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile string) {
	t.Helper()
	if err := sqlcgen.New(pool).InsertEncodedAssetRemovalRequest(context.Background(), sqlcgen.InsertEncodedAssetRemovalRequestParams{
		RecordingID: recordingID, Profile: profile,
	}); err != nil {
		t.Fatalf("inserting removal request: %v", err)
	}
}

func removalRequestExists(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM encoded_asset_removal_requests WHERE recording_id = $1 AND profile = $2)`,
		recordingID, profile).Scan(&exists); err != nil {
		t.Fatalf("querying removal request: %v", err)
	}
	return exists
}

func setAssetState(t *testing.T, pool *pgxpool.Pool, id int64, state string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET state = $2 WHERE id = $1`, id, state); err != nil {
		t.Fatalf("setting asset %d state: %v", id, err)
	}
}

// 名前付き述語 removed_encoded_assets を両方向で固定する。「desired に無い」だけでも
// 「要求行がある」だけでも消えないこと、最後の版は消えないことを、入口クエリで見る。
func TestRemovedEncodedAssets_NamedPredicateBothDirections(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	q := sqlcgen.New(pool)

	// 要求行あり + desired に無い + 原本あり → 消える。
	removed := seedRemovedEncodedRecording(t, pool, mediaDir, 9101, []string{"h265"})
	requestEncodedRemoval(t, pool, removed.recordingID, "h264")

	// 要求行あり + 足し直しで desired にある → 消えない。要求行は足し直しが消すが、
	// view が要求行の残存に頼らないことを見るため、ここでは desired だけを戻す。
	readded := seedRemovedEncodedRecording(t, pool, mediaDir, 9102, []string{"h264", "h265"})
	requestEncodedRemoval(t, pool, readded.recordingID, "h264")

	// 要求行なし + desired に無い（rescue 等で desired と実ファイルがずれた形）→ 消えない。
	drifted := seedRemovedEncodedRecording(t, pool, mediaDir, 9103, []string{"h265"})

	// 要求行あり + desired に無い + 原本なし + 外していない h265 あり → 消える。
	otherVersion := seedRemovedEncodedRecording(t, pool, mediaDir, 9104, []string{"h265"})
	requestEncodedRemoval(t, pool, otherVersion.recordingID, "h264")
	setAssetState(t, pool, otherVersion.originalID, "deleted")

	// 要求行あり + desired に無い + 原本なし + h265 も外した → どちらも最後の版なので消えない。
	lastCopy := seedRemovedEncodedRecording(t, pool, mediaDir, 9105, []string{})
	requestEncodedRemoval(t, pool, lastCopy.recordingID, "h264")
	requestEncodedRemoval(t, pool, lastCopy.recordingID, "h265")
	setAssetState(t, pool, lastCopy.originalID, "deleted")

	rows, err := q.ListRemovedEncodedAssetsToDelete(context.Background(), deleteReconcileRowLimit)
	if err != nil {
		t.Fatalf("ListRemovedEncodedAssetsToDelete: %v", err)
	}
	var got []int64
	for _, r := range rows {
		got = append(got, r.ID)
	}
	want := []int64{removed.h264ID, otherVersion.h264ID}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("removed encoded candidates = %v, want %v (removed=%d readded=%d drifted=%d otherVersion=%d lastCopy=%d/%d)",
			got, want, removed.h264ID, readded.h264ID, drifted.h264ID, otherVersion.h264ID, lastCopy.h264ID, lastCopy.h265ID)
	}
}

// 外した版は 1 パスで消え、要求行も同じ確定で消える。残す版と原本は触らず、
// 外した版は再エンコードの候補にもならない。
func TestDeleteReconcileWorker_RemovedEncoded_DeletesOnlyThatVersion(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	f := seedRemovedEncodedRecording(t, pool, mediaDir, 9201, []string{"h265"})
	requestEncodedRemoval(t, pool, f.recordingID, "h264")

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, f.h264ID); got != "deleted" {
		t.Errorf("h264 state = %q, want deleted", got)
	}
	if fileExists(f.h264Path) {
		t.Error("h264 file still exists, want removed")
	}
	if removalRequestExists(t, pool, f.recordingID, "h264") {
		t.Error("removal request still exists after the version was deleted")
	}
	if got := assetState(t, pool, f.h265ID); got != "active" {
		t.Errorf("h265 state = %q, want active", got)
	}
	if !fileExists(f.h265Path) {
		t.Error("h265 file was removed, want kept")
	}
	if got := assetState(t, pool, f.originalID); got != "active" {
		t.Errorf("original state = %q, want active", got)
	}

	missing, err := sqlcgen.New(pool).ListMissingEncodeProfiles(context.Background(), sqlcgen.ListMissingEncodeProfilesParams{
		KnownProfiles: []string{"h264", "h265"},
		CutProfiles:   []string{},
		RowLimit:      100,
	})
	if err != nil {
		t.Fatalf("ListMissingEncodeProfiles: %v", err)
	}
	for _, m := range missing {
		if m.RecordingID == f.recordingID {
			t.Errorf("missing encode profile %q listed for the recording, want none (h264 was removed from desired)", m.Profile)
		}
	}
}

// deleting のまま止まった外した版は、足し直されたら active に戻り（否定形の参照）、
// 足し直されていなければ pending 経路が続けて消す（OR の参照）。ブレーカーを発動させて
// おく。発動中は新規の削除が止まり、pending の再開だけが進む。発動していないと、否定形が
// 誤って active に戻した行も同じパスの新規候補として消え、結果が区別できない。
func TestDeleteReconcileWorker_RemovedEncodedDeleting_ReaddRevertsOtherwiseResumes(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	q := sqlcgen.New(pool)
	if err := breaker.Trip(context.Background(), q, "", breaker.DeleteReconcile, 1, breaker.Sample{Total: 2}); err != nil {
		t.Fatalf("tripping breaker: %v", err)
	}

	readded := seedRemovedEncodedRecording(t, pool, mediaDir, 9301, []string{"h265"})
	requestEncodedRemoval(t, pool, readded.recordingID, "h264")
	setAssetState(t, pool, readded.h264ID, "deleting")
	if err := q.AppendRecordingEncodeProfiles(context.Background(), sqlcgen.AppendRecordingEncodeProfilesParams{
		ID: readded.recordingID, Profiles: []string{"h264"},
	}); err != nil {
		t.Fatalf("AppendRecordingEncodeProfiles: %v", err)
	}
	if removalRequestExists(t, pool, readded.recordingID, "h264") {
		t.Error("removal request survived re-adding the profile, want cleared in the same statement")
	}

	stuck := seedRemovedEncodedRecording(t, pool, mediaDir, 9302, []string{"h265"})
	requestEncodedRemoval(t, pool, stuck.recordingID, "h264")
	setAssetState(t, pool, stuck.h264ID, "deleting")

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, readded.h264ID); got != "active" {
		t.Errorf("re-added h264 state = %q, want active", got)
	}
	if !fileExists(readded.h264Path) {
		t.Error("re-added h264 file was removed, want kept")
	}
	if got := assetState(t, pool, stuck.h264ID); got != "deleted" {
		t.Errorf("stuck h264 state = %q, want deleted", got)
	}
	if fileExists(stuck.h264Path) {
		t.Error("stuck h264 file still exists, want removed")
	}
}

// 外した版もブレーカーの 1 パス合計に入る。ごみ箱 1 件 + 外した版 2 件で閾値 2 を超える。
func TestDeleteReconcileWorker_CircuitBreaker_CountsRemovedEncoded(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	trashed := insertTestRecordingWithEventID(t, pool, 9401)
	trashAsset := seedOriginalAsset(t, pool, mediaDir, trashed, "trip/trash.m2ts", []byte("data"))
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET deleted_at = $1 WHERE id = $2", time.Now().Add(-40*24*time.Hour), trashed); err != nil {
		t.Fatalf("marking recording deleted: %v", err)
	}
	a := seedRemovedEncodedRecording(t, pool, mediaDir, 9402, []string{"h265"})
	requestEncodedRemoval(t, pool, a.recordingID, "h264")
	b := seedRemovedEncodedRecording(t, pool, mediaDir, 9403, []string{"h265"})
	requestEncodedRemoval(t, pool, b.recordingID, "h264")

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, MaxDeletesPerPass: 2}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	for _, id := range []int64{trashAsset, a.h264ID, b.h264ID} {
		if got := assetState(t, pool, id); got != "active" {
			t.Errorf("asset %d state = %q, want active (breaker should have withheld deletes)", id, got)
		}
	}
	cb, err := sqlcgen.New(pool).GetCircuitBreaker(context.Background(), sqlcgen.GetCircuitBreakerParams{
		Site: "", Name: breaker.DeleteReconcile,
	})
	if err != nil {
		t.Fatalf("expected circuit breaker to be tripped, got error: %v", err)
	}
	if cb.Pending != 3 {
		t.Errorf("breaker pending = %d, want 3", cb.Pending)
	}
}
