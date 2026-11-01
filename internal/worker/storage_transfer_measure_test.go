package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// settleTransferWork は、Work が slice の期限による snooze（遅延 0）を返す間は続けて呼び、それ以外の結果を返す。
// 測定や照合は、読みが 1 slice に収まらない限り完了まで snooze を挟むので、その間の結果をまとめて読む。
func settleTransferWork(t *testing.T, work func() error) error {
	t.Helper()
	for range 10 {
		err := work()
		if delay, ok := jobq.SnoozeDelay(err); !ok || delay != 0 {
			return err
		}
	}
	t.Fatal("transfer did not settle in 10 Work calls")
	return nil
}

// progressRow は、コピーの進捗の行のうち、この検査が見る列である。
type progressRow struct {
	verifying bool
	offset    int64
	handedOff bool
}

// readProgressRow は copyID の進捗の行を読む。行が無ければ ok=false。
func readProgressRow(t *testing.T, pool *pgxpool.Pool, copyID int64) (row progressRow, ok bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
SELECT verifying_source, offset_bytes, handed_off FROM media_asset_copy_transfer_progress WHERE copy_id = $1`, copyID).
		Scan(&row.verifying, &row.offset, &row.handedOff)
	if errors.Is(err, pgx.ErrNoRows) {
		return progressRow{}, false
	}
	if err != nil {
		t.Fatalf("reading progress row for copy %d: %v", copyID, err)
	}
	return row, true
}

// transferAttemptCount は copyID の失敗の数と最後の理由を返す。行が無ければ 0 と空文字。
func transferAttemptCount(t *testing.T, pool *pgxpool.Pool, copyID int64) (int, string) {
	t.Helper()
	var n int
	var last string
	if err := pool.QueryRow(context.Background(), `
SELECT COALESCE(MAX(attempt_count), 0), COALESCE(MAX(last_error), '') FROM media_asset_transfer_attempts WHERE copy_id = $1`, copyID).
		Scan(&n, &last); err != nil {
		t.Fatalf("reading transfer attempts for copy %d: %v", copyID, err)
	}
	return n, last
}

// recordedAssetSHA256 は論理資産の SHA-256 を返す。未知なら nil。
func recordedAssetSHA256(t *testing.T, pool *pgxpool.Pool, assetID int64) []byte {
	t.Helper()
	var sum []byte
	if err := pool.QueryRow(context.Background(), `SELECT sha256 FROM media_assets WHERE id = $1`, assetID).Scan(&sum); err != nil {
		t.Fatalf("reading media asset %d SHA-256: %v", assetID, err)
	}
	return sum
}

// transferTempFiles は dir にある転送 temp の名前を返す。
func transferTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("listing %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), mediapath.StorageTransferTempFilePrefix) {
			names = append(names, e.Name())
		}
	}
	return names
}

// adoptVerifyFixture は、宛先が既にあり論理資産の SHA-256 が既知のとき、採用の経路で原本の照合に入る転送を用意する。
// 転送元は test-store の原本、宛先は archive に置く。帯域制限と slice は、照合が複数の Work にまたがる長さにする。
type adoptVerifyFixture struct {
	pool    *pgxpool.Pool
	roots   map[string]string
	media   *pgstore.Media
	cfg     config.StorageConfig
	worker  StorageTransferWorker
	assetID int64
	args    jobs.StorageTransferArgs
}

const adoptVerifyRelPath = "sites/default/transfer/source.ts"

func newAdoptVerifyFixture(t *testing.T, source, destination []byte, expected [sha256.Size]byte) adoptVerifyFixture {
	t.Helper()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	assetID := transferTestOriginal(t, pool, roots["test-store"], source)
	if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET sha256 = $1 WHERE id = $2`, expected[:], assetID); err != nil {
		t.Fatalf("setting logical checksum: %v", err)
	}
	cfg := transferTestConfig("archive", "archive", "test-store")
	cfg.Transfer.Bandwidth = map[string]string{"test-store": "64KiBps"}
	media := pgstore.NewMedia(pool, "test-store", "")
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", mustOriginalRules(t, cfg), cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.Derived, storageTransferAttemptLimit)
	if err != nil || !ok {
		t.Fatalf("preparing adoptable transfer = (%+v, %t, %v)", transfer, ok, err)
	}
	destinationPath := filepath.Join(roots["archive"], filepath.FromSlash(transfer.RelPath))
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destinationPath, destination, 0o644); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := cfg.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	restore := storageTransferSlice
	storageTransferSlice = 150 * time.Millisecond
	t.Cleanup(func() { storageTransferSlice = restore })
	return adoptVerifyFixture{
		pool: pool, roots: roots, media: media, cfg: cfg, assetID: assetID,
		worker: StorageTransferWorker{Storage: media, Config: cfg, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()},
		args:   jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint},
	}
}

