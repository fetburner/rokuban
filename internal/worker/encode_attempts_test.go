package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
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
//
// startedMarker は sleep の前に作られる空ファイルのパス。呼び出し側はこれの
// 出現を cmd.Start() が実際に成功した証拠として待つ（下のテストのコメント参照）。
func installSlowFakeFFmpeg(t *testing.T, sleepSeconds int) (ffmpegPath, startedMarker string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffmpeg")
	startedMarker = filepath.Join(dir, "started")
	script := "#!/bin/sh\n: > " + startedMarker + "\nsleep " + strconv.Itoa(sleepSeconds) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, startedMarker
}

// TestEncodeWorker_AttemptRow_CtxCanceledLeavesRunning は ctx キャンセル
// （River の停止・タイムアウト）ではジョブの失敗として扱わないので、行が
// failed に上書きされず running のまま残ることを固定する
// （shouldNotifyEncodeFailure と同じ判定を試行状態の観測にも揃える。issue #316）。
//
// この判定を実際に load-bearing にしているのは attemptWriteContext ---
// markEncodeAttemptFailed の書き込みは job の ctx から切り離してあるので、
// encode.go の `if !shouldNotifyEncodeFailure(...) { return }` を削除すると
// キャンセル後でも書き込みが成功して failed に上書きされ、このテストが落ちる
// （切り離していなければ、書き込み自体がキャンセル済み ctx で失敗して偶然
// running のまま残り、ガードを消しても検出できない）。
//
// ctx は ffmpeg 実行中（running 行を書いた後）にキャンセルする ---
// 事前キャンセルだと markEncodeAttemptRunning 自身の DB 書き込みが ctx に
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
// touch。sleep の前に置く）を持たせ、その出現を待ってから cancel する。
// touch は子プロセスが exec された後にしか起きないので、その出現は
// 「fork/exec は完了した（os/exec の Cmd.Start が開始前にチェックする
// ctx.Done() は、開始しない分岐にはもう入れない）」ことの証拠になる ---
// ただし Start() 自身が呼び出し元へ戻っているとまでは保証しない（親が
// exec ステータス用パイプを読み切るのと子の最初の命令実行は別プロセスの
// 出来事で、順序は未計測）。FFprobe を存在しないパスにする変更は残す ---
// これが無いと実 ffprobe の呼び出し（timeout 上限 3 秒）がこの待ちを
// 不必要に長引かせる。
func TestEncodeWorker_AttemptRow_CtxCanceledLeavesRunning(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/attempt-cancel.m2ts", nil, []byte("data"))
	// workReturnTimeout は slowFFmpegSleepSeconds より十分大きく保つ必要が
	// **ある**（論理的に独立ではない）--- 計測: cancel() から Work() が返るまで
	// は sleep の長さとほぼ 1:1 で追従する（sleepSeconds=2 で ~2016ms、
	// sleepSeconds=6 で ~6015ms、いずれも cancel からの経過。TestEncodeWorker_
	// AttemptRow_CtxCanceledLeavesRunning に一時的な計測コードを入れて確認）。
	// 原因: encode.go の cmd.Stderr は *os.File ではなく &strings.Builder な
	// ので、os/exec は stderr を読む中継 goroutine を立て、cmd.Wait（
	// WaitDelay 未設定）はその goroutine が EOF で終わるまで待つ
	// （awaitGoroutines）。macOS の /bin/sh はスクリプト末尾のコマンドでも
	// exec せず fork するため（`pgrep -P` で確認）、kill は sh だけを殺し、
	// sh の子の sleep は inherited な stderr の書き込み端を握ったまま生き残る
	// --- Wait() はその sleep が寿命を迎えて stderr が閉じるまで、実質 sleep
	// の残り時間だけ返らない。5 秒 vs 5 秒だったときに確率的に落ちていたのは
	// この関係そのもの（issue #552）。この 2 つを同じ値・近い値に戻すと
	// 同じフレークが復活するので、上限側を十分離して保つ。
	const slowFFmpegSleepSeconds = 2
	const workReturnTimeout = 10 * time.Second
	slowFFmpeg, ffmpegStarted := installSlowFakeFFmpeg(t, slowFFmpegSleepSeconds)

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
			if !ok {
				return false
			}
			if state != "running" {
				t.Fatalf("state before cancel = %q, want running", state)
			}
			return true
		},
		func() string { return "timed out waiting for running attempt row to appear" },
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
	cancel()

	select {
	case err := <-workErr:
		if err == nil {
			t.Fatal("expected error with canceled ctx")
		}
	case <-time.After(workReturnTimeout):
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
	w.markEncodeAttemptFailed(context.Background(), recordingID, "h264", errors.New(longMsg))

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
// そのプロファイルは ListRecordingsMissingEncodes の候補から外れるので、
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
	w.markEncodeAttemptRunning(context.Background(), recordingID, "h264")
	if state, ok := encodeAttemptState(t, pool, recordingID, "h264"); !ok || state != "running" {
		t.Fatalf("fixture: state = %q, ok = %v, want running", state, ok)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.clearEncodeAttempt(ctx, recordingID, "h264")

	if state, ok := encodeAttemptState(t, pool, recordingID, "h264"); ok {
		t.Errorf("attempt row remains (state = %q) after clearEncodeAttempt with canceled ctx; want deleted", state)
	}
}
