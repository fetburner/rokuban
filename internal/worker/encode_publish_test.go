package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
)

// installCountingFFmpeg は「入力のコピーに、試行ごとに違うバイト列（長さも違う）を
// 足して出力する」偽 ffmpeg を PATH に置く。installFakeFFmpeg の cp だと 2 本の
// 試行が同一バイトになり、「どちらの中身が canonical か」の断言が空虚になる。
// 試行番号は ffmpeg の起動順（テストは起動を直列に進める）。
func installCountingFFmpeg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffmpeg")
	script := `#!/bin/sh
set -e
input=""
output=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-i" ]; then input="$a"; fi
  prev="$a"
  output="$a"
done
counter="` + filepath.Join(dir, "counter") + `"
n=$(cat "$counter" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "$counter"
printf 'out_time_ms=1000\nprogress=end\n'
cp "$input" "$output"
printf 'attempt-%s:' "$n" >> "$output"
yes | head -c $((n*10)) >> "$output"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
	return path
}

// pauseBeforeEncodeLock は match した試行を、stage 完了後・rel_path lock 取得前で
// 止める。reached にはその試行の scratch 出力パスが届く。
func pauseBeforeEncodeLock(t *testing.T, match func(scratchOut string) bool) (reached <-chan string, release func()) {
	t.Helper()
	ch := make(chan string, 4)
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	orig := beforeEncodeLock
	t.Cleanup(func() {
		release()
		beforeEncodeLock = orig
	})
	beforeEncodeLock = func(scratchOut string) {
		if !match(scratchOut) {
			return
		}
		ch <- scratchOut
		<-gate
	}
	return ch, release
}

func waitReached(t *testing.T, reached <-chan string, done <-chan error) {
	t.Helper()
	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("returned before reaching the pre-lock hook: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("never reached the pre-lock hook")
	}
}

func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("Work did not finish")
		return nil
	}
}

func jobWithID(id, recordingID int64) *river.Job[EncodeJobArgs] {
	return &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{ID: id, Attempt: 1, MaxAttempts: 25},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
	}
}

func inScratch(jobID string) func(string) bool {
	return func(scratchOut string) bool {
		return strings.Contains(filepath.ToSlash(scratchOut), "/encode/"+jobID+"/")
	}
}

func newH264Worker(pool *pgxpool.Pool, mediaDir, scratchDir, ffmpegPath string) *EncodeWorker {
	return &EncodeWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: scratchDir,
		FFmpeg:     ffmpegPath,
		Profiles: config.EncodeConfig{
			FFmpeg: ffmpegPath,
			Profiles: []config.EncodeProfile{{
				Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
			}},
		},
	}
}

func assertNoEncodeTemp(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), mediapath.EncodeTempFilePrefix) {
			t.Errorf("staged temp file was left behind: %s", e.Name())
		}
	}
}

// T1: stage はコピー完了まで canonical に触らない。lock 取得前の観測点で、事前に
// 置いた canonical（active 行なし）が変わっていない。canonical を直接 O_TRUNC で
// 開く実装では、この時点で中身が切り詰められている。
func TestEncodeWorker_StagingDoesNotTouchCanonical(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ffmpegPath := installCountingFFmpeg(t)
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "20240101/t1.m2ts", []string{"h264"}, []byte("original payload t1"))

	finalPath := filepath.Join(mediaDir, "20240101", "t1_h264.mp4")
	previous := bytes.Repeat([]byte("PREVIOUS-CONTENT"), 1024)
	if err := os.WriteFile(finalPath, previous, 0o644); err != nil {
		t.Fatal(err)
	}

	reached, release := pauseBeforeEncodeLock(t, func(string) bool { return true })
	w := newH264Worker(pool, mediaDir, scratchDir, ffmpegPath)
	done := make(chan error, 1)
	go func() { done <- w.Work(context.Background(), jobWithID(1, recordingID)) }()

	waitReached(t, reached, done)
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, previous) {
		t.Errorf("canonical before the lock = %d bytes, want the previous %d bytes untouched", len(got), len(previous))
	}
	release()
	if err := waitDone(t, done); err != nil {
		t.Fatalf("Work: %v", err)
	}
	got, _ = os.ReadFile(finalPath)
	if !bytes.HasPrefix(got, []byte("original payload t1attempt-1:")) {
		t.Errorf("published canonical = %q, want the encoded content", got)
	}
	assertNoEncodeTemp(t, filepath.Dir(finalPath))
}

// T2: A が commit した後に、古い観測（行は deleted で rel_path は target と同じ）の
// B が公開へ進み、commit に失敗する順序。判定 (b) が無いと B が A のファイルを
// rename で上書きし、canonical は B の中身、行は A の size_bytes になる。
func TestEncodeWorker_CommittedByAnotherAttemptIsNotOverwritten(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	ffmpegPath := installCountingFFmpeg(t)
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "20240101/t2.m2ts", []string{"h264"}, []byte("original payload t2"))

	// 削除済みの行（rel_path = target）から始める。行なしだと B は (a) で止まる。
	encRel := "20240101/t2_h264.mp4"
	seedEncodedAsset(t, pool, recordingID, "h264", encRel)
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET state = 'deleted', deleted_at = now()
		WHERE recording_id = $1 AND kind = 'encoded'`, recordingID); err != nil {
		t.Fatalf("marking the seeded row deleted: %v", err)
	}
	finalPath := filepath.Join(mediaDir, filepath.FromSlash(encRel))

	reached, release := pauseBeforeEncodeLock(t, inScratch("2"))
	origCommit := beforeEncodeCommit
	t.Cleanup(func() { beforeEncodeCommit = origCommit })

	w := newH264Worker(pool, mediaDir, scratchDir, ffmpegPath)
	doneB := make(chan error, 1)
	go func() { doneB <- w.Work(ctx, jobWithID(2, recordingID)) }()
	waitReached(t, reached, doneB)

	if err := w.Work(ctx, jobWithID(1, recordingID)); err != nil {
		t.Fatalf("Work A: %v", err)
	}
	contentA, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(contentA, []byte("attempt-2:")) {
		t.Fatalf("A's canonical = %q, want attempt-2 content", contentA)
	}

	beforeEncodeCommit = func(scratchOut string) error {
		if inScratch("2")(scratchOut) {
			return context.DeadlineExceeded
		}
		return nil
	}
	release()
	if err := waitDone(t, doneB); err != nil {
		t.Errorf("Work B: %v (want skip because A already committed)", err)
	}

	got, _ := os.ReadFile(finalPath)
	if !bytes.Equal(got, contentA) {
		t.Errorf("canonical = %d bytes, want A's %d bytes", len(got), len(contentA))
	}
	var size int64
	if err := pool.QueryRow(ctx, `SELECT size_bytes FROM media_assets
		WHERE recording_id = $1 AND kind = 'encoded' AND profile = 'h264' AND state = 'active'`, recordingID).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if size != int64(len(contentA)) {
		t.Errorf("size_bytes = %d, want A's %d", size, len(contentA))
	}
	assertNoEncodeTemp(t, filepath.Dir(finalPath))
}

