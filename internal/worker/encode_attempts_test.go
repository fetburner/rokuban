package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// waitFor は cond が true を返すまで最大 timeout 待つ（10ms 間隔でポーリング）。
// 真にならないまま timeout に達したら buildTimeoutMsg() で Fatal する
// （site_queue_scoping_test.go の waitForNotAvailable と同じ形。ここでは
// 待つ条件が呼び出し側ごとに違うので cond を引数に取る）。cond は t.Fatal を
// 呼んでよい（「時間切れ」とは別の失敗を即座に報告したいケース向け）。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, buildTimeoutMsg func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(buildTimeoutMsg())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

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

func encodeAttemptCount(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile string) (int32, bool) {
	t.Helper()
	var count int32
	err := pool.QueryRow(context.Background(),
		`SELECT attempt_count FROM recording_encode_attempts WHERE recording_id = $1 AND profile = $2`,
		recordingID, profile).Scan(&count)
	if err != nil {
		return 0, false
	}
	return count, true
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
// フェイク ffmpeg。TestEncodeWorker_AttemptRow_CtxCanceledRollsBack が
// 「running 行を書いた後、ffmpeg 実行中に ctx がキャンセルされる」を再現する
// ために使う --- installFakeFFmpeg（即座に完了する）では、ctx を事前に
// キャンセルすると beginEncodeAttempt 自身が ctx に紐付いた DB 書き込みで
// 失敗し、行が一度も書かれない（このテストが検証したい状態に到達できない）。
//
// startedMarker は sleep の起動後（PID echo の後）に作られる空ファイルのパス。
// 呼び出し側はこれの出現を cmd.Start() が実際に成功した証拠として待つ
// （下のテストのコメント参照）。
// childPIDMarker は、テスト終了時に孤児化した sleep を回収するための PID ファイル。
func installSlowFakeFFmpeg(t *testing.T, sleepSeconds int) (ffmpegPath, startedMarker, childPIDMarker string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffmpeg")
	startedMarker = filepath.Join(dir, "started")
	childPIDMarker = filepath.Join(dir, "child-pid")
	// バックグラウンドの sleep は親 shell が kill されても stderr の fd を
	// 継承したまま生き残る。これが WaitDelay の再現対象になる。
	script := "#!/bin/sh\n" +
		"sleep " + strconv.Itoa(sleepSeconds) + " &\n" +
		": > " + childPIDMarker + "\n" +
		"echo $! > " + childPIDMarker + "\n" +
		": > " + startedMarker + "\n" +
		"wait\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, startedMarker, childPIDMarker
}

// TestEncodeWorker_AttemptRow_CtxCanceledRollsBack は停止による ctx キャンセルで
// domain count が進まず、running 行も残らないことを固定する。
//
// ctx は ffmpeg 実行中（running 行を書いた後）にキャンセルする ---
// 事前キャンセルだと beginEncodeAttempt 自身の DB 書き込みが ctx に
// 紐付いて失敗し、行が一度も書かれないため検証にならない（上記
// installSlowFakeFFmpeg のコメント参照）。
//
// running 行の出現は cmd.Start() より**前**の出来事でしかない --- 行の
// commit と cmd.Start() の間には profile 解決・loadOriginal（ctx 付き DB
// クエリ）・probeEncodeDuration 等、複数の ctx 依存ステップが挟まる
// （encode.go の runEncode 参照）。行が見えた直後に cancel すると、この
// 間のどこかで ctx が先に死に、cmd.Start() 自身が「開始せず ctx.Err() を
// 返す」（os/exec の Cmd.Start は開始前に ctx.Done() をチェックする）。
// この場合 kill → reap → cmd.Wait() → <-progressDone の尾（このテストが
// 検証したい経路）を一度も通らない。
//
// 計測（このマシン、ffprobe が実 PATH 上にある状態）: running 行の出現
// だけを合図に cancel すると、cmd.Wait() 直前に置いたマーカーへの到達は
// 0/10（probeEncodeDuration が実 ffprobe を shell out し、その間に ctx が
// 死ぬ）。probeEncodeDuration を即失敗させても（FFprobe に存在しないパス
// を渡しても）このマシンでは 25/30（マシン依存 --- 別マシンでの計測では
// 29/30。cancel が loadOriginal 等の他ステップ中に着地する残りは依然として
// cmd.Start() 前に死ぬ。この割合自体を性質として当てにしない）。
//
// そこでフェイク ffmpeg 自身に「実際に起動した」印（startedMarker への
// touch。sleep の起動後に置く）を持たせ、その出現を待ってから cancel する。
// touch は子プロセスが exec された後にしか起きないので、その出現は
// 「fork/exec は完了した（os/exec の Cmd.Start が開始前にチェックする
// ctx.Done() は、開始しない分岐にはもう入れない）」ことの証拠になる ---
// ただし Start() 自身が呼び出し元へ戻っているとまでは保証しない（親が
// exec ステータス用パイプを読み切るのと子の最初の命令実行は別プロセスの
// 出来事で、順序は未計測）。FFprobe を存在しないパスにする変更は残す ---
// これが無いと実 ffprobe の呼び出し（timeout 上限 3 秒）がこの待ちを
// 不必要に長引かせる。
func TestEncodeWorker_AttemptRow_CtxCanceledRollsBack(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/attempt-cancel.m2ts", nil, []byte("data"))
	// workReturnTimeout は WaitDelay より十分大きく、WaitDelay を外した場合の
	// sleep 全長（slowFFmpegSleepSeconds）より短く保つ。WaitDelay が無いと
	// cancel() から Work() が返るまで sleep の全長を待つため、このテストが
	// タイムアウトする。
	// 原因: encode.go の cmd.Stderr は *os.File ではなく &strings.Builder な
	// ので、os/exec は stderr を読む中継 goroutine を立て、cmd.Wait はその
	// goroutine が EOF で終わるまで待つ（awaitGoroutines）。/bin/sh は子の
	// sleep が inherited な stderr の書き込み端を握ったままでも親だけ kill される。
	// CI のランナーは全て Linux（.github/workflows/ci.yml、/bin/sh は
	// dash）なので #552 は Linux 上で起きたフレーク --- macOS 固有の話ではない。
	// sleep は WaitDelay と十分離す。短すぎると WaitDelay が無くてもテストが
	// 通ってしまい、回帰を検出できない。両方を workerExecWaitDelay の同じ
	// スケールの倍数で導出する（3d > 2d は d をどう変えても成立する。加法の
	// 余裕（例: d + 3s）を混ぜると、d を十分小さくしたときに sleep <=
	// workReturnTimeout に潰れ、setWorkerExecWaitDelay を消しても検出できない
	// 無言の後退が起こり得る）。sleep は秒単位の整数しか取れないので、切り捨て
	// ではなく切り上げる --- 切り捨てると d が秒の整数倍でないとき（例: 1500ms
	// なら sleep 3s / workReturnTimeout 3s）に上の不等式が等号に潰れる。
	setShortWorkerExecWaitDelay(t)
	slowFFmpegSleepSeconds := int((workerExecWaitDelay*3 + time.Second - 1) / time.Second)
	workReturnTimeout := 2 * workerExecWaitDelay
	slowFFmpeg, ffmpegStarted, childPIDMarker := installSlowFakeFFmpeg(t, slowFFmpegSleepSeconds)
	sleepStartedAt := time.Now()
	defer func() {
		// sleep 自身の寿命（slowFFmpegSleepSeconds）を過ぎていたら、その PID は
		// とっくに sleep が自然終了して OS に再利用され得る。/bin/sh を殺した
		// 時点で sleep は reparent されテストからは生死を確認できないので、
		// ここで安価に「まだ生きている想定期間内か」だけ見て、超えていたら
		// 無関係なプロセスを殺さないよう kill をスキップする。
		if time.Since(sleepStartedAt) > time.Duration(slowFFmpegSleepSeconds)*time.Second {
			return
		}
		pidBytes, err := os.ReadFile(childPIDMarker)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
		if err != nil {
			return
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	}()

	w := &EncodeWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: t.TempDir(),
		FFmpeg:     slowFFmpeg,
		// 実 machine に ffprobe があると probeEncodeDuration が実 ffprobe を
		// shell out し、cmd.Start() 前の待ちを timeout 上限（3 秒）まで
		// 引き延ばしうる。存在しないパスにして即失敗させる（上のテスト
		// doc コメントの計測参照）。
		FFprobe: "/nonexistent/ffprobe",
		Profiles: config.EncodeConfig{
			FFmpeg: slowFFmpeg,
			Profiles: []config.EncodeProfile{{
				Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
			}},
		},
	}
	prior, err := w.beginEncodeAttempt(context.Background(), recordingID, "h264")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := w.markEncodeAttemptFailed(context.Background(), recordingID, "h264", prior.count, errors.New("previous failure"))
	if err != nil || !updated {
		t.Fatalf("setting previous attempt: updated=%v err=%v", updated, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
	}

	workErr := make(chan error, 1)
	go func() { workErr <- w.Work(ctx, job) }()

	// running 行が書かれる（cmd.Start より前）のを待つ。行自体はテストの
	// アサーション対象（cancel 前の状態）でもあるので残す。
	waitFor(t, 5*time.Second,
		func() bool {
			state, ok := encodeAttemptState(t, pool, recordingID, "h264")
			return ok && state == "running"
		},
		func() string { return "timed out waiting for the previous failed attempt to become running" },
	)
	// cmd.Start() が実際に成功した（フェイク ffmpeg が起動した）のを待って
	// から cancel する（上の doc コメント参照）。
	waitFor(t, 5*time.Second,
		func() bool {
			_, err := os.Stat(ffmpegStarted)
			return err == nil
		},
		func() string {
			// フェイク ffmpeg の script 自体が起動できない場合（noexec /
			// EACCES / /bin/sh 不在等）、原因は runEncode が返した
			// "starting ffmpeg: ..." で workErr に既に載っている。
			// 症状（マーカー未出現）だけでなく原因も一緒に出す。
			select {
			case err := <-workErr:
				return fmt.Sprintf("timed out waiting for fake ffmpeg to start; Work() already returned: %v", err)
			default:
				return "timed out waiting for fake ffmpeg to start"
			}
		},
	)
	cancelAt := time.Now()
	cancel()

	select {
	case err := <-workErr:
		if elapsed := time.Since(cancelAt); elapsed >= workReturnTimeout {
			t.Fatalf("Work() returned after %s; WaitDelay did not bound the stderr pipe wait", elapsed)
		}
		if err == nil {
			t.Fatal("expected error with canceled ctx")
		}
	case <-time.After(workReturnTimeout):
		t.Fatal("timed out waiting for Work() to return after cancel")
	}

	state, ok := encodeAttemptState(t, pool, recordingID, "h264")
	if !ok || state != "failed" {
		t.Fatalf("state after canceled retry = %q, ok=%v, want prior failed state", state, ok)
	}
	if count, ok := encodeAttemptCount(t, pool, recordingID, "h264"); !ok || count != 1 {
		t.Errorf("attempt_count after canceled retry = %d, ok=%v, want 1", count, ok)
	}
}

func TestEncodeWorker_AttemptLimitDoesNotReadRiverAttempt(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/domain-attempt-limit.m2ts", nil, []byte("data"))
	w := &EncodeWorker{Pool: pool, MediaDir: mediaDir}
	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 1},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "missing"},
	}
	for attempt := int32(1); attempt <= 25; attempt++ {
		err := w.Work(context.Background(), job)
		if attempt < 25 && err == nil {
			t.Fatalf("Work at domain attempt %d succeeded; want retryable failure", attempt)
		}
		if attempt == 25 && !isJobCancel(err) {
			t.Fatalf("Work at domain attempt 25 = %v, want river.JobCancel after recording failure", err)
		}
	}
	// 上限到達後の Work は runEncode の前に JobCancel を返す。
	if err := w.Work(context.Background(), job); !isJobCancel(err) {
		t.Fatalf("Work after the domain limit = %v, want river.JobCancel", err)
	}
	if state, ok := encodeAttemptState(t, pool, recordingID, "missing"); !ok || state != "failed" {
		t.Fatalf("state at domain limit = %q, ok=%v, want failed", state, ok)
	}
	if count, ok := encodeAttemptCount(t, pool, recordingID, "missing"); !ok || count != 25 {
		t.Errorf("attempt_count at domain limit = %d, ok=%v, want 25", count, ok)
	}
}

