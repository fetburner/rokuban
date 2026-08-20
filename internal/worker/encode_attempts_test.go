package worker

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
)

// encodeAttemptState は recording_encode_attempts から 1 行だけ読む
// テスト用ヘルパー。行が無ければ ok=false。
func encodeAttemptState(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile string) (state string, ok bool) {
	t.Helper()
	row := pool.QueryRow(context.Background(),
		`SELECT state FROM recording_encode_attempts WHERE recording_id = $1 AND profile = $2`,
		recordingID, profile)
	if err := row.Scan(&state); err != nil {
		return "", false
	}
	return state, true
}

// TestEncodeWorker_AttemptRow_ClearedOnSuccess は成功時に
// recording_encode_attempts の行が残らないことを固定する（issue #316）。
// running を書いてから成功で消す、という 1 サイクルを両方向で見る:
// 「行が一時的にできる」側は TestEncodeWorker_AttemptRow_FailedOnFailure が、
// 「最終的に消える」側はここが見る。
func TestEncodeWorker_AttemptRow_ClearedOnSuccess(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ffmpegPath := installFakeFFmpeg(t)

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	rel := "20240101/attempt-success.m2ts"
	content := []byte("fake-ts-payload-for-attempt-success-test")
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, rel, []string{"h264"}, content)

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
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work(): %v", err)
	}

	if _, ok := encodeAttemptState(t, pool, recordingID, "h264"); ok {
		t.Errorf("recording_encode_attempts row remains after success; want cleared")
	}
}

// TestEncodeWorker_AttemptRow_FailedOnFailure は失敗時に
// recording_encode_attempts が state='failed' になり、error にメッセージが
// 入ることを固定する。
func TestEncodeWorker_AttemptRow_FailedOnFailure(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/attempt-fail.m2ts", nil, []byte("data"))

	w := &EncodeWorker{
		Pool:     pool,
		MediaDir: mediaDir,
		Profiles: config.EncodeConfig{}, // "missing" は未定義 → 失敗する
	}

	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "missing"},
	}
	if err := w.Work(context.Background(), job); err == nil {
		t.Fatal("expected error for unknown profile")
	}

	state, ok := encodeAttemptState(t, pool, recordingID, "missing")
	if !ok {
		t.Fatalf("recording_encode_attempts row missing after failure; want state=failed")
	}
	if state != "failed" {
		t.Errorf("state = %q, want failed", state)
	}

	var errMsg *string
	if err := pool.QueryRow(context.Background(),
		`SELECT error FROM recording_encode_attempts WHERE recording_id = $1 AND profile = $2`,
		recordingID, "missing",
	).Scan(&errMsg); err != nil {
		t.Fatalf("reading error column: %v", err)
	}
	if errMsg == nil || *errMsg == "" {
		t.Errorf("error = %v, want a non-empty message", errMsg)
	}
}

// installSlowFakeFFmpeg は起動後に指定秒数寝続ける（実行し続ける）だけの
// フェイク ffmpeg。TestEncodeWorker_AttemptRow_CtxCanceledLeavesRunning が
// 「running 行を書いた後、ffmpeg 実行中に ctx がキャンセルされる」を再現する
// ために使う --- installFakeFFmpeg（即座に完了する）では、ctx を事前に
// キャンセルすると markEncodeAttemptRunning 自身が ctx に紐付いた DB 書き込みで
// 失敗し、行が一度も書かれない（このテストが検証したい状態に到達できない）。
func installSlowFakeFFmpeg(t *testing.T, sleepSeconds int) (ffmpegPath string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\nsleep " + strconv.Itoa(sleepSeconds) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEncodeWorker_AttemptRow_CtxCanceledLeavesRunning は ctx キャンセル
// （River の停止・タイムアウト）ではジョブの失敗として扱わないので、行が
// failed に上書きされず running のまま残ることを固定する
// （shouldNotifyEncodeFailure と同じ判定を試行状態の観測にも揃える。issue #316）。
//
// ctx は ffmpeg 実行中（running 行を書いた後）にキャンセルする ---
// 事前キャンセルだと markEncodeAttemptRunning 自身の DB 書き込みが ctx に
// 紐付いて失敗し、行が一度も書かれないため検証にならない（上記
// installSlowFakeFFmpeg のコメント参照）。
func TestEncodeWorker_AttemptRow_CtxCanceledLeavesRunning(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/attempt-cancel.m2ts", nil, []byte("data"))
	slowFFmpeg := installSlowFakeFFmpeg(t, 5)

	w := &EncodeWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: t.TempDir(),
		FFmpeg:     slowFFmpeg,
		Profiles: config.EncodeConfig{
			FFmpeg: slowFFmpeg,
			Profiles: []config.EncodeProfile{{
				Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
			}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
	}

	workErr := make(chan error, 1)
	go func() { workErr <- w.Work(ctx, job) }()

	// running 行が書かれる（cmd.Start より前）のを待ってからキャンセルする。
	deadline := time.Now().Add(5 * time.Second)
	for {
		if state, ok := encodeAttemptState(t, pool, recordingID, "h264"); ok {
			if state != "running" {
				t.Fatalf("state before cancel = %q, want running", state)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for running attempt row to appear")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-workErr:
		if err == nil {
			t.Fatal("expected error with canceled ctx")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Work() to return after cancel")
	}

	state, ok := encodeAttemptState(t, pool, recordingID, "h264")
	if !ok {
		t.Fatalf("recording_encode_attempts row missing after ctx cancel; want state=running (left as-is)")
	}
	if state != "running" {
		t.Errorf("state = %q, want running (ctx cancel must not mark failed)", state)
	}
}