// drivePastVerification は Work を、照合が終わるまで繰り返す。途中の snooze は遅延 0 だけを許し、
// 各 snooze の後に、照合が途中で、失敗がまだ数えられていないことを確かめる。終わった Work の結果を返す。
func (f adoptVerifyFixture) drivePastVerification(t *testing.T, wantDuring int) int {
	t.Helper()
	snoozes := 0
	for range 300 {
		err := f.worker.Work(context.Background(), f.args)
		if err == nil {
			return snoozes
		}
		requireSliceSnooze(t, err, "verifying the source")
		snoozes++
		copyID := transferTestCopyID(t, f.pool, f.assetID, "archive")
		row, ok := readProgressRow(t, f.pool, copyID)
		if !ok || !row.verifying || !row.handedOff || row.offset <= 0 || row.offset > int64(len(f.sourceBytes(t))) {
			t.Fatalf("progress after verification slice %d = (%+v, %t), want a handed-off verification with 0 < offset <= size", snoozes, row, ok)
		}
		if n, _ := transferAttemptCount(t, f.pool, copyID); n != wantDuring {
			t.Fatalf("failures counted in the middle of verification (slice %d): attempts = %d, want %d", snoozes, n, wantDuring)
		}
	}
	t.Fatal("verification did not finish in 300 Work calls")
	return snoozes
}

func (f adoptVerifyFixture) sourceBytes(t *testing.T) []byte {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(f.roots["test-store"], filepath.FromSlash(adoptVerifyRelPath)))
	if err != nil {
		t.Fatalf("reading source: %v", err)
	}
	return got
}

// TestStorageTransferWorkerVerifiesMismatchAcrossSlicesBeforeCountingFailure は、原本の不一致の照合が slice にまたがり、
// 照合の途中では失敗が数えられず、照合の終わりで 1 回だけ数えられることを確かめる。
// 転送元が正しいときと壊れているときで、記録される理由が違うことも確かめる。
func TestStorageTransferWorkerVerifiesMismatchAcrossSlicesBeforeCountingFailure(t *testing.T) {
	content := bytes.Repeat([]byte("verified-source-"), 12800)
	other := bytes.Repeat([]byte("damaged-bytes--"), 12800)
	corrupted := bytes.Clone(content)
	corrupted[0] ^= 0xff
	expected := sha256.Sum256(content)
	for _, tc := range []struct {
		name       string
		source     []byte
		wantReason string
	}{
		{name: "valid source: the destination is what differs", source: content, wantReason: "destination differs from the valid source"},
		{name: "corrupt source: the source is what differs", source: corrupted, wantReason: "source copy does not match the logical media asset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdoptVerifyFixture(t, tc.source, other, expected)
			snoozes := f.drivePastVerification(t, 0)
			if snoozes < 2 {
				t.Fatalf("verification finished after %d snoozes; want the slice limit to split it", snoozes)
			}
			copyID := transferTestCopyID(t, f.pool, f.assetID, "archive")
			n, last := transferAttemptCount(t, f.pool, copyID)
			if n != 1 || !strings.Contains(last, tc.wantReason) {
				t.Fatalf("recorded failure = (%d, %q), want one failure containing %q", n, last, tc.wantReason)
			}
			if row, ok := readProgressRow(t, f.pool, copyID); ok && row.verifying {
				t.Fatal("verification flag is still set after the verification finished")
			}
		})
	}
}