func isJobCancel(err error) bool {
	var cancel *rivertype.JobCancelError
	return errors.As(err, &cancel)
}

func TestEncodeWorker_AttemptLimitCountsRunningRowAsDead(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/dead-domain-attempts.m2ts", nil, []byte("data"))
	w := &EncodeWorker{Pool: pool}
	for attempt := int32(1); attempt <= 25; attempt++ {
		start, err := w.beginEncodeAttempt(context.Background(), recordingID, "h264")
		if err != nil {
			t.Fatal(err)
		}
		if start.count != attempt {
			t.Fatalf("started attempt count = %d, want %d", start.count, attempt)
		}
		if attempt == 1 && start.deadAttempt != 0 {
			t.Fatalf("first attempt marked dead attempt %d, want 0", start.deadAttempt)
		}
		if attempt > 1 && start.deadAttempt != attempt-1 {
			t.Fatalf("started attempt %d reported dead attempt %d, want %d", attempt, start.deadAttempt, attempt-1)
		}
	}
	terminal, err := w.beginEncodeAttempt(context.Background(), recordingID, "h264")
	if err != nil {
		t.Fatal(err)
	}
	if !terminal.terminal || terminal.count != 25 || terminal.deadAttempt != 25 {
		t.Fatalf("terminal start = %+v, want terminal count/dead attempt 25", terminal)
	}
	if state, ok := encodeAttemptState(t, pool, recordingID, "h264"); !ok || state != "failed" {
		t.Fatalf("state after 25 dead attempts = %q, ok=%v, want failed", state, ok)
	}
	if count, ok := encodeAttemptCount(t, pool, recordingID, "h264"); !ok || count != 25 {
		t.Errorf("attempt_count after 25 dead attempts = %d, ok=%v, want 25", count, ok)
	}
}

