package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/webhook"
)

func TestBuildFFmpegArgs(t *testing.T) {
	crf := 23
	p := config.EncodeProfile{
		Name:       "h264",
		Container:  "mp4",
		VideoCodec: "libx264",
		AudioCodec: "aac",
		Height:     720,
		CRF:        &crf,
		Preset:     "medium",
		ExtraArgs:  []string{"-movflags", "+faststart"},
	}
	args := BuildFFmpegArgs(p, "/in.m2ts", "/out.mp4")

	// 必須フラグと入出力の位置。
	if !slices.Contains(args, "-i") {
		t.Fatal("missing -i")
	}
	if got := args[slices.Index(args, "-i")+1]; got != "/in.m2ts" {
		t.Errorf("input = %q, want /in.m2ts", got)
	}
	if args[len(args)-1] != "/out.mp4" {
		t.Errorf("last arg = %q, want output path", args[len(args)-1])
	}
	if !slices.Contains(args, "-progress") {
		t.Fatal("missing -progress (must use pipe:1, not stderr scraping)")
	}
	if got := args[slices.Index(args, "-progress")+1]; got != "pipe:1" {
		t.Errorf("-progress target = %q, want pipe:1", got)
	}
	if !slices.Contains(args, "libx264") || !slices.Contains(args, "aac") {
		t.Errorf("args missing codecs: %v", args)
	}
	if !slices.Contains(args, "scale=-2:720") {
		t.Errorf("args missing scale filter: %v", args)
	}
	if !slices.Contains(args, "23") || !slices.Contains(args, "medium") {
		t.Errorf("args missing crf/preset: %v", args)
	}
	if !slices.Contains(args, "+faststart") {
		t.Errorf("args missing extra_args: %v", args)
	}
	// 自由形式のシェルは組み立てない。
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "&&") || strings.Contains(joined, "|") {
		t.Errorf("args look like shell: %v", args)
	}
}

func TestBuildFFmpegArgs_NoOptional(t *testing.T) {
	p := config.EncodeProfile{
		Name:       "copyish",
		Container:  "mkv",
		VideoCodec: "libx265",
		AudioCodec: "copy",
	}
	args := BuildFFmpegArgs(p, "in", "out")
	if slices.Contains(args, "-crf") {
		t.Error("crf should be omitted when nil")
	}
	if slices.Contains(args, "-preset") {
		t.Error("preset should be omitted when empty")
	}
	if slices.Contains(args, "-vf") {
		t.Error("scale should be omitted when height=0")
	}
	if !slices.Contains(args, "mkv") {
		t.Errorf("container -f missing: %v", args)
	}
}

func TestEncodedRelPath(t *testing.T) {
	got, err := EncodedRelPath("20240101/120000_title_1024.m2ts", "h264", "mp4")
	if err != nil {
		t.Fatal(err)
	}
	want := "20240101/120000_title_1024_h264.mp4"
	if got != want {
		t.Errorf("EncodedRelPath = %q, want %q", got, want)
	}

	// プロファイル名の ".." や "/" はパスに持ち込まない。
	got, err = EncodedRelPath("a.m2ts", "foo/../bar", "mkv")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "..") {
		t.Errorf("path contains ..: %q", got)
	}
	if strings.Count(got, "/") != 0 {
		t.Errorf("profile must not introduce hierarchy: %q", got)
	}
}

// installFakeFFmpeg は PATH 先頭に「入力を出力へコピーし progress を stdout に書く」
// 偽 ffmpeg を置き、元の PATH を復元する cleanup を返す。
func installFakeFFmpeg(t *testing.T) (ffmpegPath string) {
	t.Helper()
	dir := t.TempDir()
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.bat"
	}
	path := filepath.Join(dir, name)

	// 最後の引数が出力。入力は -i の次。
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
if [ -z "$input" ] || [ -z "$output" ]; then
  echo "fake-ffmpeg: missing input/output" >&2
  exit 2
fi
# 進捗を pipe:1（stdout）へ。stderr には出さない。
printf 'out_time_ms=1000\nprogress=continue\n'
printf 'out_time_ms=2000\nprogress=end\n'
cp "$input" "$output"
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

