package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// TestSeekTileCount は枚数の決定を実装の定数ではなくリテラルで固定する。
//
// 形（間隔 10 秒・上限 1080 枚）は web/src/lib/seek-tiles.ts にも同じ値があり、
// openapi.yaml を経由しないので手で揃えるしかない。片方を変えたらこのテストが
// 落ちるので、変えるときはもう片方も同じ PR で直すことになる。
func TestSeekTileCount(t *testing.T) {
	tests := []struct {
		name string
		dur  time.Duration
		want int
	}{
		{"尺が取れない（0）でも 1 枚", 0, 1},
		{"負の尺でも 1 枚", -time.Second, 1},
		{"10 秒ちょうどで 1 枚", 10 * time.Second, 1},
		{"11 秒で 2 枚", 11 * time.Second, 2},
		{"1 時間で 360 枚", time.Hour, 360},
		{"3 時間ちょうどで 1080 枚", 3 * time.Hour, 1080},
		{"3 時間を超えたら 1080 枚で頭打ち", 5 * time.Hour, 1080},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := seekTileCount(tt.dur); got != tt.want {
				t.Errorf("seekTileCount(%v) = %d, want %d", tt.dur, got, tt.want)
			}
		})
	}
}

// TestSeekTilesRelPath は相対パスをリテラルで固定する。poster と同じ
// thumbnails/ 配下に置く（rel_path の名前空間の検査を増やさない）。
func TestSeekTilesRelPath(t *testing.T) {
	if got, want := seekTilesRelPath(7), "thumbnails/7_tiles.jpg"; got != want {
		t.Errorf("seekTilesRelPath(7) = %q, want %q", got, want)
	}
}

// TestSeekTilesWorker_ExtractTileArgs は 1 枚の抽出が「入力シーク（-ss が -i より
// 前）」で行われることと、SAR の補正と 16:9 への pad がフィルタに入っていることを
// 固定する。読み捨てをやめると生成コストが番組長に比例するようになる（issue #873
// の測定で全デコード方式の約 9 倍）。
func TestSeekTilesWorker_ExtractTileArgs(t *testing.T) {
	var gotArgs []string
	out := filepath.Join(t.TempDir(), "out.jpg")
	w := &SeekTilesWorker{
		runCmd: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			gotArgs = args
			return nil, os.WriteFile(out, tinyJPEG, 0o644)
		},
	}
	if err := w.extractTile(context.Background(), "in.m2ts", out, 30*time.Second); err != nil {
		t.Fatalf("extractTile: %v", err)
	}

	ss := indexOfArg(gotArgs, "-ss")
	if ss < 0 || ss+1 >= len(gotArgs) {
		t.Fatalf("no -ss in args: %v", gotArgs)
	}
	if input := indexOfArg(gotArgs, "-i"); input < 0 || ss > input {
		t.Errorf("-ss must come before -i (input seek), args: %v", gotArgs)
	}
	if got := gotArgs[ss+1]; got != "30.000" {
		t.Errorf("-ss = %q, want %q", got, "30.000")
	}
	vf := indexOfArg(gotArgs, "-vf")
	if vf < 0 || vf+1 >= len(gotArgs) {
		t.Fatalf("no -vf filter in args: %v", gotArgs)
	}
	const want = "scale=round(iw*sar/2)*2:ih,setsar=1," +
		"scale=160:90:force_original_aspect_ratio=decrease," +
		"pad=160:90:(ow-iw)/2:(oh-ih)/2,setsar=1"
	if got := gotArgs[vf+1]; got != want {
		t.Errorf("-vf = %q, want %q", got, want)
	}
}

// TestSeekTilesWorker_ComposeSheetArgs は合成が「10 列 x 行数」の格子で、
// フレーム列が 0 始まりであることを固定する。クライアントは列数と 1 枚の大きさ
// だけを知っていれば位置を計算できるので、この 2 つがずれると全部の位置がずれる。
func TestSeekTilesWorker_ComposeSheetArgs(t *testing.T) {
	var gotArgs []string
	w := &SeekTilesWorker{
		runCmd: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			gotArgs = args
			return nil, nil
		},
	}
	if err := w.composeSheet(context.Background(), "/scratch/frames", "out.jpg", 4); err != nil {
		t.Fatalf("composeSheet: %v", err)
	}
	if got := gotArgs[indexOfArg(gotArgs, "-start_number")+1]; got != "0" {
		t.Errorf("-start_number = %q, want %q", got, "0")
	}
	if got := gotArgs[indexOfArg(gotArgs, "-vf")+1]; got != "tile=10x4" {
		t.Errorf("-vf = %q, want %q", got, "tile=10x4")
	}
}

