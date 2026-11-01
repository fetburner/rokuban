package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// fakeResumableStore は fsstore を埋め込み、テストが GetFrom・CompleteUpload・AbortUpload の挙動だけを変えられるようにする。
// 埋め込みで Store / Reader / RangeReader / Resumable を全部委譲するので、worker は本物と同じ経路を通る。
type fakeResumableStore struct {
	*fsstore.Store
	getOffsets    []int64     // GetFrom が呼ばれた offset の列
	beforeGetFrom func()      // GetFrom の最初に呼ぶ。テストが読みの途中で論理資産の世代を変えるのに使う。
	completeErr   []error     // CompleteUpload が先頭から返す。nil の要素は本物の完了。尽きたら本物。
	abortResult   []fakeAbort // AbortUpload が先頭から返す。尽きたら本物。
	abortCalls    int         // AbortUpload が呼ばれた回数
	beforeAbort   func()      // AbortUpload の最初に呼ぶ。テストが掃除の途中で締切を切るのに使う。
	// lastAbortDeadline は直近の AbortUpload に渡された ctx の締切（無ければ零値）。
	lastAbortDeadline time.Time
}

type fakeAbort struct {
	gone bool
	err  error
}

func (s *fakeResumableStore) GetFrom(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	s.getOffsets = append(s.getOffsets, offset)
	if s.beforeGetFrom != nil {
		s.beforeGetFrom()
	}
	return s.Store.GetFrom(ctx, key, offset)
}

func (s *fakeResumableStore) CompleteUpload(ctx context.Context, state []byte) error {
	if len(s.completeErr) > 0 {
		err := s.completeErr[0]
		s.completeErr = s.completeErr[1:]
		if err != nil {
			return err
		}
	}
	return s.Store.CompleteUpload(ctx, state)
}

func (s *fakeResumableStore) AbortUpload(ctx context.Context, state []byte) (bool, error) {
	s.abortCalls++
	s.lastAbortDeadline, _ = ctx.Deadline()
	if s.beforeAbort != nil {
		s.beforeAbort()
	}
	if len(s.abortResult) > 0 {
		r := s.abortResult[0]
		s.abortResult = s.abortResult[1:]
		return r.gone, r.err
	}
	return s.Store.AbortUpload(ctx, state)
}

// derivedResumeFixture は、slice ごとに止まる派生物の転送を 1 件用意する。
// 帯域制限 64KiB/s と 150ms の slice で、300 KB の本体が 10 回ほどの slice に分かれる。
type derivedResumeFixture struct {
	pool    *pgxpool.Pool
	roots   map[string]string
	objects map[string]objstore.Store
	worker  StorageTransferWorker
	args    jobs.StorageTransferArgs
	assetID int64
	relPath string
	content []byte
}

func newDerivedResumeFixture(t *testing.T) derivedResumeFixture {
	t.Helper()
	return newDerivedResumeFixtureWithSHA(t, true)
}

// newDerivedResumeFixtureWithSHA は、論理資産の SHA-256 を knownSHA に応じて記録するか、未計測のまま残す。
func newDerivedResumeFixtureWithSHA(t *testing.T, knownSHA bool) derivedResumeFixture {
	t.Helper()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	profile := "web"
	relPath := "sites/default/transfer/resumed.mp4"
	content := bytes.Repeat([]byte("derived-resume-"), 20000)
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, relPath, content)
	if knownSHA {
		sum := sha256.Sum256(content)
		if _, err := pool.Exec(context.Background(), "UPDATE media_assets SET sha256 = $1 WHERE id = $2", sum[:], assetID); err != nil {
			t.Fatalf("setting logical checksum: %v", err)
		}
	}
	cfg := transferTestConfig("archive", "archive", "test-store")
	cfg.Transfer.Bandwidth = map[string]string{"test-store": "64KiBps"}
	fingerprint, err := cfg.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	restore := storageTransferSlice
	storageTransferSlice = 150 * time.Millisecond
	t.Cleanup(func() { storageTransferSlice = restore })
	objects := make(map[string]objstore.Store, len(roots))
	for name, root := range roots {
		fs, err := fsstore.New(root)
		if err != nil {
			t.Fatalf("creating %s test object store: %v", name, err)
		}
		objects[name] = &fakeResumableStore{Store: fs}
	}
	return derivedResumeFixture{
		pool: pool, roots: roots, objects: objects,
		worker: StorageTransferWorker{
			Storage: pgstore.NewMedia(pool, "test-store", ""), Config: cfg, Roots: roots,
			ObjectStores: objects, ScratchDir: t.TempDir(),
		},
		args:    jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint},
		assetID: assetID, relPath: relPath, content: content,
	}
}