func TestEncodeJobTimeoutSnapshotsDurationAndRate(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/timeout-snapshot.m2ts", []string{"h264"}, []byte("data"))
	startedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	endedAt := startedAt.Add(90 * time.Minute)
	if _, err := pool.Exec(ctx, `UPDATE recordings SET started_at = $2, ended_at = $3 WHERE id = $1`, recordingID, startedAt, endedAt); err != nil {
		t.Fatal(err)
	}
	profiles := config.EncodeConfig{Profiles: []config.EncodeProfile{{Name: "h264", Rate: 1.5}}}
	args, err := newEncodeJobArgs(ctx, sqlcgen.New(pool), recordingID, "h264", profiles)
	if err != nil {
		t.Fatal(err)
	}
	if args.Timeout != 135*time.Minute {
		t.Fatalf("queued timeout = %s, want 2h15m (90m recording × 1.5)", args.Timeout)
	}
	worker := &EncodeWorker{}
	if got := worker.Timeout(&river.Job[EncodeJobArgs]{Args: args}); got != 135*time.Minute {
		t.Errorf("Timeout() = %s, want queued 2h15m", got)
	}
	shortEndedAt := startedAt.Add(5 * time.Minute)
	if _, err := pool.Exec(ctx, `UPDATE recordings SET ended_at = $2 WHERE id = $1`, recordingID, shortEndedAt); err != nil {
		t.Fatal(err)
	}
	shortProfiles := config.EncodeConfig{Profiles: []config.EncodeProfile{{Name: "h264", Rate: 4}}}
	shortArgs, err := newEncodeJobArgs(ctx, sqlcgen.New(pool), recordingID, "h264", shortProfiles)
	if err != nil {
		t.Fatal(err)
	}
	if shortArgs.Timeout != time.Hour {
		t.Fatalf("queued timeout for short recording = %s, want 1h floor (5m recording × rate 4 = 20m)", shortArgs.Timeout)
	}
	if got := worker.Timeout(&river.Job[EncodeJobArgs]{Args: shortArgs}); got != time.Hour {
		t.Errorf("Timeout() for short recording = %s, want queued 1h", got)
	}
	legacy := &river.Job[EncodeJobArgs]{Args: EncodeJobArgs{RecordingID: recordingID, Profile: "h264"}}
	if got := worker.Timeout(legacy); got != 12*time.Hour {
		t.Errorf("Timeout() for old args = %s, want 12h", got)
	}
	// started_at / ended_at が NULL（ライブラリ取り込み）なら番組長（既定 30 分）で計算する。
	if _, err := pool.Exec(ctx, `UPDATE recordings SET started_at = NULL, ended_at = NULL, program_duration_ms = 3 * 3600 * 1000 WHERE id = $1`, recordingID); err != nil {
		t.Fatal(err)
	}
	fromProgram, err := newEncodeJobArgs(ctx, sqlcgen.New(pool), recordingID, "h264", profiles)
	if err != nil {
		t.Fatal(err)
	}
	if fromProgram.Timeout != 270*time.Minute {
		t.Errorf("queued timeout from program_duration_ms = %s, want 4h30m (3h × 1.5)", fromProgram.Timeout)
	}
	// started_at == ended_at（rescue 再スキャン）でも番組長を使う。
	if _, err := pool.Exec(ctx, `UPDATE recordings SET started_at = $2, ended_at = $2 WHERE id = $1`, recordingID, startedAt); err != nil {
		t.Fatal(err)
	}
	zeroLen, err := newEncodeJobArgs(ctx, sqlcgen.New(pool), recordingID, "h264", profiles)
	if err != nil {
		t.Fatal(err)
	}
	if zeroLen.Timeout != 270*time.Minute {
		t.Errorf("queued timeout with started_at == ended_at = %s, want 4h30m", zeroLen.Timeout)
	}
	// 実尺も番組長も無ければ 12 時間。
	if _, err := pool.Exec(ctx, `UPDATE recordings SET started_at = NULL, ended_at = NULL, program_duration_ms = 0 WHERE id = $1`, recordingID); err != nil {
		t.Fatal(err)
	}
	unknown, err := newEncodeJobArgs(ctx, sqlcgen.New(pool), recordingID, "h264", profiles)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Timeout != 12*time.Hour {
		t.Errorf("queued timeout with unknown duration = %s, want 12h", unknown.Timeout)
	}
}