// TestStorageTransferWorkerCountsVerifiedFailureAtAttemptLimitEdge は、失敗が上限の 1 つ手前（9 回）のときに不一致が見つかっても、
// 照合が最後まで回り、終わりで 1 回だけ数えられて上限に達することを確かめる。
// 照合の途中で数えたり予約を止めたりすると、照合が完了せずに終わる。
func TestStorageTransferWorkerCountsVerifiedFailureAtAttemptLimitEdge(t *testing.T) {
	content := bytes.Repeat([]byte("verified-source-"), 12800)
	other := bytes.Repeat([]byte("damaged-bytes--"), 12800)
	expected := sha256.Sum256(content)
	f := newAdoptVerifyFixture(t, content, other, expected)
	ctx := context.Background()
	transfer, ok, err := f.media.PrepareMediaAssetTransfer(ctx, f.assetID, "archive", mustOriginalRules(t, f.cfg), f.cfg.FSStoreNames(), f.cfg.FSStoreNames(), f.cfg.FSStoreNames(), f.cfg.Derived, storageTransferAttemptLimit)
	if err != nil || !ok {
		t.Fatalf("preparing transfer = (%+v, %t, %v)", transfer, ok, err)
	}
	for range storageTransferAttemptLimit - 1 {
		if err := f.media.RecordMediaAssetTransferFailure(ctx, transfer.CopyID, "earlier attempt"); err != nil {
			t.Fatal(err)
		}
	}
	snoozes := f.drivePastVerification(t, int(storageTransferAttemptLimit)-1)
	if snoozes < 2 {
		t.Fatalf("verification finished after %d snoozes; want the slice limit to split it", snoozes)
	}
	n, last := transferAttemptCount(t, f.pool, transfer.CopyID)
	if n != int(storageTransferAttemptLimit) || !strings.Contains(last, "destination differs from the valid source") {
		t.Fatalf("recorded failure = (%d, %q), want the limit reached by the verified failure", n, last)
	}
}

// TestStorageTransferWorkerMeasuresUnknownSourceAcrossSlicesBeforeAdopting は、宛先が既にあって論理資産の SHA-256 が未知の
// 原本で、転送元を slice ごとに測り、測り終えてから既知の経路で宛先を採用することを確かめる。
// 測り直しは保存された位置から続き、先頭から読み直さない。
func TestStorageTransferWorkerMeasuresUnknownSourceAcrossSlicesBeforeAdopting(t *testing.T) {
	content := bytes.Repeat([]byte("measured-source-"), 12800)
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	assetID := transferTestOriginal(t, pool, roots["test-store"], content)
	cfg := transferTestConfig("archive", "archive", "test-store")
	cfg.Transfer.Bandwidth = map[string]string{"test-store": "64KiBps"}
	media := pgstore.NewMedia(pool, "test-store", "")
	transfer, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive", mustOriginalRules(t, cfg), cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.FSStoreNames(), cfg.Derived, storageTransferAttemptLimit)
	if err != nil || !ok {
		t.Fatalf("preparing adoptable transfer = (%+v, %t, %v)", transfer, ok, err)
	}
	destinationPath := filepath.Join(roots["archive"], filepath.FromSlash(transfer.RelPath))
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destinationPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := cfg.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	restore := storageTransferSlice
	storageTransferSlice = 150 * time.Millisecond
	t.Cleanup(func() { storageTransferSlice = restore })
	worker := StorageTransferWorker{Storage: media, Config: cfg, Roots: roots, ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir()}
	args := jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}

	measuredSlices := 0
	last := int64(0)
	for range 300 {
		err := worker.Work(context.Background(), args)
		if err == nil {
			break
		}
		requireSliceSnooze(t, err, "measuring or adopting the destination")
		if recordedAssetSHA256(t, pool, assetID) != nil {
			continue
		}
		measuredSlices++
		copyID := transferTestCopyID(t, pool, assetID, "archive")
		row, ok := readProgressRow(t, pool, copyID)
		if !ok || !row.handedOff || row.offset <= last || row.offset > int64(len(content)) {
			t.Fatalf("measuring slice %d saved (%+v, %t), want a handed-off position beyond %d and within the source", measuredSlices, row, ok, last)
		}
		last = row.offset
	}
	if measuredSlices < 1 {
		t.Fatal("source was measured in no slice; want the slice limit to split the measurement")
	}
	sum := sha256.Sum256(content)
	if got := recordedAssetSHA256(t, pool, assetID); !bytes.Equal(got, sum[:]) {
		t.Fatalf("media_assets.sha256 = %x, want %x", got, sum)
	}
	if got, err := os.ReadFile(destinationPath); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("destination after adoption = %d bytes, %v; want the source bytes", len(got), err)
	}
	if state := transferTestCopyState(t, pool, assetID); state != "active" {
		t.Fatalf("archive copy state = %q, want active", state)
	}
}

// transferTestCopyState は archive の copy の状態を返す。
func transferTestCopyState(t *testing.T, pool *pgxpool.Pool, assetID int64) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive'`, assetID).Scan(&state); err != nil {
		t.Fatalf("reading archive copy state: %v", err)
	}
	return state
}

// stallingReadSeeker は after バイトまで読ませ、その後は ctx が終わるまで返さない。slice の期限で止まる転送元を作る。
type stallingReadSeeker struct {
	*bytes.Reader
	ctx   context.Context
	after int64
	read  int64
}