// countingRunCmd は runCmd の呼び出し回数と、書き出された出力パスを記録する。
type countingRunCmd struct {
	calls    int
	outputs  []string
	failOn   string // この出力パスへの書き出しで失敗させる（空なら常に成功）
	silentOn string // この出力パスでは何も書かずに成功を返す（ffmpeg が 0 フレームで終わる形）
	probeErr bool   // ffprobe を失敗させる
	// videoEnd が正なら、-ss がそれ以上の抽出は何も書かずに成功を返す
	// （映像の終端より後ろへの入力シーク。実 ffmpeg 9 は非 0 で終わるが、
	// 終了コードに頼らない形で再現する）。
	videoEnd float64
	// lastKeyframe が正なら、-ss がそれより 0.5 秒以上後ろの抽出は失敗する
	// （GOP の長い H.264 で、最後のキーフレームより後ろが取れない形。ffmpeg 9 の
	// 実挙動に合わせて非 0 で返す）。
	lastKeyframe float64
	// formatDuration が空でなければ format=duration にはこの値を返す（映像より
	// 音声が長い TS の形）。空なら duration を返す。
	formatDuration string
	seeks          []string
	duration       string
}

func (c *countingRunCmd) run(_ context.Context, name string, args ...string) ([]byte, error) {
	if strings.Contains(name, "ffprobe") || containsArg(args, "stream=duration") {
		if c.probeErr {
			return nil, fmt.Errorf("ffprobe: injected failure")
		}
		if containsArg(args, "format=duration") {
			if c.formatDuration != "" {
				return []byte(c.formatDuration + "\n"), nil
			}
			return []byte(c.duration + "\n"), nil
		}
		// 実 ffprobe は TS に対して programs 節と streams 節の 2 行を返す。
		return []byte(c.duration + "\n" + c.duration + "\n"), nil
	}
	c.calls++
	if len(args) == 0 {
		return nil, fmt.Errorf("ffmpeg: no args")
	}
	out := args[len(args)-1]
	c.outputs = append(c.outputs, out)
	if c.failOn != "" && out == c.failOn {
		return nil, fmt.Errorf("ffmpeg: injected failure for %s", out)
	}
	if c.silentOn != "" && out == c.silentOn {
		return nil, nil
	}
	if ss := indexOfArg(args, "-ss"); ss >= 0 {
		c.seeks = append(c.seeks, args[ss+1])
		at, _ := strconv.ParseFloat(args[ss+1], 64)
		if c.videoEnd > 0 && at >= c.videoEnd {
			return nil, nil
		}
		if c.lastKeyframe > 0 && at > c.lastKeyframe+0.5 {
			return nil, fmt.Errorf("ffmpeg: nothing was written (ss=%s)", args[ss+1])
		}
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return nil, err
	}
	return nil, os.WriteFile(out, tinyJPEG, 0o644)
}

func seedOriginalForSeekTiles(t *testing.T, pool *sqlcgen.Queries, mediaDir string, recordingID int64, rel string) {
	const content = "fake-ts"
	t.Helper()
	full := filepath.Join(mediaDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     rel,
		SizeBytes:   int64(len(content)),
	}); err != nil {
		t.Fatalf("seeding original: %v", err)
	}
}

func runSeekTilesJob(t *testing.T, w *SeekTilesWorker, recordingID int64) error {
	t.Helper()
	return w.Work(context.Background(), &river.Job[SeekTilesJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   SeekTilesJobArgs{RecordingID: recordingID},
	})
}