// 上限到達後の POST encode-profiles 相当（failed 行の削除）で、次の Work が count 1 から走る。
func TestEncodeWorker_ResetAfterLimitRestartsFromOne(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/reset-after-limit.m2ts", nil, []byte("data"))
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO recording_encode_attempts (recording_id, profile, state, error, attempted_at, attempt_count)
		 VALUES ($1, 'missing', 'failed', 'x', now(), 25)`, recordingID); err != nil {
		t.Fatal(err)
	}
	w := &EncodeWorker{Pool: pool, MediaDir: mediaDir}
	if _, err := sqlcgen.New(pool).DeleteFailedRecordingEncodeAttempts(context.Background(), sqlcgen.DeleteFailedRecordingEncodeAttemptsParams{
		RecordingID: recordingID, Profiles: []string{"missing"},
	}); err != nil {
		t.Fatal(err)
	}
	start, err := w.beginEncodeAttempt(context.Background(), recordingID, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if start.terminal || start.count != 1 {
		t.Fatalf("start after reset = %+v, want count 1 and not terminal", start)
	}
}

func TestEncodeWorker_DeadlineCountsFailureAndStopsFFmpeg(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/deadline.m2ts", []string{"h264"}, []byte("data"))
	installFakeFFprobe(t, "60.0", true)
	ffmpegDir := t.TempDir()
	ffmpegPath := filepath.Join(ffmpegDir, "ffmpeg")
	startedPath := filepath.Join(ffmpegDir, "started")
	pidPath := filepath.Join(ffmpegDir, "pid")
	script := "#!/bin/sh\necho $$ > " + pidPath + "\n: > " + startedPath + "\nexec sleep 60\n"
	if err := os.WriteFile(ffmpegPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	w := &EncodeWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: t.TempDir(),
		FFmpeg:     ffmpegPath,
		FFprobe:    "ffprobe",
		Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{{
			Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
		}}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 26},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
	}
	workDone := make(chan error, 1)
	go func() { workDone <- w.Work(ctx, job) }()
	waitFor(t, 6*time.Second, func() bool {
		_, err := os.Stat(startedPath)
		return err == nil
	}, func() string { return "fake ffmpeg did not start before the deadline" })
	select {
	case err := <-workDone:
		if err == nil {
			t.Fatal("Work succeeded after its deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Work did not return after the deadline canceled ffmpeg")
	}
	if state, ok := encodeAttemptState(t, pool, recordingID, "h264"); !ok || state != "failed" {
		t.Fatalf("state after deadline = %q, ok=%v, want failed", state, ok)
	}
	if count, ok := encodeAttemptCount(t, pool, recordingID, "h264"); !ok || count != 1 {
		t.Errorf("attempt_count after deadline = %d, ok=%v, want 1", count, ok)
	}
	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("fake ffmpeg PID %d still exists after Work returned", pid)
	}
}

// TestTruncateEncodeAttemptError_MultibyteBoundary は、バイト単位で切り詰めた
// 位置がマルチバイト文字の内側に落ちる入力でも、結果が常に有効な UTF-8 に
// なることを固定する（issue #316 のレビューで判明: 生のバイトスライスは
// Postgres が拒否する不正な UTF-8 を作り得た）。
//
// "日" は UTF-8 で 3 バイト。1000 回繰り返すと 3000 バイトで、
// encodeAttemptErrorMaxLen(2000) バイト目はちょうど文字の内側に落ちる
// （2000 = 3*666 + 2）。
func TestTruncateEncodeAttemptError_MultibyteBoundary(t *testing.T) {
	msg := strings.Repeat("日", 1000)
	if len(msg) <= encodeAttemptErrorMaxLen {
		t.Fatalf("test fixture too short: %d bytes, want > %d", len(msg), encodeAttemptErrorMaxLen)
	}

	got := truncateEncodeAttemptError(msg)

	if !utf8.ValidString(got) {
		t.Fatalf("truncateEncodeAttemptError(...) = %q, not valid UTF-8", got)
	}
	if len(got) == 0 {
		t.Error("truncateEncodeAttemptError(...) = \"\", want a non-empty truncated message")
	}
	if len(got) > encodeAttemptErrorMaxLen {
		t.Errorf("len(got) = %d, want <= %d", len(got), encodeAttemptErrorMaxLen)
	}
}

// TestTruncateEncodeAttemptError_ShortMessageUnchanged は上限未満の入力が
// そのまま返ることを固定する（truncateEncodeAttemptError_MultibyteBoundary の
// 逆方向 --- 短い入力まで削ってしまわないこと）。
func TestTruncateEncodeAttemptError_ShortMessageUnchanged(t *testing.T) {
	msg := "short ascii error"
	if got := truncateEncodeAttemptError(msg); got != msg {
		t.Errorf("truncateEncodeAttemptError(%q) = %q, want unchanged", msg, got)
	}
}

// TestEncodeWorker_AttemptRow_FailedOnFailure_MultibyteTruncation は、2000
// バイトを超える日本語（マルチバイト）エラーでも recording_encode_attempts に
// state='failed' の行が実際に書けることを固定する。バイト単位のスライスは
// 文字境界の内側で切れて不正な UTF-8 を作り、Postgres がその INSERT/UPDATE を
// 拒否するので、修正前はこの行が一度も書けず running のまま残った
// （実機で「失敗が失敗として見えなくなる」形の再現。issue #316 のレビューで
// 判明）。
func TestEncodeWorker_AttemptRow_FailedOnFailure_MultibyteTruncation(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/attempt-fail-multibyte.m2ts", nil, []byte("data"))

	w := &EncodeWorker{Pool: pool}
	longMsg := strings.Repeat("日", 1000)
	start, err := w.beginEncodeAttempt(context.Background(), recordingID, "h264")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := w.markEncodeAttemptFailed(context.Background(), recordingID, "h264", start.count, errors.New(longMsg))
	if err != nil || !updated {
		t.Fatalf("markEncodeAttemptFailed() = (%v, %v), want updated", updated, err)
	}

	state, ok := encodeAttemptState(t, pool, recordingID, "h264")
	if !ok {
		t.Fatalf("recording_encode_attempts row missing after failure with long multibyte error; want state=failed (byte-boundary truncation must not produce invalid UTF-8)")
	}
	if state != "failed" {
		t.Errorf("state = %q, want failed", state)
	}

	var errMsg *string
	if err := pool.QueryRow(context.Background(),
		`SELECT error FROM recording_encode_attempts WHERE recording_id = $1 AND profile = $2`,
		recordingID, "h264",
	).Scan(&errMsg); err != nil {
		t.Fatalf("reading error column: %v", err)
	}
	if errMsg == nil || *errMsg == "" {
		t.Fatalf("error = %v, want a non-empty truncated message", errMsg)
	}
	if !utf8.ValidString(*errMsg) {
		t.Errorf("error column is not valid UTF-8: %q", *errMsg)
	}
	if len(*errMsg) > encodeAttemptErrorMaxLen {
		t.Errorf("len(error) = %d, want <= %d", len(*errMsg), encodeAttemptErrorMaxLen)
	}
}

// TestEncodeWorker_ClearAttempt_SurvivesCanceledCtx は、成功時の DELETE が
// job の ctx のキャンセルに巻き込まれないことを固定する（issue #316 の
// レビューで判明）。
//
// 成功パスの clearEncodeAttempt は commitEncoded と webhook 通知（HTTP、
// タイムアウトまで待つ）の後の defer で走るので、その間に River が停止して
// ctx がキャンセルされ得る。job の ctx をそのまま渡していると DELETE が
// 失敗し、encoded 資産があるのに running を主張する行が恒久的に残る ---
// そのプロファイルは ListMissingEncodeProfiles の候補から外れるので、
// 冪等スキップ経路の掃除も二度と走らない（不変条件 10）。
// clearEncodeAttempt から attemptWriteContext を外すとこのテストが落ちる。
func TestEncodeWorker_ClearAttempt_SurvivesCanceledCtx(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/attempt-clear-canceled.m2ts", nil, []byte("data"))

	w := &EncodeWorker{Pool: pool}
	start, err := w.beginEncodeAttempt(context.Background(), recordingID, "h264")
	if err != nil {
		t.Fatal(err)
	}
	if state, ok := encodeAttemptState(t, pool, recordingID, "h264"); !ok || state != "running" {
		t.Fatalf("fixture: state = %q, ok = %v, want running", state, ok)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.clearEncodeAttempt(ctx, recordingID, "h264", start.count); err != nil {
		t.Fatal(err)
	}

	if state, ok := encodeAttemptState(t, pool, recordingID, "h264"); ok {
		t.Errorf("attempt row remains (state = %q) after clearEncodeAttempt with canceled ctx; want deleted", state)
	}
}

// installLeakyExitZeroFakeFFmpeg は installFakeFFmpeg と同じく入力を出力へ
// コピーして exit 0 するが、stderr の書き込み端を継承したまま生き残る孫プロセス
// （sleep）を残す偽 ffmpeg。TestEncodeWorker_WaitDelayExpiredOnSuccess が、
// 「ffmpeg 自体は完走したが孫プロセスが fd を握ったままで WaitDelay が先に
// 切れる」exit 0 版のハングを再現するために使う。
func installLeakyExitZeroFakeFFmpeg(t *testing.T, sleepSeconds int) (ffmpegPath, childPIDMarker string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffmpeg")
	childPIDMarker = filepath.Join(dir, "child-pid")
	script := "#!/bin/sh\n" +
		"set -e\n" +
		"input=\"\"\n" +
		"output=\"\"\n" +
		"prev=\"\"\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"-i\" ]; then input=\"$a\"; fi\n" +
		"  prev=\"$a\"\n" +
		"  output=\"$a\"\n" +
		"done\n" +
		"if [ -z \"$input\" ] || [ -z \"$output\" ]; then\n" +
		"  echo \"fake-ffmpeg: missing input/output\" >&2\n" +
		"  exit 2\n" +
		"fi\n" +
		// バックグラウンドの sleep は stderr の fd（cmd.Stderr が &strings.Builder
		// なので os/exec が張るパイプ）を継承したまま、この shell が exit 0 で
		// 終わった後も生き残る。
		"sleep " + strconv.Itoa(sleepSeconds) + " &\n" +
		"echo $! > " + childPIDMarker + "\n" +
		"printf 'out_time_ms=1000\\nprogress=continue\\n'\n" +
		"printf 'out_time_ms=2000\\nprogress=end\\n'\n" +
		"cp \"$input\" \"$output\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, childPIDMarker
}

// TestEncodeWorker_WaitDelayExpiredOnSuccess_TreatedAsSuccess は、ffmpeg が
// exit 0 で完走しても孫プロセスが stderr の fd を握ったままだと Cmd.Wait が
// exec.ErrWaitDelay を返す（go1.26.6 os/exec の awaitGoroutines。cmd.Stderr は
// *os.File ではなく &strings.Builder なので、Wait はそれをコピーする goroutine
// の終了も待つ）ケースを固定する。この分岐が無いと、完走した正常なエンコードが
// 「ffmpeg failed: exec: WaitDelay expired before I/O complete」として failed
// 行になり、River が再試行し続ける（MaxAttempts 25）。
//
// workerExecWaitDelay（internal/worker/exec.go）が実際に経過するのを待つ必要が
// あるため、このテストは数秒かかる。
func TestEncodeWorker_WaitDelayExpiredOnSuccess_TreatedAsSuccess(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	rel := "20240101/waitdelay-success.m2ts"
	content := []byte("fake-ts-payload-for-waitdelay-success-test")
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, rel, []string{"h264"}, content)

	// sleep は WaitDelay より十分長く保つ（この shell 自体は即座に exit するので、
	// installSlowFakeFFmpeg のような cancel との競合はない。PID 再利用を避ける
	// ため、テスト終了時の cleanup 猶予も込みで長めに取る）。
	setShortWorkerExecWaitDelay(t)
	sleepSeconds := int(workerExecWaitDelay/time.Second*3) + 5
	leakyFFmpeg, childPIDMarker := installLeakyExitZeroFakeFFmpeg(t, sleepSeconds)
	sleepStartedAt := time.Now()
	defer func() {
		if time.Since(sleepStartedAt) > time.Duration(sleepSeconds)*time.Second {
			return
		}
		pidBytes, err := os.ReadFile(childPIDMarker)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
		if err != nil {
			return
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	}()

	var logBuf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	w := &EncodeWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: scratchDir,
		FFmpeg:     leakyFFmpeg,
		// 実 ffprobe を shell out させない（本題である WaitDelay の再現には
		// 無関係。probeEncodeDuration の失敗は進捗計測を無効にするだけ）。
		FFprobe: "/nonexistent/ffprobe",
		Profiles: config.EncodeConfig{
			FFmpeg: leakyFFmpeg,
			Profiles: []config.EncodeProfile{{
				Name:       "h264",
				Container:  "mp4",
				VideoCodec: "libx264",
				AudioCodec: "aac",
			}},
		},
	}

	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
	}

	start := time.Now()
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work(): %v (log: %s)", err, logBuf.String())
	}
	// WaitDelay の経路を実際に踏んだことの裏付け --- 踏んでいなければ ffmpeg が
	// 即座に完了して Work() もすぐ返り、この分岐は何も検証していないことになる。
	if elapsed := time.Since(start); elapsed < workerExecWaitDelay/2 {
		t.Fatalf("Work() returned after %s; want roughly workerExecWaitDelay (%s), the leaky child fd did not force a WaitDelay wait", elapsed, workerExecWaitDelay)
	}

	if !strings.Contains(logBuf.String(), "WaitDelay expired before I/O completed") {
		t.Errorf("log output = %q, want a warning distinguishing the WaitDelay-on-success path", logBuf.String())
	}

	if state, ok := encodeAttemptState(t, pool, recordingID, "h264"); ok {
		t.Errorf("recording_encode_attempts row = %q after WaitDelay-on-success; want cleared (treated as success)", state)
	}

	// 「成功として扱った」ことそのものを検証する --- 上の attempt 行チェックは
	// 行が一度も書かれなかった回帰も見逃す（TestEncodeWorker_SuccessAndIdempotent
	// と同じ形の検査）。
	var encodedCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'encoded' AND profile = 'h264' AND state = 'active'`,
		recordingID,
	).Scan(&encodedCount); err != nil {
		t.Fatal(err)
	}
	if encodedCount != 1 {
		t.Errorf("active encoded media_assets = %d, want 1 (WaitDelay-on-success must still commit)", encodedCount)
	}
}

// setShortWorkerExecWaitDelay は workerExecWaitDelay を 1 秒に差し替え、t.Cleanup で
// 戻す。WaitDelay が実際に経過するのを待つテストが、本番既定の 5 秒を毎回払わない
// ための差し替え。sleep や timeout は呼び出し側が workerExecWaitDelay の倍数で導出
// しているので、比率（sleep > WaitDelay、Work の復帰上限 = 2 倍）は変わらない。
func setShortWorkerExecWaitDelay(t *testing.T) {
	t.Helper()
	prev := workerExecWaitDelay
	workerExecWaitDelay = time.Second
	t.Cleanup(func() { workerExecWaitDelay = prev })
}