// T3: cut。古い計画（g1 → g2）の X が止まっている間に、行が active のまま g3 へ進み、
// 区間も X と違う。X は公開せず「計画が古い」で戻る（Work では snooze になる。
// 成功で skip すると X の新しい keep が消える）。(a) が無いと、区間が違うので (b) では
// 止まらず、X が行を g2 へ巻き戻す。
func TestEncodeWorker_StaleCutPlanDoesNotRewindRow(t *testing.T) {
	testStaleCutPlan(t, "active")
}

// T3b: 同じ順序でも、行が active でない（ごみ箱など）なら (a) は成功で飛ばす。
func TestEncodeWorker_StaleCutPlanOnInactiveRowSkipsWithSuccess(t *testing.T) {
	testStaleCutPlan(t, "deleted")
}

func testStaleCutPlan(t *testing.T, rowState string) {
	t.Helper()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "20240101/t3.m2ts", []string{"h264"}, []byte("original payload t3"))

	g1, g2, g3 := "20240101/t3_h264.g1.mp4", "20240101/t3_h264.g2.mp4", "20240101/t3_h264.g3.mp4"
	abs := func(rel string) string { return filepath.Join(mediaDir, filepath.FromSlash(rel)) }
	assetID := seedEncodedAsset(t, pool, recordingID, "h264", g1)
	if err := setFrozenCuts(t, pool, assetID, []chapters.Range{{StartMs: 0, EndMs: 2000}}); err != nil {
		t.Fatal(err)
	}

	cut := &cutContext{keep: []chapters.Range{{StartMs: 0, EndMs: 1000}}}
	observed, err := planEncodePublish(ctx, sqlcgen.New(pool), recordingID, "h264", cut, nil)
	if err != nil {
		t.Fatal(err)
	}
	if observed.skip || observed.generation != 2 || observed.replaced != g1 {
		t.Fatalf("plan = %+v, want generation 2 replacing %s and no skip", observed, g1)
	}

	scratchOut := filepath.Join(scratchDir, "out.mp4")
	if err := os.WriteFile(scratchOut, []byte("X output"), 0o644); err != nil {
		t.Fatal(err)
	}
	reached, release := pauseBeforeEncodeLock(t, func(string) bool { return true })
	w := newH264Worker(pool, mediaDir, scratchDir, "")
	type result struct {
		published bool
		err       error
	}
	res := make(chan result, 1)
	never := make(chan error)
	go func() {
		_, published, err := w.publishEncoded(ctx, encodePublishInput{
			recordingID: recordingID, profile: "h264", relPath: g2, finalPath: abs(g2),
			scratchOut: scratchOut, observed: observed, cut: cut,
		})
		res <- result{published, err}
	}()
	waitReached(t, reached, never)

	// 別の実行が g3 へ進めた状態（区間も X と違う）。
	if err := os.WriteFile(abs(g3), []byte("g3 output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET rel_path = $2 WHERE id = $1`, assetID, g3); err != nil {
		t.Fatal(err)
	}
	if err := setFrozenCuts(t, pool, assetID, []chapters.Range{{StartMs: 0, EndMs: 500}}); err != nil {
		t.Fatal(err)
	}
	if rowState != "active" {
		if _, err := pool.Exec(ctx, `UPDATE media_assets SET state = $2, deleted_at = now() WHERE id = $1`, assetID, rowState); err != nil {
			t.Fatal(err)
		}
	}

	release()
	r := <-res
	if r.published {
		t.Fatalf("publishEncoded published over a newer row")
	}
	if rowState == "active" {
		var snooze *rivertype.JobSnoozeError
		if !errors.As(r.err, &snooze) {
			t.Fatalf("publishEncoded err = %v, want *rivertype.JobSnoozeError (replan via snooze, not success)", r.err)
		}
	} else if r.err != nil {
		t.Fatalf("publishEncoded err = %v, want a successful skip", r.err)
	}
	var rel string
	if err := pool.QueryRow(ctx, `SELECT rel_path FROM media_assets WHERE id = $1`, assetID).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	if rel != g3 {
		t.Errorf("row rel_path = %s, want %s (the row must not be rewound)", rel, g3)
	}
	if _, err := os.Stat(abs(g3)); err != nil {
		t.Errorf("g3 file is missing: %v", err)
	}
	if _, err := os.Stat(abs(g2)); !os.IsNotExist(err) {
		t.Errorf("g2 file exists (err=%v), want it never published", err)
	}
	assertNoEncodeTemp(t, filepath.Dir(abs(g2)))
}

