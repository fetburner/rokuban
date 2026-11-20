package storagemaint

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// transferMarkerAt は物理転送の確定時刻（loop_success_markers の storage_transfer / all）を返す。
// 行が無ければ ok=false。
func transferMarkerAt(t *testing.T, pool *pgxpool.Pool) (at time.Time, ok bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT last_success_at FROM loop_success_markers WHERE loop = $1 AND scope = $2`,
		store.LoopStorageTransfer, store.StorageTransferScope).Scan(&at)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false
		}
		t.Fatalf("reading storage transfer marker: %v", err)
	}
	return at, true
}

// seedTransferWork は転送の対象を 1 件用意し、そのワーカーと引数を返す。
// original は原本の転送、derived は派生物（オブジェクト経由）の転送。
func seedTransferWork(t *testing.T, pool *pgxpool.Pool, kind string) (StorageTransferWorker, jobs.StorageTransferArgs) {
	t.Helper()
	roots := transferTestRoots(t, "test-store", "archive")
	cfg := transferTestConfig("archive", "archive", "test-store")
	fingerprint, err := cfg.PolicyFingerprint()
	testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
	var assetID int64
	switch kind {
	case "original":
		assetID = transferTestOriginal(t, pool, roots["test-store"], []byte("marker original bytes"))
	case "derived":
		recordingID := dbtest.Recording(t, pool)
		content := []byte("marker derived bytes")
		assetID = seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindThumbnail, nil, "sites/default/transfer/marker.jpg", content)
		sum := sha256.Sum256(content)
		testutil.MustExec(t, pool, "setting logical checksum", "UPDATE media_assets SET sha256 = $1 WHERE id = $2", sum[:], assetID)
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	worker := StorageTransferWorker{
		Storage: pgstore.NewMedia(pool, "test-store", ""), Config: cfg, Roots: roots,
		ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir(),
	}
	return worker, jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}
}

// 物理転送の確定時刻は、公開が active になった試行だけで進む（原本・派生物の両方の公開経路）。
func TestStorageTransferMarkerAdvancesOnPublish(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"original", "derived"} {
		t.Run(kind, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			worker, args := seedTransferWork(t, pool, kind)
			if _, ok := transferMarkerAt(t, pool); ok {
				t.Fatal("marker present before any publish")
			}
			testutil.RequireNoError(t, settleTransferWork(t, func() error {
				return worker.Work(context.Background(), args)
			}), "StorageTransferWorker.Work")
			at, ok := transferMarkerAt(t, pool)
			if !ok {
				t.Fatal("marker absent after the copy became active")
			}
			if time.Since(at) > time.Minute {
				t.Fatalf("marker = %v, want a time from this publish", at)
			}
		})
	}
}

// 公開に至らない経路では確定時刻を進めない。転送が no-op（コピーが既に active で
// 転送の対象が無い）のときは、以前の印があっても書かない。
func TestStorageTransferMarkerDoesNotAdvanceWithoutPublish(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	worker, args := seedTransferWork(t, pool, "original")
	testutil.RequireNoError(t, settleTransferWork(t, func() error {
		return worker.Work(context.Background(), args)
	}), "first Work")
	testutil.MustExec(t, pool, "clearing the marker", `DELETE FROM loop_success_markers WHERE loop = $1`, store.LoopStorageTransfer)
	testutil.RequireNoError(t, settleTransferWork(t, func() error {
		return worker.Work(context.Background(), args)
	}), "second Work (no-op)")
	if _, ok := transferMarkerAt(t, pool); ok {
		t.Fatal("marker written by a no-op transfer job, want no write before a publish")
	}
}

// 印の書き込みが失敗したら、公開（active 化）も取り消す。取り消さないと、再試行は
// 「既に active」として no-op になり、印は永久に進まない。印の書き込みを外へ出したら、
// この試験が active 化の残りで落ちる。
func TestStorageTransferMarkerFailureRollsBackPublish(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	worker, args := seedTransferWork(t, pool, "original")
	testutil.MustExec(t, pool, "rejecting the marker write", `
CREATE OR REPLACE FUNCTION reject_transfer_marker() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.loop = 'storage_transfer' THEN
    RAISE EXCEPTION 'transfer marker write rejected';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER reject_transfer_marker BEFORE INSERT OR UPDATE ON loop_success_markers
FOR EACH ROW EXECUTE FUNCTION reject_transfer_marker()`)
	if err := settleTransferWork(t, func() error {
		return worker.Work(context.Background(), args)
	}); err == nil {
		t.Fatal("Work with a rejected marker write = nil, want error")
	}
	if got := copyStateIn(t, pool, args.MediaAssetID, "archive"); got == "active" {
		t.Fatal("copy is active although the marker write failed; the publish must roll back with it")
	}
	if _, ok := transferMarkerAt(t, pool); ok {
		t.Fatal("marker present after a rejected write")
	}

	testutil.MustExec(t, pool, "lifting the rejection", `DROP TRIGGER reject_transfer_marker ON loop_success_markers`)
	testutil.RequireNoError(t, settleTransferWork(t, func() error {
		return worker.Work(context.Background(), args)
	}), "retry Work")
	if got := copyStateIn(t, pool, args.MediaAssetID, "archive"); got != "active" {
		t.Fatalf("copy state after retry = %q, want active", got)
	}
	if _, ok := transferMarkerAt(t, pool); !ok {
		t.Fatal("retry did not record the marker")
	}
}

// 同じ試行の公開が既に active になっていた（already_active）ときも、印は進む。
// 先行の試行が active にしたのと同じ成功として、この呼び出しも数える。
func TestCompleteMediaAssetTransferAlreadyActiveAdvancesMarker(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	content := []byte("already active bytes")
	assetID := transferTestOriginal(t, pool, roots["test-store"], content)
	cfg := transferTestConfig("archive", "archive", "test-store")
	rules := mustOriginalRules(t, cfg)
	media := pgstore.NewMedia(pool, "test-store", "")
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", rules, cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.Derived, store.MediaAssetTransferAttemptLimit)
	if err != nil || !ok {
		t.Fatalf("PrepareMediaAssetTransfer = (%+v, %t, %v)", transfer, ok, err)
	}
	sum := sha256.Sum256(content)
	active, outcome, err := media.CompleteMediaAssetTransfer(context.Background(), transfer, "archive", cfg.Derived, rules, sum[:])
	testutil.RequireNoError(t, err, "first CompleteMediaAssetTransfer")
	if !active || outcome != "activated" {
		t.Fatalf("first CompleteMediaAssetTransfer = (%t, %q), want (true, activated)", active, outcome)
	}
	testutil.MustExec(t, pool, "rewinding the marker", `UPDATE loop_success_markers SET last_success_at = '2000-01-01T00:00:00Z' WHERE loop = $1`, store.LoopStorageTransfer)

	active, outcome, err = media.CompleteMediaAssetTransfer(context.Background(), transfer, "archive", cfg.Derived, rules, sum[:])
	testutil.RequireNoError(t, err, "second CompleteMediaAssetTransfer")
	if !active || outcome != "already_active" {
		t.Fatalf("second CompleteMediaAssetTransfer = (%t, %q), want (true, already_active)", active, outcome)
	}
	at, ok := transferMarkerAt(t, pool)
	if !ok || !at.After(time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("marker after already_active = %v (present %t), want a time from this call", at, ok)
	}
}