func seedRecordingWithOriginal(t *testing.T, pool *pgxpool.Pool, mediaDir, relPath string, profiles []string, content []byte) int64 {
	t.Helper()
	q := sqlcgen.New(pool)
	id, err := q.CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              "default",
		NetworkID:         32736,
		ServiceID:         1024,
		EventID:           int32(time.Now().UnixNano() % 1_000_000_000), // 衝突回避
		ServiceName:       "テスト",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "encode test",
		ProgramStartAt:    time.Now(),
		ProgramDurationMs: 1800000,
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("CreateRecording: %v", err)
	}
	if len(profiles) > 0 {
		if _, err := pool.Exec(context.Background(),
			`UPDATE recordings SET encode_profiles = $2 WHERE id = $1`, id, profiles); err != nil {
			t.Fatalf("set encode_profiles: %v", err)
		}
	}
	full, err := filepath.Abs(filepath.Join(mediaDir, filepath.FromSlash(relPath)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatal(err)
	}
	profileNil := (*string)(nil)
	if _, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id,
		Kind:        db.AssetKindOriginal,
		Profile:     profileNil,
		RelPath:     relPath,
		SizeBytes:   int64(len(content)),
	}); err != nil {
		t.Fatalf("CreateMediaAsset original: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM media_assets WHERE recording_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM recordings WHERE id = $1`, id)
	})
	return id
}

func TestEncodeWorker_SuccessAndIdempotent(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ffmpegPath := installFakeFFmpeg(t)

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	rel := "20240101/show.m2ts"
	content := []byte("fake-ts-payload-for-encode-test-0123456789")
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
		t.Fatalf("Work() first run: %v", err)
	}

	encRel := "20240101/show_h264.mp4"
	outPath := filepath.Join(mediaDir, filepath.FromSlash(encRel))
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading encoded file: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("encoded content = %q, want copy of input", got)
	}

	// media_assets 行が 1 件。
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'encoded' AND profile = 'h264' AND state = 'active'`,
		recordingID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("encoded assets = %d, want 1", count)
	}

	// 2 回目は冪等（再エンコードせず成功）。
	// ファイルを触って「上書きされない」ことを見る。
	marker := []byte("already-committed-should-not-overwrite")
	if err := os.WriteFile(outPath, marker, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() second run: %v", err)
	}
	got2, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != string(marker) {
		t.Errorf("second run rewrote file; idempotent skip failed")
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'encoded'`,
		recordingID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("after second run encoded assets = %d, want 1", count)
	}

	// scratch が掃除されていること。
	entries, _ := os.ReadDir(scratchDir)
	if len(entries) != 0 {
		// encode/ 配下も空なら OK。親に encode が残っていても中身が空なら良い。
		for _, e := range entries {
			sub := filepath.Join(scratchDir, e.Name())
			if infos, err := os.ReadDir(sub); err == nil && len(infos) > 0 {
				t.Errorf("scratch not cleaned: %s still has %d entries", sub, len(infos))
			}
		}
	}
}

// 成功時に encode.finished が発火し、ペイロードに profile が載ること。
func TestEncodeWorker_FiresEncodeFinishedWebhook(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ffmpegPath := installFakeFFmpeg(t)

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	rel := "20240101/finished.m2ts"
	content := []byte("payload-for-webhook-finished-test")
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, rel, []string{"h264"}, content)

	rec, client := newWebhookRecorder(t)
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
		Webhook: client,
	}

	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}

	events := rec.received()
	if len(events) != 1 {
		t.Fatalf("webhook events = %d, want 1: %+v", len(events), events)
	}
	got := events[0]
	if got.Type != webhook.EventEncodeFinished {
		t.Errorf("type = %q, want %q", got.Type, webhook.EventEncodeFinished)
	}
	if got.RecordingID != recordingID {
		t.Errorf("recordingId = %d, want %d", got.RecordingID, recordingID)
	}
	if got.Profile != "h264" {
		t.Errorf("profile = %q, want h264", got.Profile)
	}
	if got.Status != "finished" {
		t.Errorf("status = %q, want finished", got.Status)
	}
	if got.Title != "encode test" {
		t.Errorf("title = %q, want %q", got.Title, "encode test")
	}
	// attempt は失敗イベント専用（成功は 1 回しか配送されない）。
	if got.Attempt != 0 || got.MaxAttempts != 0 {
		t.Errorf("attempt/maxAttempts = %d/%d, want 0/0 on finished", got.Attempt, got.MaxAttempts)
	}

	// 冪等スキップ（既に active な encoded がある）では再発火しない。
	rec.reset()
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() second run: %v", err)
	}
	if events := rec.received(); len(events) != 0 {
		t.Errorf("webhook fired on idempotent skip: %+v", events)
	}
}