func (s *stallingReadSeeker) Read(p []byte) (int, error) {
	if s.read >= s.after {
		<-s.ctx.Done()
		return 0, s.ctx.Err()
	}
	if remaining := s.after - s.read; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := s.Reader.Read(p)
	s.read += int64(n)
	return n, err
}

// TestStorageTransferWorkerResumesSourceVerificationAfterHandoff は、照合が途中で止まったあとに、
// きれいな引き継ぎでは保存した位置から、きれいでない引き継ぎでは先頭から読み直すことを確かめる。
// どちらも temp は作らず、照合の途中では失敗が数えられない。
func TestStorageTransferWorkerResumesSourceVerificationAfterHandoff(t *testing.T) {
	for _, tc := range []struct {
		name           string
		uncleanHandoff bool
	}{
		{name: "clean handoff continues from the saved position"},
		{name: "unclean handoff starts the verification from the beginning", uncleanHandoff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := storageTransferSlice
			storageTransferSlice = time.Second
			t.Cleanup(func() { storageTransferSlice = restore })
			content := bytes.Repeat([]byte("remote-verified-"), 12800)
			served := bytes.Clone(content)
			served[0] ^= 0xff
			var mu sync.Mutex
			calls := 0
			stalling := true
			f := newTransferTestRemoteFixture(t, content, true, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				mu.Lock()
				calls++
				first := calls == 1
				hold := stalling
				mu.Unlock()
				body := bytes.NewReader(served)
				if first || !hold {
					http.ServeContent(w, r, "", time.Time{}, body)
					return
				}
				http.ServeContent(w, r, "", time.Time{}, &stallingReadSeeker{Reader: body, ctx: r.Context(), after: 64 << 10})
			})
			worker := f.worker(t)
			ctx := context.Background()

			// 1 回目の Work: コピーが全部読んで不一致を見つけ、照合の最初の slice が止まる。
			requireSliceSnooze(t, worker.Work(ctx, f.args()), "verification slice that stalls")
			copyID := transferTestCopyID(t, f.pool, f.assetID, "archive")
			row, ok := readProgressRow(t, f.pool, copyID)
			size := int64(len(content))
			if !ok || !row.verifying || row.offset <= 0 || row.offset >= size {
				t.Fatalf("progress after the stalled verification slice = (%+v, %t), want a verification saved part-way", row, ok)
			}
			if n, _ := transferAttemptCount(t, f.pool, copyID); n != 0 {
				t.Fatalf("failure counted during verification: attempts = %d, want 0", n)
			}
			dir := filepath.Dir(filepath.Join(f.roots["archive"], filepath.FromSlash(transferTestCopyRelPath(t, f.pool, f.assetID, "archive"))))
			if names := transferTempFiles(t, dir); len(names) != 0 {
				t.Fatalf("transfer temp files during verification = %v, want none", names)
			}
			ranges := f.streamer.seenRanges()
			if got := ranges[len(ranges)-1]; got != "" {
				t.Fatalf("first verification request Range = %q, want none (from the start)", got)
			}

			mu.Lock()
			stalling = false
			mu.Unlock()
			wantRange := fmt.Sprintf("bytes=%d-", row.offset)
			if tc.uncleanHandoff {
				// 持ち主が保存せずに止まった（lease だけが切れた）。次の試行は先頭から読み直す。
				if _, err := f.pool.Exec(ctx, `
UPDATE media_asset_copy_transfer_progress SET handed_off = false, lease_expires_at = now() - interval '1 minute' WHERE copy_id = $1`, copyID); err != nil {
					t.Fatalf("expiring verification lease: %v", err)
				}
				wantRange = ""
			}
			if err := worker.Work(ctx, f.args()); err != nil {
				t.Fatalf("verification Work after the handoff: %v", err)
			}
			ranges = f.streamer.seenRanges()
			if got := ranges[len(ranges)-1]; got != wantRange {
				t.Fatalf("verification request after the handoff had Range %q, want %q", got, wantRange)
			}
			if n, last := transferAttemptCount(t, f.pool, copyID); n != 1 || !strings.Contains(last, "does not match the logical media asset") {
				t.Fatalf("recorded failure = (%d, %q), want one source mismatch", n, last)
			}
			if f.destinationExists() {
				t.Fatal("mismatched source was published")
			}
			if names := transferTempFiles(t, dir); len(names) != 0 {
				t.Fatalf("transfer temp files after verification = %v, want none", names)
			}
		})
	}
}

