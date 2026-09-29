package worker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/mediapath"
)

// newPublishTestWorker は encode の公開プロトコルだけを試すための worker を組む。
// ffmpeg は呼ばない（scratch 出力はテストが直接置く）。
func newPublishTestWorker(pool *pgxpool.Pool, mediaDir, scratchDir string) *EncodeWorker {
	return &EncodeWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: scratchDir,
		Profiles: config.EncodeConfig{
			Profiles: []config.EncodeProfile{{Name: "h264", Container: "mp4"}},
		},
	}
}

// TestEncodeWorker_PublishDoesNotTouchCanonicalBeforeRename は、公開が
// 「同じディレクトリの temp へ stage → rename」で行われ、rename の直前まで
// canonical が一切触られないことを固定する。
//
// 直接 `O_TRUNC` で canonical を開くと、この観測点で前の公開内容が既に消えている
// （切り詰められた状態か、コピー途中の内容が見える）。
func TestEncodeWorker_PublishDoesNotTouchCanonicalBeforeRename(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ffmpegPath := installFakeFFmpeg(t)

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	rel := "20240101/publish.m2ts"
	content := []byte("original payload for the atomic publish test")
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, rel, []string{"h264"}, content)

	// 前の公開の残骸（DB 行を持たないファイル）。この上へ置き直すときも、rename の
	// 直前までは中身が変わってはならない。
	encRel := "20240101/publish_h264.mp4"
	finalPath := filepath.Join(mediaDir, filepath.FromSlash(encRel))
	previous := bytes.Repeat([]byte("PREVIOUS-CONTENT"), 1024)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalPath, previous, 0o644); err != nil {
		t.Fatal(err)
	}

	renameReached := make(chan struct{})
	releaseRename := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRename) }) }
	observed := make(chan []byte, 1)
	originalRename := renameEncodedFile
	t.Cleanup(func() {
		release()
		renameEncodedFile = originalRename
	})
	renameEncodedFile = func(src, dst string) error {
		// rename の直前の canonical。まだ前の内容のままのはず。
		got, err := os.ReadFile(dst)
		if err != nil {
			observed <- []byte(fmt.Sprintf("<read error: %v>", err))
		} else {
			observed <- got
		}
		close(renameReached)
		<-releaseRename
		return originalRename(src, dst)
	}

	w := &EncodeWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: scratchDir,
		FFmpeg:     ffmpegPath,
		Profiles: config.EncodeConfig{
			FFmpeg: ffmpegPath,
			Profiles: []config.EncodeProfile{{
				Name:       "h264",
				Container:  "mp4",
				VideoCodec: "libx264",
				AudioCodec: "aac",
			}},
		},
	}
	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
	}

	done := make(chan error, 1)
	go func() { done <- w.Work(context.Background(), job) }()

	select {
	case <-renameReached:
	case err := <-done:
		t.Fatalf("encode returned before publishing: %v (canonical was overwritten in place?)", err)
	case <-time.After(30 * time.Second):
		t.Fatal("encode never reached the publish rename (canonical was overwritten in place?)")
	}

	before := <-observed
	if !bytes.Equal(before, previous) {
		t.Errorf("canonical before the rename = %d bytes, want the previous %d bytes untouched",
			len(before), len(previous))
	}

	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Work() error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Work() did not finish after the rename gate was released")
	}

	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("reading published canonical: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("published canonical = %q, want the encoded content", got)
	}
	// stage した一時ファイルを置き忘れないこと。
	entries, err := os.ReadDir(filepath.Dir(finalPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), mediapath.EncodeTempFilePrefix) {
			t.Errorf("staged temp file was left behind: %s", e.Name())
		}
	}
}