// TestSeekTilesWorker_CreatesAsset は生成からコミットまでの 1 周を見る。
// 枚数はリテラル（100 秒 → 10 枚 → 1 行）で固定する。
func TestSeekTilesWorker_CreatesAsset(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	seedOriginalForSeekTiles(t, sqlcgen.New(pool), mediaDir, recordingID, "shows/tiles.m2ts")

	cmd := &countingRunCmd{duration: "100"}
	w := &SeekTilesWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: t.TempDir(),
		runCmd:     cmd.run,
	}
	if err := runSeekTilesJob(t, w, recordingID); err != nil {
		t.Fatalf("SeekTilesWorker.Work: %v", err)
	}

	// 100 秒 / 10 秒 = 10 枚の抽出 + 1 回の合成。
	if cmd.calls != 11 {
		t.Errorf("ffmpeg calls = %d, want 11 (10 tiles + 1 compose)", cmd.calls)
	}

	row, err := sqlcgen.New(pool).GetSeekTilesMediaAssetForServing(context.Background(), recordingID)
	if err != nil {
		t.Fatalf("seek_tiles asset missing: %v", err)
	}
	if row.RelPath != "thumbnails/1_tiles.jpg" && row.RelPath != fmt.Sprintf("thumbnails/%d_tiles.jpg", recordingID) {
		t.Errorf("rel_path = %q, want thumbnails/%d_tiles.jpg", row.RelPath, recordingID)
	}
	if row.SizeBytes != int64(len(tinyJPEG)) {
		t.Errorf("size_bytes = %d, want %d", row.SizeBytes, len(tinyJPEG))
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "thumbnails", fmt.Sprintf("%d_tiles.jpg", recordingID))); err != nil {
		t.Errorf("tile sheet not written to media: %v", err)
	}
	// scratch は残骸を残さない。
	entries, err := os.ReadDir(filepath.Join(w.ScratchDir, "seek_tiles"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading scratch: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("scratch left behind %d entries, want 0", len(entries))
	}
}