func (f derivedResumeFixture) work(t *testing.T) error {
	t.Helper()
	return f.worker.Work(context.Background(), f.args)
}

func (f derivedResumeFixture) source(t *testing.T) *fakeResumableStore {
	t.Helper()
	return f.objects["test-store"].(*fakeResumableStore)
}

func (f derivedResumeFixture) target(t *testing.T) *fakeResumableStore {
	t.Helper()
	return f.objects["archive"].(*fakeResumableStore)
}

func (f derivedResumeFixture) copyID(t *testing.T) int64 {
	t.Helper()
	return transferTestCopyID(t, f.pool, f.assetID, "archive")
}

func (f derivedResumeFixture) copyState(t *testing.T) string {
	t.Helper()
	var state string
	if err := f.pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive'`, f.assetID).Scan(&state); err != nil {
		t.Fatalf("reading archive copy state: %v", err)
	}
	return state
}

// stagingFiles は宛先の directory にある、置き場の staging（生成物の一時ファイル）の名前を返す。
func (f derivedResumeFixture) stagingFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(filepath.Join(f.roots["archive"], filepath.FromSlash(f.relPath))))
	if err != nil {
		t.Fatalf("listing destination directory: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), mediapath.GeneratedAssetTempFilePrefix) {
			names = append(names, e.Name())
		}
	}
	return names
}

func (f derivedResumeFixture) abandonedCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_transfer_abandoned_uploads WHERE store = 'archive'`).Scan(&n); err != nil {
		t.Fatalf("counting abandoned uploads: %v", err)
	}
	return n
}

func (f derivedResumeFixture) attemptCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT COALESCE((SELECT attempt_count FROM media_asset_transfer_attempts WHERE copy_id = $1), 0)`, f.copyID(t)).Scan(&n); err != nil {
		t.Fatalf("counting transfer attempts: %v", err)
	}
	return n
}