// TestEncodeWorker_ConcurrentPublishNeverShowsPartialCanonical は同じ canonical を
// 2 本の公開が奪い合っても、読者が観測するのは常にどちらかの完全な内容であることを
// 固定する。`O_TRUNC` で canonical を直接開く実装では、コピー中の中途半端な内容が
// この読み取りで観測される（payload を MB 単位にして窓を広げている）。
func TestEncodeWorker_ConcurrentPublishNeverShowsPartialCanonical(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	rel := "20240101/race.m2ts"
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, rel, []string{"h264"}, []byte("seed"))

	w := newPublishTestWorker(pool, mediaDir, scratchDir)
	encRel := "20240101/race_h264.mp4"
	finalPath := filepath.Join(mediaDir, filepath.FromSlash(encRel))

	payloadA := bytes.Repeat([]byte("A"), 4<<20)
	payloadB := bytes.Repeat([]byte("B"), 2<<20)

	publish := func(payload []byte, scratchName string) error {
		scratchOut := filepath.Join(scratchDir, scratchName)
		if err := os.WriteFile(scratchOut, payload, 0o644); err != nil {
			return err
		}
		_, err := w.publishEncoded(context.Background(), encodePublishInput{
			recordingID: recordingID,
			profile:     "h264",
			relPath:     encRel,
			finalPath:   finalPath,
			scratchOut:  scratchOut,
		})
		return err
	}

	stop := make(chan struct{})
	partial := make(chan string, 1)
	var reads int
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := os.ReadFile(finalPath)
			if err != nil {
				continue // まだ公開されていない
			}
			reads++
			if !bytes.Equal(got, payloadA) && !bytes.Equal(got, payloadB) {
				select {
				case partial <- fmt.Sprintf("%d bytes (neither payload: A=%d, B=%d)",
					len(got), len(payloadA), len(payloadB)):
				default:
				}
				return
			}
		}
	}()

	errA := make(chan error, 1)
	errB := make(chan error, 1)
	go func() { errA <- publish(payloadA, "a.out.mp4") }()
	go func() { errB <- publish(payloadB, "b.out.mp4") }()
	if err := <-errA; err != nil {
		t.Errorf("publish A: %v", err)
	}
	if err := <-errB; err != nil {
		t.Errorf("publish B: %v", err)
	}
	close(stop)
	readerWG.Wait()

	select {
	case bad := <-partial:
		t.Fatalf("reader observed a partially written canonical: %s", bad)
	default:
	}
	if reads == 0 {
		t.Fatal("reader never observed the canonical; the assertion above proved nothing")
	}

	// 最後に commit した行が、最後に置いたファイルを指していること。
	info, err := os.Stat(finalPath)
	if err != nil {
		t.Fatalf("statting canonical: %v", err)
	}
	var sizeBytes int64
	if err := pool.QueryRow(context.Background(),
		`SELECT size_bytes FROM media_assets
		 WHERE recording_id = $1 AND kind = 'encoded' AND profile = 'h264' AND state = 'active'`,
		recordingID,
	).Scan(&sizeBytes); err != nil {
		t.Fatalf("reading media_assets row: %v", err)
	}
	if sizeBytes != info.Size() {
		t.Errorf("media_assets.size_bytes = %d, but the canonical is %d bytes", sizeBytes, info.Size())
	}
}