// 失敗時に encode.failed が発火すること。ペイロードの profile はジョブ引数（設定に
// 存在しない名前でも）そのまま載り、River の試行回数も載る（恒久的な失敗では
// 試行ごとに配送されるので、受け側が最終試行を見分けられるようにする）。
func TestEncodeWorker_FiresEncodeFailedWebhook(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/fail.m2ts", nil, []byte("data"))

	rec, client := newWebhookRecorder(t)
	w := &EncodeWorker{
		Pool:     pool,
		MediaDir: mediaDir,
		Profiles: config.EncodeConfig{}, // "missing" プロファイルは未定義 → 失敗する
		Webhook:  client,
	}

	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{Attempt: 3, MaxAttempts: 25},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "missing"},
	}
	if err := w.Work(context.Background(), job); err == nil {
		t.Fatal("expected error for unknown profile")
	}

	events := rec.received()
	if len(events) != 1 {
		t.Fatalf("webhook events = %d, want 1: %+v", len(events), events)
	}
	got := events[0]
	if got.Type != webhook.EventEncodeFailed {
		t.Errorf("type = %q, want %q", got.Type, webhook.EventEncodeFailed)
	}
	if got.RecordingID != recordingID {
		t.Errorf("recordingId = %d, want %d", got.RecordingID, recordingID)
	}
	if got.Profile != "missing" {
		t.Errorf("profile = %q, want missing", got.Profile)
	}
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.Attempt != 3 || got.MaxAttempts != 25 {
		t.Errorf("attempt/maxAttempts = %d/%d, want 3/25", got.Attempt, got.MaxAttempts)
	}
}

// encode.failed を発火するかの判定（両方向）。Work 越しの ctx キャンセル
// テストは notify 内の DB 読みも同時に失敗するため、この分岐だけを分離して見る。
func TestShouldNotifyEncodeFailure(t *testing.T) {
	boom := errors.New("ffmpeg failed")
	cases := []struct {
		name   string
		err    error
		ctxErr error
		want   bool
	}{
		{"failure with live ctx", boom, nil, true},
		{"failure while ctx canceled", boom, context.Canceled, false},
		{"failure while ctx deadline exceeded", boom, context.DeadlineExceeded, false},
		{"success", nil, nil, false},
		{"success while ctx canceled", nil, context.Canceled, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldNotifyEncodeFailure(c.err, c.ctxErr); got != c.want {
				t.Errorf("shouldNotifyEncodeFailure(%v, %v) = %v, want %v", c.err, c.ctxErr, got, c.want)
			}
		})
	}
}

// ctx キャンセル（River の停止・タイムアウト）では発火しないこと。判定そのものは
// TestShouldNotifyEncodeFailure が見る（ここは Work 越しに POST が飛ばないことの確認）。
func TestEncodeWorker_CtxCanceled_DoesNotFireWebhook(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/cancel.m2ts", nil, []byte("data"))

	rec, client := newWebhookRecorder(t)
	w := &EncodeWorker{
		Pool:     pool,
		MediaDir: mediaDir,
		Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{{
			Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
		}}},
		Webhook: client,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	job := &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
	}
	if err := w.Work(ctx, job); err == nil {
		t.Fatal("expected error with canceled ctx")
	}
	if events := rec.received(); len(events) != 0 {
		t.Errorf("webhook fired on ctx cancel: %+v", events)
	}
}