// snooze（計画のやり直し）は失敗ではないので、encode.failed も failed 行も出さない。
func TestShouldNotifyEncodeFailure_SnoozeIsNotAFailure(t *testing.T) {
	if shouldNotifyEncodeFailure(fmt.Errorf("wrapped: %w", river.JobSnooze(time.Second)), nil) {
		t.Error("a snooze must not notify encode.failed")
	}
	if !shouldNotifyEncodeFailure(errors.New("boom"), nil) {
		t.Error("a real failure must notify encode.failed")
	}
}

// T4: scratch はジョブ ID ごと。別ジョブ A が終わった後も、B の scratch が残る。
// 固定パスだと A の開始時・終了時の RemoveAll が B の出力を消す。
func TestEncodeWorker_ScratchIsPerJob(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	ffmpegPath := installCountingFFmpeg(t)
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "20240101/t4.m2ts", []string{"h264"}, []byte("original payload t4"))

	var first atomic.Bool
	reached, release := pauseBeforeEncodeLock(t, func(string) bool { return first.CompareAndSwap(false, true) })
	w := newH264Worker(pool, mediaDir, scratchDir, ffmpegPath)
	doneB := make(chan error, 1)
	go func() { doneB <- w.Work(ctx, jobWithID(2, recordingID)) }()
	waitReached(t, reached, doneB)

	if err := w.Work(ctx, jobWithID(1, recordingID)); err != nil {
		t.Fatalf("Work A: %v", err)
	}
	if _, err := os.Stat(filepath.Join(scratchDir, "encode", "2", "out.mp4")); err != nil {
		t.Errorf("B's scratch output is gone after A finished: %v", err)
	}
	release()
	if err := waitDone(t, doneB); err != nil {
		t.Fatalf("Work B: %v", err)
	}
	if _, err := os.Stat(filepath.Join(scratchDir, "encode", "2")); !os.IsNotExist(err) {
		t.Errorf("B's scratch was not cleaned up (err=%v)", err)
	}
}