// TestEncodeWorker_PublishHoldsRelPathFileLockThroughCommit は、公開（rename）から
// DB commit までのあいだ rel_path filesystem lock を保持することを固定する。
// 孤児回収は同じ lock を非 blocking で取ってから canonical を unlink するので、
// 公開と commit の間で lock を離すと、commit 前の行と消えた実体が組み合わせになりうる。
func TestEncodeWorker_PublishHoldsRelPathFileLockThroughCommit(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	rel := "20240101/lock.m2ts"
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, rel, []string{"h264"}, []byte("seed"))
	w := newPublishTestWorker(pool, mediaDir, scratchDir)
	encRel := "20240101/lock_h264.mp4"
	finalPath := filepath.Join(mediaDir, filepath.FromSlash(encRel))
	scratchOut := filepath.Join(scratchDir, "out.mp4")
	if err := os.WriteFile(scratchOut, []byte("encoded bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	renameReached := make(chan struct{})
	releaseRename := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRename) }) }
	originalRename := renameEncodedFile
	t.Cleanup(func() {
		release()
		renameEncodedFile = originalRename
	})
	renameEncodedFile = func(src, dst string) error {
		close(renameReached)
		<-releaseRename
		return originalRename(src, dst)
	}

	published := make(chan error, 1)
	go func() {
		_, err := w.publishEncoded(context.Background(), encodePublishInput{
			recordingID: recordingID,
			profile:     "h264",
			relPath:     encRel,
			finalPath:   finalPath,
			scratchOut:  scratchOut,
		})
		published <- err
	}()

	select {
	case <-renameReached:
	case err := <-published:
		release()
		t.Fatalf("publish returned before the rename: %v", err)
	case <-time.After(30 * time.Second):
		release()
		t.Fatal("publish never reached the rename hook")
	}
	fileLock, acquired, err := tryLockMediaRelPathFile(finalPath, encRel)
	if err != nil {
		release()
		<-published
		t.Fatalf("trying the rel_path file lock during publish: %v", err)
	}
	if acquired {
		_ = fileLock.Close()
		release()
		<-published
		t.Fatal("rel_path file lock was available while publish was renaming into the canonical")
	}
	release()
	if err := <-published; err != nil {
		t.Fatalf("publishEncoded() error: %v", err)
	}
}

// TestEncodeWorker_ScratchIsExclusivePerRecordingProfile は、同じ (recording, profile)
// の別実行が scratch を掴んでいる間、この実行が待たずに戻り、相手の途中出力を
// 消さないことを固定する。固定パスの scratch を排他無しで使うと、先に終わった側の
// RemoveAll が相手の出力を消す。
func TestEncodeWorker_ScratchIsExclusivePerRecordingProfile(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ffmpegPath := installFakeFFmpeg(t)

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	rel := "20240101/exclusive.m2ts"
	content := []byte("original payload for the scratch exclusion test")
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, rel, []string{"h264"}, content)

	profile := config.EncodeProfile{Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"}
	w := &EncodeWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: scratchDir,
		FFmpeg:     ffmpegPath,
		Profiles:   config.EncodeConfig{FFmpeg: ffmpegPath, Profiles: []config.EncodeProfile{profile}},
	}
	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
	}

	scratchPath, _, err := w.scratchPaths(recordingID, profile)
	if err != nil {
		t.Fatal(err)
	}
	// 別の実行を模す: 実際に使う lock を取り、scratch に途中出力を置く。
	// 別の fd なので同じプロセスでも flock は衝突する。
	holder, acquired, err := lockEncodeScratch(scratchPath)
	if err != nil {
		t.Fatalf("locking scratch for the simulated other attempt: %v", err)
	}
	if !acquired {
		t.Fatal("scratch lock was already held")
	}
	defer func() { _ = holder.Close() }()
	if err := os.MkdirAll(scratchPath, 0o755); err != nil {
		t.Fatal(err)
	}
	inFlight := filepath.Join(scratchPath, "out.mp4")
	if err := os.WriteFile(inFlight, []byte("in-flight output of the other attempt"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := w.Work(context.Background(), job); err == nil {
		t.Fatal("encode ran while another attempt held the scratch")
	} else if !strings.Contains(err.Error(), "in use by another attempt") {
		t.Errorf("error = %v, want the scratch-in-use deferral", err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Errorf("the other attempt's in-flight output was removed: %v", err)
	}

	// lock を離せば同じジョブが通る（延期が恒久的な停止になっていない）。
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() after the other attempt released the scratch: %v", err)
	}
	encRel := "20240101/exclusive_h264.mp4"
	got, err := os.ReadFile(filepath.Join(mediaDir, filepath.FromSlash(encRel)))
	if err != nil {
		t.Fatalf("reading published canonical: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("published canonical = %q, want the encoded content", got)
	}
}