// TestStorageTransferWorkerMeasuresUnknownDerivedSourceBeforeSending は、SHA-256 が未知の派生物が、転送元を slice ごとに測り、
// 測り終えてから既知の経路で送ることを確かめる。測っている間は、宛先の本体もアップロードも scratch も作らない。
func TestStorageTransferWorkerMeasuresUnknownDerivedSourceBeforeSending(t *testing.T) {
	f := newDerivedResumeFixtureWithSHA(t, false)
	measured := 0
	for range 300 {
		shaBefore := recordedAssetSHA256(t, f.pool, f.assetID)
		mark := len(f.source(t).getOffsets)
		err := f.work(t)
		opens := f.source(t).getOffsets[mark:]
		if shaBefore == nil {
			requireSliceSnooze(t, err, "measuring the derived source")
			// 測定が末尾まで済んでいた Work は転送元を開かないことがある（開くのは未読の位置からだけ）。
			if measured > 0 && len(opens) > 0 && opens[0] == 0 {
				t.Fatalf("a measuring Work re-read the source from 0: %v", opens)
			}
			measured++
			if upload := progressUploadStore(t, f.pool, f.copyID(t)); upload != nil {
				t.Fatalf("upload store %q set while the source was still being measured", *upload)
			}
			if got := f.scratchEntries(t); len(got) != 0 {
				t.Fatalf("scratch entries during measurement = %v, want none", got)
			}
			if fileExists(filepath.Join(f.roots["archive"], filepath.FromSlash(transferTestCopyRelPath(t, f.pool, f.assetID, "archive")))) {
				t.Fatal("destination was published while the source was still being measured")
			}
			continue
		}
		if err == nil {
			break
		}
		requireSliceSnooze(t, err, "sending the measured derived source")
	}
	if measured < 2 {
		t.Fatalf("source measured in %d Work calls; want the slice limit to split the measurement", measured)
	}
	f.assertPublished(t)
	sum := sha256.Sum256(f.content)
	if got := recordedAssetSHA256(t, f.pool, f.assetID); !bytes.Equal(got, sum[:]) {
		t.Fatalf("media_assets.sha256 = %x, want %x", got, sum)
	}
	if got := f.scratchEntries(t); len(got) != 0 {
		t.Fatalf("scratch entries after the transfer = %v, want none", got)
	}
}

// TestStorageTransferWorkerDoesNotRecordSHA256AfterGenerationChanges は、派生物の SHA-256 を測っている間に論理資産の世代が変わったら、
// 測った値を論理資産に書かないことを確かめる。世代は測り始めた時点の値で比べる。
func TestStorageTransferWorkerDoesNotRecordSHA256AfterGenerationChanges(t *testing.T) {
	f := newDerivedResumeFixtureWithSHA(t, false)
	// 測定を 1 Work に収める。帯域制限を外すので、設定を作り直して fingerprint も合わせる。
	cfg := transferTestConfig("archive", "archive", "test-store")
	fingerprint, err := cfg.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	restore := storageTransferSlice
	storageTransferSlice = time.Minute
	t.Cleanup(func() { storageTransferSlice = restore })
	worker := f.worker
	worker.Config = cfg
	args := f.args
	args.PolicyFingerprint = fingerprint
	// 測定の最初の読みの直前に、論理資産の世代を進める。
	f.source(t).beforeGetFrom = func() {
		if _, err := f.pool.Exec(context.Background(), `UPDATE media_assets SET derived_generation = COALESCE(derived_generation, 0) + 1 WHERE id = $1`, f.assetID); err != nil {
			t.Errorf("advancing derived generation: %v", err)
		}
	}
	err = worker.Work(context.Background(), args)
	requireSliceSnooze(t, err, "measurement that finishes after the generation changed")
	if got := recordedAssetSHA256(t, f.pool, f.assetID); got != nil {
		t.Fatalf("SHA-256 %x recorded for a generation that changed during the measurement, want none", got)
	}
}

// progressUploadStore は copyID の進捗の送信置き場を返す。無ければ nil。
func progressUploadStore(t *testing.T, pool *pgxpool.Pool, copyID int64) *string {
	t.Helper()
	var store *string
	err := pool.QueryRow(context.Background(), `SELECT upload_store FROM media_asset_copy_transfer_progress WHERE copy_id = $1`, copyID).Scan(&store)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading upload store for copy %d: %v", copyID, err)
	}
	return store
}

// scratchEntries は、派生物の転送が使う scratch の中身の名前を返す。
func (f derivedResumeFixture) scratchEntries(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.worker.ScratchDir)
	if err != nil {
		t.Fatalf("listing scratch: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