// 既に active な seek_tiles があるなら ffmpeg を走らせない（冪等）。
func TestSeekTilesWorker_IdempotentRerun(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	seedOriginalForSeekTiles(t, sqlcgen.New(pool), mediaDir, recordingID, "shows/idem.m2ts")

	cmd := &countingRunCmd{duration: "30"}
	w := &SeekTilesWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: t.TempDir(), runCmd: cmd.run}
	if err := runSeekTilesJob(t, w, recordingID); err != nil {
		t.Fatalf("first run: %v", err)
	}
	firstCalls := cmd.calls
	if err := runSeekTilesJob(t, w, recordingID); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if cmd.calls != firstCalls {
		t.Errorf("ffmpeg calls on rerun = %d, want %d (no work)", cmd.calls, firstCalls)
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'seek_tiles'`, recordingID).Scan(&count); err != nil {
		t.Fatalf("counting seek_tiles rows: %v", err)
	}
	if count != 1 {
		t.Errorf("seek_tiles rows = %d, want 1", count)
	}
}

// original が無ければ何もせず成功で終える（レベルトリガー。再試行しても埋まらない）。
func TestSeekTilesWorker_NoOriginal_Skips(t *testing.T) {
	pool := setupTestPool(t)
	recordingID := insertTestRecording(t, pool)

	cmd := &countingRunCmd{duration: "30"}
	w := &SeekTilesWorker{Pool: pool, MediaDir: t.TempDir(), ScratchDir: t.TempDir(), runCmd: cmd.run}
	if err := runSeekTilesJob(t, w, recordingID); err != nil {
		t.Fatalf("SeekTilesWorker.Work: %v", err)
	}
	if cmd.calls != 0 {
		t.Errorf("ffmpeg calls without an original = %d, want 0", cmd.calls)
	}
}

// 途中で失敗したら部分成果をコミットしない（行の存在 = 全部そろっている）。
// scratch の残骸も次回の実行で捨てる。
func TestSeekTilesWorker_PartialFailureCommitsNothing(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	seedOriginalForSeekTiles(t, sqlcgen.New(pool), mediaDir, recordingID, "shows/partial.m2ts")

	// 3 枚中 2 枚目（000001.jpg）の抽出で落とす。最後の 1 枚は直前のタイルで
	// 埋めるので、途中の失敗で見る。
	cmd := &countingRunCmd{
		duration: "30",
		failOn:   filepath.Join(scratchDir, "seek_tiles", fmt.Sprintf("%d", recordingID), "000001.jpg"),
	}
	w := &SeekTilesWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: scratchDir, runCmd: cmd.run}
	if err := runSeekTilesJob(t, w, recordingID); err == nil {
		t.Fatal("Work() succeeded, want an error from the injected extraction failure")
	}

	if _, err := sqlcgen.New(pool).GetActiveSeekTilesMediaAssetID(context.Background(), recordingID); err == nil {
		t.Error("seek_tiles row was committed even though the extraction failed partway")
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "thumbnails", fmt.Sprintf("%d_tiles.jpg", recordingID))); err == nil {
		t.Error("tile sheet was written to media even though the extraction failed partway")
	}
	if _, err := os.Stat(filepath.Join(scratchDir, "seek_tiles", fmt.Sprintf("%d", recordingID), "000000.jpg")); err == nil {
		t.Error("scratch frames were left behind after a failed run")
	}
}

// 長さが取れないときは 1 枚だけの格子をコミットせずに失敗する。コミットすると
// 行が「全部そろっている」を主張し、定期パスが作り直さず、until_encoded の
// 原本削除の条件も満たしてしまう。
func TestSeekTilesWorker_ProbeFailureCommitsNothing(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	seedOriginalForSeekTiles(t, sqlcgen.New(pool), mediaDir, recordingID, "shows/noprobe.m2ts")

	cmd := &countingRunCmd{probeErr: true}
	w := &SeekTilesWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: t.TempDir(), runCmd: cmd.run}
	if err := runSeekTilesJob(t, w, recordingID); err == nil {
		t.Fatal("Work() succeeded, want an error when ffprobe fails")
	}
	if _, err := sqlcgen.New(pool).GetActiveSeekTilesMediaAssetID(context.Background(), recordingID); err == nil {
		t.Error("seek_tiles row was committed even though the duration was unknown")
	}
	if cmd.calls != 0 {
		t.Errorf("ffmpeg calls after a probe failure = %d, want 0", cmd.calls)
	}
}

// ffmpeg が 0 フレームで終了コード 0 を返しても、穴の空いた格子をコミットしない
// （image2 は連番の穴で読むのをやめ、以降が黒のまま合成される）。
func TestSeekTilesWorker_MissingTileCommitsNothing(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	seedOriginalForSeekTiles(t, sqlcgen.New(pool), mediaDir, recordingID, "shows/hole.m2ts")

	cmd := &countingRunCmd{
		duration: "30",
		silentOn: filepath.Join(scratchDir, "seek_tiles", fmt.Sprintf("%d", recordingID), "000001.jpg"),
	}
	w := &SeekTilesWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: scratchDir, runCmd: cmd.run}
	if err := runSeekTilesJob(t, w, recordingID); err == nil {
		t.Fatal("Work() succeeded, want an error when a tile was not written")
	}
	if _, err := sqlcgen.New(pool).GetActiveSeekTilesMediaAssetID(context.Background(), recordingID); err == nil {
		t.Error("seek_tiles row was committed with a missing tile")
	}
}

// 抜き出し位置は i×10 秒だが、映像の終端から 1 秒より後ろには置かない。
func TestSeekTileAt(t *testing.T) {
	tests := []struct {
		i    int
		dur  time.Duration
		want time.Duration
	}{
		{0, 30020 * time.Millisecond, 0},
		{2, 30020 * time.Millisecond, 20 * time.Second},
		{3, 30020 * time.Millisecond, 29020 * time.Millisecond},
		{0, 500 * time.Millisecond, 0},
	}
	for _, tt := range tests {
		if got := seekTileAt(tt.i, tt.dur); got != tt.want {
			t.Errorf("seekTileAt(%d, %s) = %s, want %s", tt.i, tt.dur, got, tt.want)
		}
	}
}

// 長さが 10 秒の倍数をわずかに超える録画（30.02 秒 → 4 枚）でも、最後のタイルを
// 映像の終端より手前から取り、コミットまで進む。終端を指すと 1 フレームも出ず、
// 毎回同じ枚で失敗し続けて until_encoded の原本が消えなくなる。
func TestSeekTilesWorker_LastTileStaysBeforeVideoEnd(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	seedOriginalForSeekTiles(t, sqlcgen.New(pool), mediaDir, recordingID, "shows/tail.m2ts")

	cmd := &countingRunCmd{duration: "30.02", videoEnd: 30.0}
	w := &SeekTilesWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: t.TempDir(), runCmd: cmd.run}
	if err := runSeekTilesJob(t, w, recordingID); err != nil {
		t.Fatalf("Work() = %v, want success", err)
	}
	if want := []string{"0.000", "10.000", "20.000", "29.020"}; !slices.Equal(cmd.seeks, want) {
		t.Errorf("seeks = %v, want %v", cmd.seeks, want)
	}
	if _, err := sqlcgen.New(pool).GetActiveSeekTilesMediaAssetID(context.Background(), recordingID); err != nil {
		t.Errorf("seek_tiles row was not committed: %v", err)
	}
}

// 長さ 0 秒も「取れない」と同じ扱いにする（1 枚の格子をコミットしない）。
func TestSeekTilesWorker_ZeroDurationCommitsNothing(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	seedOriginalForSeekTiles(t, sqlcgen.New(pool), mediaDir, recordingID, "shows/zero.m2ts")

	cmd := &countingRunCmd{duration: "0.000000"}
	w := &SeekTilesWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: t.TempDir(), runCmd: cmd.run}
	if err := runSeekTilesJob(t, w, recordingID); err == nil {
		t.Fatal("Work() succeeded, want an error for a zero duration")
	}
	if _, err := sqlcgen.New(pool).GetActiveSeekTilesMediaAssetID(context.Background(), recordingID); err == nil {
		t.Error("seek_tiles row was committed for a zero duration")
	}
}

// GOP の長い H.264 では最後のキーフレームより後ろが取れない（x264 GOP 5 秒で、
// 終端の 2 秒手前でも失敗した）。最後の 1 枚だけは直前のタイルで埋めてコミットまで
// 進む。埋めないと毎回同じ枚で落ち、until_encoded の原本が消えなくなる。
func TestSeekTilesWorker_LastTileFallsBackToPrevious(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	seedOriginalForSeekTiles(t, sqlcgen.New(pool), mediaDir, recordingID, "shows/longgop.m2ts")

	// 40.007 秒 → 5 枚、最後は 39.007 秒。最後のキーフレームは 35 秒。
	cmd := &countingRunCmd{duration: "40.007", lastKeyframe: 35}
	w := &SeekTilesWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: t.TempDir(), runCmd: cmd.run}
	if err := runSeekTilesJob(t, w, recordingID); err != nil {
		t.Fatalf("Work() = %v, want success", err)
	}
	if want := []string{"0.000", "10.000", "20.000", "30.000", "39.007"}; !slices.Equal(cmd.seeks, want) {
		t.Errorf("seeks = %v, want %v", cmd.seeks, want)
	}
	if _, err := sqlcgen.New(pool).GetActiveSeekTilesMediaAssetID(context.Background(), recordingID); err != nil {
		t.Errorf("seek_tiles row was not committed: %v", err)
	}
}

// 1 枚しか無いときは埋める元が無いので失敗する。
func TestSeekTilesWorker_SingleTileFailureCommitsNothing(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	seedOriginalForSeekTiles(t, sqlcgen.New(pool), mediaDir, recordingID, "shows/single.m2ts")

	cmd := &countingRunCmd{
		duration: "5",
		failOn:   filepath.Join(scratchDir, "seek_tiles", fmt.Sprintf("%d", recordingID), "000000.jpg"),
	}
	w := &SeekTilesWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: scratchDir, runCmd: cmd.run}
	if err := runSeekTilesJob(t, w, recordingID); err == nil {
		t.Fatal("Work() succeeded, want an error when the only tile fails")
	}
	if _, err := sqlcgen.New(pool).GetActiveSeekTilesMediaAssetID(context.Background(), recordingID); err == nil {
		t.Error("seek_tiles row was committed without any extracted tile")
	}
}

// 長さは format ではなく映像ストリームから取る。映像が音声より早く終わる TS
// （映像 19.5 秒・format 22 秒）で format を使うと 3 枚になり、最後の 20 秒は
// 映像の終端を越える。
func TestSeekTilesWorker_UsesVideoStreamDuration(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	seedOriginalForSeekTiles(t, sqlcgen.New(pool), mediaDir, recordingID, "shows/shortvideo.m2ts")

	cmd := &countingRunCmd{duration: "19.5", formatDuration: "22"}
	w := &SeekTilesWorker{Pool: pool, MediaDir: mediaDir, ScratchDir: t.TempDir(), runCmd: cmd.run}
	if err := runSeekTilesJob(t, w, recordingID); err != nil {
		t.Fatalf("Work() = %v, want success", err)
	}
	if want := []string{"0.000", "10.000"}; !slices.Equal(cmd.seeks, want) {
		t.Errorf("seeks = %v, want %v", cmd.seeks, want)
	}
}