// commit の直前まで rel_path filesystem lock を保持する（孤児回収は同じ lock を
// 非 blocking で取ってから unlink する）。
func TestEncodeWorker_PublishHoldsRelPathFileLockThroughCommit(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "20240101/lock.m2ts", []string{"h264"}, []byte("seed"))
	encRel := "20240101/lock_h264.mp4"
	finalPath := filepath.Join(mediaDir, filepath.FromSlash(encRel))
	scratchOut := filepath.Join(scratchDir, "out.mp4")
	if err := os.WriteFile(scratchOut, []byte("encoded bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	var acquiredDuringCommit bool
	orig := beforeEncodeCommit
	t.Cleanup(func() { beforeEncodeCommit = orig })
	beforeEncodeCommit = func(string) error {
		l, ok, err := tryLockMediaRelPathFile(finalPath, encRel)
		if err != nil {
			return err
		}
		if ok {
			acquiredDuringCommit = true
			_ = l.Close()
		}
		return nil
	}
	w := newH264Worker(pool, mediaDir, scratchDir, "")
	if _, published, err := w.publishEncoded(context.Background(), encodePublishInput{
		recordingID: recordingID, profile: "h264", relPath: encRel, finalPath: finalPath, scratchOut: scratchOut,
	}); err != nil || !published {
		t.Fatalf("publishEncoded = published %v, err %v", published, err)
	}
	if acquiredDuringCommit {
		t.Fatal("rel_path file lock was free right before the commit")
	}
}

// lock 待ちで ctx が切れたら、stage 済みの temp は消える。
func TestEncodeWorker_PublishCancelledWhileWaitingForLockRemovesTemp(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "20240101/cancel.m2ts", []string{"h264"}, []byte("seed"))
	encRel := "20240101/cancel_h264.mp4"
	finalPath := filepath.Join(mediaDir, filepath.FromSlash(encRel))
	scratchOut := filepath.Join(scratchDir, "out.mp4")
	if err := os.WriteFile(scratchOut, []byte("encoded bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	held, ok, err := tryLockMediaRelPathFile(finalPath, encRel)
	if err != nil || !ok {
		t.Fatalf("holding the lock: ok=%v err=%v", ok, err)
	}
	defer func() { _ = held.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	w := newH264Worker(pool, mediaDir, scratchDir, "")
	if _, _, err := w.publishEncoded(ctx, encodePublishInput{
		recordingID: recordingID, profile: "h264", relPath: encRel, finalPath: finalPath, scratchOut: scratchOut,
	}); err == nil {
		t.Fatal("publishEncoded succeeded while the lock was held")
	}
	assertNoEncodeTemp(t, filepath.Dir(finalPath))
}