// expireLease は、前の試行が保存せずに抜けた（lease だけが切れた）状態にする。期限は DB の now() で比べる。
func (f derivedResumeFixture) expireLease(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
UPDATE media_asset_copy_transfer_progress SET handed_off = false, lease_expires_at = now() - interval '1 minute'
WHERE copy_id = $1`, f.copyID(t)); err != nil {
		t.Fatalf("expiring transfer lease: %v", err)
	}
}

// driveToEnd は Work を繰り返し、コピーが active になるまで進める。途中の snooze は slice の期限だけを許す。
func (f derivedResumeFixture) driveToEnd(t *testing.T) {
	t.Helper()
	for range 300 {
		err := f.work(t)
		if err == nil {
			if state := f.copyState(t); state != "active" {
				t.Fatalf("Work finished without error but the archive copy is %q", state)
			}
			return
		}
		requireSliceSnooze(t, err, "driving the transfer to the end")
	}
	t.Fatal("transfer did not finish in 300 Work calls")
}

// assertPublished は、宛先が論理資産と同じ内容で、コピーが active であることを確かめる。
func (f derivedResumeFixture) assertPublished(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(f.roots["archive"], filepath.FromSlash(f.relPath)))
	if err != nil || !bytes.Equal(got, f.content) {
		t.Fatalf("destination = %d bytes, %v; want the %d source bytes", len(got), err, len(f.content))
	}
	if state := f.copyState(t); state != "active" {
		t.Fatalf("archive copy state = %q, want active", state)
	}
}

// requireSliceSnooze は、err が slice の期限による snooze（遅延 0）であることを確かめる。
func requireSliceSnooze(t *testing.T, err error, what string) {
	t.Helper()
	delay, ok := jobq.SnoozeDelay(err)
	if !ok || delay != 0 {
		t.Fatalf("%s: Work returned %v; want a slice snooze with delay 0", what, err)
	}
}

// TestStorageTransferWorkerResumesDerivedUploadAcrossSlices は、SHA-256 が既知の派生物が slice の期限で止まるたびに
// 進捗と送信状態を保存し、次の Work が保存された offset から転送元を読み直して完了することを確かめる。
func TestStorageTransferWorkerResumesDerivedUploadAcrossSlices(t *testing.T) {
	f := newDerivedResumeFixture(t)
	snoozes := 0
	for range 300 {
		err := f.work(t)
		if err == nil {
			break
		}
		requireSliceSnooze(t, err, "slice")
		snoozes++
		var handedOff bool
		var offset int64
		var uploadStore *string
		var uploadState []byte
		if err := f.pool.QueryRow(context.Background(), `
SELECT handed_off, offset_bytes, upload_store, upload_state FROM media_asset_copy_transfer_progress WHERE copy_id = $1`, f.copyID(t)).Scan(&handedOff, &offset, &uploadStore, &uploadState); err != nil {
			t.Fatalf("reading saved progress after a slice: %v", err)
		}
		if !handedOff || offset <= 0 || offset >= int64(len(f.content)) || uploadStore == nil || *uploadStore != "archive" || len(uploadState) == 0 {
			t.Fatalf("saved progress after slice %d = handed_off %t offset %d upload store %v upload state %d bytes; want a handed-off upload with 0 < offset < %d",
				snoozes, handedOff, offset, uploadStore, len(uploadState), len(f.content))
		}
	}
	if snoozes < 2 {
		t.Fatalf("transfer finished after %d snoozes; want the slice limit to split it", snoozes)
	}
	f.assertPublished(t)
	// 最後の Work を含め、Work ごとに転送元を 1 回開く。2 回目以降は先頭から読み直さない。
	offsets := f.source(t).getOffsets
	if len(offsets) != snoozes+1 {
		t.Fatalf("source opened %d times for %d snoozes, want one open per Work: %v", len(offsets), snoozes, offsets)
	}
	if offsets[0] != 0 {
		t.Fatalf("first source read offset = %d, want 0", offsets[0])
	}
	for _, off := range offsets[1:] {
		if off == 0 {
			t.Fatalf("a resumed slice re-read the source from 0: %v", offsets)
		}
	}
}

// TestStorageTransferWorkerRestartsDerivedUploadAfterUncleanHandoff は、保存せずに lease だけが切れた試行の後は
// 新しいアップロードから送り直し、古い送信状態を掃除待ちに移して、次の Work の掃除で staging と行を消すことを確かめる。
func TestStorageTransferWorkerRestartsDerivedUploadAfterUncleanHandoff(t *testing.T) {
	f := newDerivedResumeFixture(t)
	requireSliceSnooze(t, f.work(t), "first slice")
	before := f.stagingFiles(t)
	if len(before) != 1 {
		t.Fatalf("staging files after the first slice = %v, want exactly one", before)
	}
	f.expireLease(t)

	mark := len(f.source(t).getOffsets)
	requireSliceSnooze(t, f.work(t), "attempt after the lease expired")
	if offsets := f.source(t).getOffsets[mark:]; len(offsets) == 0 || offsets[0] != 0 {
		t.Fatalf("attempt after an unclean handoff read the source from %v; want a fresh upload from 0", offsets)
	}
	if n := f.abandonedCount(t); n != 1 {
		t.Fatalf("abandoned uploads after the new attempt started = %d, want the old upload alone (1)", n)
	}
	if !slices.Contains(f.stagingFiles(t), before[0]) {
		t.Fatalf("the old staging %q was removed before the cleanup ran", before[0])
	}

	// 次の Work は掃除を先に行うので、古い staging と掃除待ちの行が消える。
	requireSliceSnooze(t, f.work(t), "next attempt, which runs the cleanup")
	if slices.Contains(f.stagingFiles(t), before[0]) {
		t.Fatalf("the old staging %q survived the cleanup", before[0])
	}
	if n := f.abandonedCount(t); n != 0 {
		t.Fatalf("abandoned uploads after the cleanup = %d, want 0", n)
	}
	f.driveToEnd(t)
	f.assertPublished(t)
}

// TestStorageTransferWorkerRestartsDerivedUploadWhenCompleteIsInvalid は、完了が ErrUploadInvalid（S3 の InvalidPart 相当）を
// 返したら、その試行は失敗として数え、次の試行は新しいアップロードから送り直すことを確かめる。
func TestStorageTransferWorkerRestartsDerivedUploadWhenCompleteIsInvalid(t *testing.T) {
	f := newDerivedResumeFixture(t)
	f.target(t).completeErr = []error{objstore.ErrUploadInvalid}
	failures := 0
	checkAbandoned := false
	for range 300 {
		err := f.work(t)
		if checkAbandoned {
			checkAbandoned = false
			// 失敗の次の試行は新しいアップロードを始め、失敗した送信状態は掃除待ちに移っている。
			if n := f.abandonedCount(t); n != 1 {
				t.Fatalf("abandoned uploads after the attempt that follows the failure = %d, want 1", n)
			}
		}
		if err != nil {
			requireSliceSnooze(t, err, "slice")
			continue
		}
		if f.copyState(t) == "active" {
			break
		}
		failures++
		if failures > 1 {
			t.Fatalf("completion failed %d times; want only the injected ErrUploadInvalid", failures)
		}
		if n := f.attemptCount(t); n != 1 {
			t.Fatalf("transfer attempts after the invalid completion = %d, want 1", n)
		}
		checkAbandoned = true
	}
	if failures != 1 {
		t.Fatalf("invalid completions = %d, want 1", failures)
	}
	f.assertPublished(t)
}

// TestStorageTransferWorkerKeepsAbandonedUploadUntilStoreReportsGone は、掃除の Abort が「まだ残っている」を返す間は
// 掃除待ちの行を残し、Abort がエラーでも転送ジョブは失敗にしないこと、gone と答えて初めて行を消すことを確かめる。
func TestStorageTransferWorkerKeepsAbandonedUploadUntilStoreReportsGone(t *testing.T) {
	f := newDerivedResumeFixture(t)
	requireSliceSnooze(t, f.work(t), "first slice")
	first := f.stagingFiles(t)
	if len(first) != 1 {
		t.Fatalf("staging files after the first slice = %v, want exactly one", first)
	}
	f.expireLease(t)
	requireSliceSnooze(t, f.work(t), "attempt that abandons the first upload")
	if n := f.abandonedCount(t); n != 1 {
		t.Fatalf("abandoned uploads = %d, want the first upload (1)", n)
	}
	if got := f.stagingFiles(t); len(got) != 2 || !slices.Contains(got, first[0]) {
		t.Fatalf("staging files = %v, want the abandoned upload %q and the current one", got, first[0])
	}
	// Abort の 1 回目は「まだ残っている」、2 回目はエラー、3 回目以降は本物（gone）。
	f.target(t).abortResult = []fakeAbort{{gone: false}, {err: errors.New("store unavailable")}}

	requireSliceSnooze(t, f.work(t), "cleanup reports the upload still present")
	if n := f.abandonedCount(t); n != 1 {
		t.Fatalf("abandoned uploads after a not-gone answer = %d, want the row kept (1)", n)
	}
	requireSliceSnooze(t, f.work(t), "cleanup fails with an error; the transfer job must not fail")
	if n := f.abandonedCount(t); n != 1 {
		t.Fatalf("abandoned uploads after an Abort error = %d, want the row kept (1)", n)
	}
	requireSliceSnooze(t, f.work(t), "cleanup reports gone")
	if n := f.abandonedCount(t); n != 0 {
		t.Fatalf("abandoned uploads after gone = %d, want 0", n)
	}
	if slices.Contains(f.stagingFiles(t), first[0]) {
		t.Fatalf("the abandoned staging %q survived the gone answer", first[0])
	}
	f.driveToEnd(t)
	f.assertPublished(t)
}

// TestStorageTransferWorkerCleanupStopsWhenItRunsOutOfTime は、掃除の途中で締切が切れたら残りの掃除待ちの行に触れず、
// 掃除の Abort には専用の締切が掛かっていて、転送は次の Work で最後まで進むことを確かめる。
func TestStorageTransferWorkerCleanupStopsWhenItRunsOutOfTime(t *testing.T) {
	f := newDerivedResumeFixture(t)
	requireSliceSnooze(t, f.work(t), "first slice")
	// 掃除待ちの行を 2 件作る。各 Work は先頭で掃除をするので、1 件目は次の Work の掃除で「まだ残っている」と答えさせる。
	f.target(t).abortResult = []fakeAbort{{gone: false}}
	for range 2 {
		f.expireLease(t)
		requireSliceSnooze(t, f.work(t), "attempt that abandons the previous upload")
	}
	if n := f.abandonedCount(t); n != 2 {
		t.Fatalf("abandoned uploads = %d, want 2 before the cleanup", n)
	}

	// 1 件目の Abort の途中で締切を切る。Abort は「まだ残っている」を返すので、行は消えない。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := f.target(t)
	target.abortResult = []fakeAbort{{gone: false}}
	target.beforeAbort = cancel
	target.abortCalls = 0
	f.worker.abortAbandonedUploads(ctx, "archive")

	if target.abortCalls != 1 {
		t.Fatalf("AbortUpload calls after the deadline ran out = %d, want 1 (the rest must wait for the next Work)", target.abortCalls)
	}
	if n := f.abandonedCount(t); n != 2 {
		t.Fatalf("abandoned uploads = %d, want both rows kept (2)", n)
	}
	// 掃除の Abort には締切が掛かる（Work の締切を食わないための上限）。
	if left := time.Until(target.lastAbortDeadline); target.lastAbortDeadline.IsZero() || left <= 0 || left > abandonedUploadCleanupTimeout {
		t.Fatalf("cleanup Abort deadline = %v (%v from now), want a deadline within %v", target.lastAbortDeadline, left, abandonedUploadCleanupTimeout)
	}

	// 転送は止まらず、次の Work で掃除を終えて完了する。
	f.driveToEnd(t)
	f.assertPublished(t)
	// 掃除は Work の先頭で行う。完了した後の Work でも残りが片付くことを確かめる。
	if err := f.work(t); err != nil {
		t.Fatalf("Work after the transfer finished = %v, want nil", err)
	}
	if n := f.abandonedCount(t); n != 0 {
		t.Fatalf("abandoned uploads after the next Work = %d, want 0", n)
	}
}

// uploadStateOf は copyID の進捗行に保存された送信状態を返す。行が無いか未保存なら nil。
func uploadStateOf(t *testing.T, f derivedResumeFixture) []byte {
	t.Helper()
	var state []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT upload_state FROM media_asset_copy_transfer_progress WHERE copy_id = $1`, f.copyID(t)).Scan(&state); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reading saved upload state: %v", err)
	}
	return state
}