func TestEnqueueMissingEncodes_LevelTrigger(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	if _, err := pool.Exec(context.Background(), "DELETE FROM river_job"); err != nil {
		t.Fatal(err)
	}

	mediaDir := t.TempDir()
	content := []byte("payload")
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "x/a.m2ts",
		[]string{"h264", "h265"}, content)

	// h264 は既に encoded 済み → 投入しない。
	h264 := "h264"
	q := sqlcgen.New(pool)
	if _, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindEncoded,
		Profile:     &h264,
		RelPath:     "x/a_h264.mp4",
		SizeBytes:   1,
	}); err != nil {
		t.Fatal(err)
	}

	workers := NewWorkers(&Deps{Pool: pool})
	client, err := NewClient(pool, workers, ClientConfig{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if err := EnqueueMissingEncodes(context.Background(), client, pool, recordingID); err != nil {
		t.Fatalf("EnqueueMissingEncodes: %v", err)
	}

	// h265 だけ 1 件。
	var kinds []string
	rows, err := pool.Query(context.Background(),
		`SELECT args->>'profile' FROM river_job WHERE kind = 'encode' AND args->>'recording_id' = $1::text`,
		recordingID,
	)
	if err != nil {
		// recording_id は JSON number。文字列比較を避ける。
		rows, err = pool.Query(context.Background(),
			`SELECT args->>'profile' FROM river_job WHERE kind = 'encode' AND (args->>'recording_id')::bigint = $1`,
			recordingID,
		)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kinds, []string{"h265"}) {
		t.Errorf("enqueued profiles = %v, want [h265]", kinds)
	}

	// 再呼び出しは UniqueOpts で重複スキップ（エラーにならない）。
	if err := EnqueueMissingEncodes(context.Background(), client, pool, recordingID); err != nil {
		t.Fatalf("second EnqueueMissingEncodes: %v", err)
	}
}

func TestEncodeJobArgs_InsertOptsQueue(t *testing.T) {
	opts := EncodeJobArgs{}.InsertOpts()
	if opts.Queue != encodeQueue {
		t.Errorf("Queue = %q, want %q", opts.Queue, encodeQueue)
	}
	if !opts.UniqueOpts.ByArgs {
		t.Error("UniqueOpts.ByArgs should be true")
	}
}

// EncodeEnqueueHintArgs（issue #133 の事後追加ヒントジョブ）は encode キューと
// pending 状態での ByArgs 一意化を使うこと。
func TestEncodeEnqueueHintArgs_InsertOptsQueue(t *testing.T) {
	opts := EncodeEnqueueHintArgs{}.InsertOpts()
	if opts.Queue != encodeQueue {
		t.Errorf("Queue = %q, want %q", opts.Queue, encodeQueue)
	}
	if !opts.UniqueOpts.ByArgs {
		t.Error("UniqueOpts.ByArgs should be true")
	}
}

// river.ClientFromContextSafely が失敗する ctx（River の外から Work を直接呼んだ
// 場合）では、ruler_pass 完了時の reconcile_pass ヒントのように黙って何もしない
// のではなくエラーを返すこと。このジョブ自体の主目的が「encode ジョブを実際に
// 投入すること」であるため、client が無いことをサイレントな no-op にすると
// ユーザーの事後追加依頼が消えてしまう（EncodeEnqueueHintWorker.Work の doc
// コメント参照）。
func TestEncodeEnqueueHintWorker_Work_WithoutClient_Errors(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	w := &EncodeEnqueueHintWorker{Pool: pool}
	job := &river.Job[EncodeEnqueueHintArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   EncodeEnqueueHintArgs{RecordingID: 999999},
	}
	if err := w.Work(context.Background(), job); err == nil {
		t.Fatal("expected error when no river client is attached to ctx, got nil")
	}
}

// EncodeEnqueueHintWorker は EncodeEnqueueHintArgs ジョブを実際の River クライアント
// 経由で処理すると、EnqueueMissingEncodes を呼んで desired（recordings.encode_profiles）
// − observed（active encoded media_assets）の差分を encode ジョブとして投入すること。
// 既に active encoded な h264 は再投入せず h265 だけが投入されることまで見る（issue
// #133 の受け入れ「予約が無い録画で事後追加が成功し、encode_profiles に反映されて
// encode ジョブが投入されること」の worker 側の裏付け --- api 側は
// internal/api/recordings_encode_profiles_test.go が見る）。
func TestEncodeEnqueueHintWorker_EnqueuesMissingEncodes(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	if _, err := pool.Exec(context.Background(), "DELETE FROM river_job"); err != nil {
		t.Fatal(err)
	}

	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "hint/a.m2ts",
		[]string{"h264", "h265"}, []byte("payload"))

	h264 := "h264"
	q := sqlcgen.New(pool)
	if _, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindEncoded,
		Profile:     &h264,
		RelPath:     "hint/a_h264.mp4",
		SizeBytes:   1,
	}); err != nil {
		t.Fatal(err)
	}

	workers := NewWorkers(&Deps{Pool: pool})
	client, err := NewClient(pool, workers, ClientConfig{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	subscribeCh, subscribeCancel := client.Subscribe(river.EventKindJobCompleted)
	defer subscribeCancel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("starting client: %v", err)
	}
	defer func() {
		cancel()
		<-client.Stopped()
	}()

	if _, err := client.Insert(context.Background(), EncodeEnqueueHintArgs{RecordingID: recordingID}, nil); err != nil {
		t.Fatalf("inserting encode_enqueue_hint job: %v", err)
	}

	deadline := time.After(20 * time.Second)
	for {
		select {
		case event := <-subscribeCh:
			if event.Job.Kind == "encode_enqueue_hint" {
				goto hintCompleted
			}
		case <-deadline:
			t.Fatal("timed out waiting for encode_enqueue_hint completion")
		}
	}
hintCompleted:

	var profiles []string
	rows, err := pool.Query(context.Background(),
		`SELECT args->>'profile' FROM river_job WHERE kind = 'encode' AND (args->>'recording_id')::bigint = $1`,
		recordingID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		profiles = append(profiles, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(profiles, []string{"h265"}) {
		t.Errorf("enqueued profiles = %v, want [h265] (h264 は既に active encoded なので投入されない)", profiles)
	}
}