// TestStorageTransferWorkerRestartsAfterSourceLengthMismatch は、転送元が宣言より長くなって送信が ErrChecksumMismatch になったとき、
// ずれた転送元のバイトを含む送信状態を保存せず、次の試行が新しいアップロード（新しい staging）から始まることを確かめる。
// 送った slice の区切りで止まる場合は、保存された状態から続くこと（TestStorageTransferWorkerResumesDerivedUploadAcrossSlices）と対になる。
func TestStorageTransferWorkerRestartsAfterSourceLengthMismatch(t *testing.T) {
	f := newDerivedResumeFixture(t)
	requireSliceSnooze(t, f.work(t), "first slice")
	firstStaging := f.stagingFiles(t)
	if len(firstStaging) != 1 {
		t.Fatalf("staging files after the first slice = %v, want exactly one", firstStaging)
	}

	// 宣言より 1 バイト長い転送元にする。送信は宣言の長さを超えたところで ErrChecksumMismatch になる。
	sourcePath := filepath.Join(f.roots["test-store"], filepath.FromSlash(f.relPath))
	if err := os.WriteFile(sourcePath, append(bytes.Clone(f.content), '!'), 0o644); err != nil {
		t.Fatalf("lengthening the source: %v", err)
	}
	var prev []byte
	var err error
	for range 300 {
		prev = uploadStateOf(t, f)
		err = f.work(t)
		if _, snooze := jobq.SnoozeDelay(err); !snooze {
			break
		}
	}
	if err != nil {
		t.Fatalf("mismatched attempt = %v, want nil after recording the failure", err)
	}
	if n, last := transferAttemptCount(t, f.pool, f.copyID(t)); n != 1 || !strings.Contains(last, "derived source mismatch") {
		t.Fatalf("recorded failure = (%d, %q), want one derived source mismatch", n, last)
	}
	if after := uploadStateOf(t, f); !bytes.Equal(after, prev) {
		t.Fatal("the mismatched attempt saved its upload state; want the last clean state kept")
	}
	if row, ok := readProgressRow(t, f.pool, f.copyID(t)); ok && row.handedOff {
		t.Fatal("the mismatched attempt left the progress handed off; want the lease released without a save")
	}

	// 転送元を正しい長さに戻す。次の試行は保存された状態から続かず、新しいアップロードを始める。
	if err := os.WriteFile(sourcePath, f.content, 0o644); err != nil {
		t.Fatalf("restoring the source: %v", err)
	}
	requireSliceSnooze(t, f.work(t), "next attempt")
	if after := uploadStateOf(t, f); bytes.Equal(after, prev) {
		t.Fatal("the next attempt continued the previous upload state; want a new upload")
	}
	if n := f.abandonedCount(t); n != 1 {
		t.Fatalf("abandoned uploads after the next attempt = %d, want the previous upload (1)", n)
	}
	if got := f.stagingFiles(t); len(got) != 2 || !slices.Contains(got, firstStaging[0]) {
		t.Fatalf("staging files = %v, want the previous upload %q and the new one", got, firstStaging[0])
	}
	f.driveToEnd(t)
	f.assertPublished(t)
}
