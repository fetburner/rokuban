package thumbnail

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestSeekTileCount は枚数の決定を実装の定数ではなくリテラルで固定する。
//
// 形（間隔 10 秒・上限 1080 枚）は web/src/lib/seek-tiles.ts にも同じ値があり、
// openapi.yaml を経由しないので手で揃えるしかない。片方を変えたらこのテストが
// 落ちるので、変えるときはもう片方も同じ PR で直すことになる。
func TestSeekTileCount(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	for _, tc := range []struct {
		generation int64
		want       string
	}{
		{generation: 0, want: "thumbnails/7_tiles.jpg"},
		{generation: 1, want: "thumbnails/7_tiles.g1.jpg"},
		{generation: 12, want: "thumbnails/7_tiles.g12.jpg"},
	} {
		if got := seekTilesRelPath(7, tc.generation); got != tc.want {
			t.Errorf("seekTilesRelPath(7, %d) = %q, want %q", tc.generation, got, tc.want)
		}
	}
}

// TestSeekTilesWorker_ExtractTileArgs は 1 枚の抽出が「入力シーク（-ss が -i より
// 前）」で行われることと、SAR の補正と 16:9 への pad がフィルタに入っていることを
// 固定する。読み捨てをやめると生成コストが番組長に比例するようになる（issue #873
// の測定で全デコード方式の約 9 倍）。
func TestSeekTilesWorker_ExtractTileArgs(t *testing.T) {
	t.Parallel()
	var gotArgs []string
	out := filepath.Join(t.TempDir(), "out.jpg")
	w := &SeekTilesWorker{
		runCmd: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			gotArgs = args
			return nil, os.WriteFile(out, tinyJPEG, 0o644)
		},
	}
	testutil.RequireNoError(t, w.extractTile(context.Background(), "in.m2ts", out, 30*time.Second), "extractTile")

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
	t.Parallel()
	var gotArgs []string
	w := &SeekTilesWorker{
		runCmd: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			gotArgs = args
			return nil, nil
		},
	}
	testutil.RequireNoError(t, w.composeSheet(context.Background(), "/scratch/frames", "out.jpg", 4), "composeSheet")
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
	failOn   string // この basename への書き出しで失敗させる（空なら常に成功）
	silentOn string // この basename では何も書かず成功する（ffmpeg が 0 フレームで終わる形）
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
	outputContent  []byte
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
	if c.failOn != "" && filepath.Base(out) == c.failOn {
		return nil, fmt.Errorf("ffmpeg: injected failure for %s", out)
	}
	if c.silentOn != "" && filepath.Base(out) == c.silentOn {
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
	content := c.outputContent
	if content == nil {
		content = tinyJPEG
	}
	return nil, os.WriteFile(out, content, 0o644)
}

func seedOriginalForSeekTiles(t *testing.T, db *pgxpool.Pool, mediaDir string, recordingID int64, rel string) {
	const content = "fake-ts"
	t.Helper()
	full := filepath.Join(mediaDir, filepath.FromSlash(rel))
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(full), 0o755), "os.MkdirAll")
	testutil.RequireNoError(t, os.WriteFile(full, []byte(content), 0o644), "os.WriteFile")
	dbtest.MediaAsset(t, db, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.RelPath = rel
		fixtureParams.SizeBytes = int64(len(content))
	})
}

func runSeekTilesJob(t *testing.T, w *SeekTilesWorker, recordingID int64) error {
	t.Helper()
	return jobqfaketest.RunWorkerOnce(context.Background(), w,

		jobs.SeekTilesJobArgs{RecordingID: recordingID},
	)
}

// headAfterPutObjectStore は、同じキーへの 4 回目の Head（後から公開に入った試行の Head）を、
// 先に公開した試行の Put が終わるまで待たせる。2 試行の公開が「片方の Put の後に、
// もう片方がキーを確かめる」順で重なる窓を固定する。待ちを実際に掛けたかを gated で記録する。
type headAfterPutObjectStore struct {
	objstore.Store
	key     string
	putDone chan struct{}
	once    sync.Once
	mu      sync.Mutex
	heads   int
	gated   bool
}

// Head は同じキーの呼び出しを数え、4 回目だけ最初の Put の完了まで待ってから内側の Head を呼ぶ。
// 1・2 回目は nextUnusedSeekTilesRelPath（ffmpeg の前）、3・4 回目は公開時の Head である。
func (s *headAfterPutObjectStore) Head(ctx context.Context, key string) (objstore.Object, error) {
	if key == s.key {
		s.mu.Lock()
		s.heads++
		n := s.heads
		s.mu.Unlock()
		if n == 4 {
			select {
			case <-s.putDone:
			case <-ctx.Done():
				return objstore.Object{}, ctx.Err()
			case <-time.After(10 * time.Second):
				return objstore.Object{}, fmt.Errorf("first Put of %q did not finish", key)
			}
			s.mu.Lock()
			s.gated = true
			s.mu.Unlock()
		}
	}
	return s.Store.Head(ctx, key)
}

// Put は内側の Put が終わった時点で、待っている Head を解放する。
func (s *headAfterPutObjectStore) Put(ctx context.Context, key string, src io.Reader, size int64, checksum [32]byte, contentType string) error {
	err := s.Store.Put(ctx, key, src, size, checksum, contentType)
	if key == s.key {
		s.once.Do(func() { close(s.putDone) })
	}
	return err
}

// requireOnlyActiveSeekTilesCopy は、録画の seek_tiles のコピー行（削除済みを除く）がちょうど 1 つで、
// state が active かつ rel_path が want であることを確かめる。pending の残りもここで検出する。
func requireOnlyActiveSeekTilesCopy(t *testing.T, pool *pgxpool.Pool, recordingID int64, want string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT c.state, c.rel_path FROM media_asset_copies c
		JOIN media_assets a ON a.id = c.media_asset_id
		WHERE a.recording_id = $1 AND a.kind = 'seek_tiles' AND c.deleted_at IS NULL`, recordingID)
	testutil.RequireNoError(t, err, "querying seek_tiles copy rows")
	defer rows.Close()
	var got []string
	for rows.Next() {
		var state, relPath string
		testutil.RequireNoError(t, rows.Scan(&state, &relPath), "scanning seek_tiles copy row")
		got = append(got, state+" "+relPath)
	}
	testutil.RequireNoError(t, rows.Err(), "iterating seek_tiles copy rows")
	if wantRows := []string{"active " + want}; !slices.Equal(got, wantRows) {
		t.Fatalf("seek_tiles copy rows = %q, want exactly %q", got, wantRows)
	}
}

// TestSeekTilesWorkerUsesAttemptUniqueScratchForOverlappingAttempts は、同じ録画の seek tiles を
// 2 試行が重ねて走らせたとき、どちらも nil で終わり、コピー行がちょうど 1 つ active になることを固定する。
// 表の 2 行は公開時の Head の順序だけを変える。
//   - 順序を固定しない: 実際の並びに任せる。
//   - 2 本目の公開時 Head を 1 本目の Put の後に固定する: ラッパで待たせ、待ちが掛かったことも確かめる。
//     公開前の Head が、相手が Put を済ませたキーを見つけても失敗にしないことをこの行が固定する。
//
// どちらの行も、ffmpeg の出力パスは試行ごとに異なる。
func TestSeekTilesWorkerUsesAttemptUniqueScratchForOverlappingAttempts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		orderHeads bool
	}{
		{name: "unordered", orderHeads: false},
		{name: "second head after first put", orderHeads: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			mediaDir := t.TempDir()
			scratchDir := t.TempDir()
			recordingID := dbtest.Recording(t, pool)
			seedOriginalForSeekTiles(t, pool, mediaDir, recordingID, "shows/tiles-overlap.m2ts")
			wantKey := fmt.Sprintf("thumbnails/%d_tiles.jpg", recordingID)

			objects := testFSObjectStore(mediaDir)
			var gate *headAfterPutObjectStore
			if tc.orderHeads {
				gate = &headAfterPutObjectStore{Store: objects, key: wantKey, putDone: make(chan struct{})}
				objects = gate
			}

			paths := make(chan string, 64)
			release := make(chan struct{})
			results := make(chan error, 2)
			w := &SeekTilesWorker{
				Objects:         objects,
				ObjectPublisher: pgstoretest.Media(pool),
				Media:           pgstoretest.Media(pool), MediaDir: mediaDir, ScratchDir: scratchDir,
				runCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
					if strings.Contains(name, "ffprobe") || containsArg(args, "stream=duration") {
						return []byte("11\n"), nil
					}
					if len(args) == 0 {
						return nil, fmt.Errorf("ffmpeg: no args")
					}
					out := args[len(args)-1]
					paths <- out
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
						return nil, err
					}
					return nil, os.WriteFile(out, tinyJPEG, 0o644)
				},
			}

			for range 2 {
				go func() {
					results <- jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.SeekTilesJobArgs{RecordingID: recordingID})
				}()
			}
			gotPaths := make([]string, 0, 2)
			deadline := time.After(10 * time.Second)
		collect:
			for len(gotPaths) < 2 {
				select {
				case path := <-paths:
					gotPaths = append(gotPaths, path)
				case <-deadline:
					break collect
				}
			}
			// 到達が揃わなくても release で止まった試行を解放し、結果を読み切ってから検査する。
			close(release)

			for range 2 {
				select {
				case err := <-results:
					if err != nil {
						t.Errorf("SeekTilesWorker.Work: %v", err)
					}
				case <-time.After(15 * time.Second):
					t.Fatal("overlapping seek-tiles attempts did not finish after ffmpeg was released")
				}
			}
			if len(gotPaths) != 2 {
				t.Fatalf("ffmpeg reached %d of 2 overlapping attempts", len(gotPaths))
			}
			if gotPaths[0] == gotPaths[1] {
				t.Fatalf("overlapping attempts share ffmpeg output path %q", gotPaths[0])
			}
			if gate != nil {
				gate.mu.Lock()
				gated := gate.gated
				gate.mu.Unlock()
				if !gated {
					t.Fatal("the second publish Head did not wait for the first Put, so the ordering this row fixes was not forced")
				}
			}
			requireOnlyActiveSeekTilesCopy(t, pool, recordingID, wantKey)
		})
	}
}

// TestSeekTilesWorker_CreatesAsset は生成からコミットまでの 1 周を見る。
// 枚数はリテラル（100 秒 → 10 枚 → 1 行）で固定する。
func TestSeekTilesWorker_CreatesAsset(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	seedOriginalForSeekTiles(t, pool, mediaDir, recordingID, "shows/tiles.m2ts")

	cmd := &countingRunCmd{duration: "100"}
	w := &SeekTilesWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		Media:           pgstoretest.Media(pool), MediaDir: mediaDir,
		ScratchDir: t.TempDir(),
		runCmd:     cmd.run,
	}
	testutil.RequireNoError(t, runSeekTilesJob(t, w, recordingID), "SeekTilesWorker.Work")

	// 100 秒 / 10 秒 = 10 枚の抽出 + 1 回の合成。
	if cmd.calls != 11 {
		t.Errorf("ffmpeg calls = %d, want 11 (10 tiles + 1 compose)", cmd.calls)
	}

	row, err := pgstoretest.Media(pool).GetSeekTilesMediaAssetForServing(context.Background(), recordingID)
	testutil.RequireNoError(t, err, "seek_tiles asset missing")
	if row.RelPath != "thumbnails/1_tiles.jpg" && row.RelPath != fmt.Sprintf("thumbnails/%d_tiles.jpg", recordingID) {
		t.Errorf("rel_path = %q, want thumbnails/%d_tiles.jpg", row.RelPath, recordingID)
	}
	if row.SizeBytes != int64(len(tinyJPEG)) {
		t.Errorf("size_bytes = %d, want %d", row.SizeBytes, len(tinyJPEG))
	}
	assertMediaAssetCopy(t, pool, row.ID, row.RelPath)
	assertMediaAssetSHA256(t, pool, recordingID, store.AssetKindSeekTiles, tinyJPEG)
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
	t.Parallel()
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	seedOriginalForSeekTiles(t, pool, mediaDir, recordingID, "shows/idem.m2ts")

	cmd := &countingRunCmd{duration: "30"}
	w := &SeekTilesWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool), Media: pgstoretest.Media(pool), MediaDir: mediaDir, ScratchDir: t.TempDir(), runCmd: cmd.run}
	testutil.RequireNoError(t, runSeekTilesJob(t, w, recordingID), "first run")
	firstCalls := cmd.calls
	testutil.RequireNoError(t, runSeekTilesJob(t, w, recordingID), "second run")
	if cmd.calls != firstCalls {
		t.Errorf("ffmpeg calls on rerun = %d, want %d (no work)", cmd.calls, firstCalls)
	}

	var count int
	testutil.RequireNoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'seek_tiles'`, recordingID).Scan(&count), "counting seek_tiles rows")
	if count != 1 {
		t.Errorf("seek_tiles rows = %d, want 1", count)
	}
}

// Deleted seek_tiles keep their generation. A leftover output path from an
// uncommitted publish also advances the candidate instead of being overwritten.
func TestSeekTilesWorker_RecreatesAtUnusedGeneration(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	seedOriginalForSeekTiles(t, pool, mediaDir, recordingID, "shows/tiles-recreate.m2ts")
	cmd := &countingRunCmd{duration: "30", outputContent: []byte("first tile sheet")}
	w := &SeekTilesWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		Media:           pgstoretest.Media(pool), MediaDir: mediaDir,
		ScratchDir: t.TempDir(), runCmd: cmd.run,
	}
	testutil.RequireNoError(t, runSeekTilesJob(t, w, recordingID), "initial Work")

	var assetID int64
	testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT id FROM media_assets WHERE recording_id = $1 AND kind = 'seek_tiles'`, recordingID).Scan(&assetID), "loading initial seek_tiles asset")
	var oldRelPath string
	testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT rel_path FROM media_asset_copies WHERE media_asset_id = $1`, assetID).Scan(&oldRelPath), "loading initial seek_tiles path")
	testutil.MustExec(t, pool, "tombstoning seek_tiles copy", `UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE media_asset_id = $1`, assetID)
	oldAbs, err := mediapath.Resolve(mediaDir, oldRelPath)
	testutil.RequireNoError(t, err, "resolving tombstoned seek_tiles path")
	testutil.RequireNoError(t, os.Remove(oldAbs), "removing tombstoned seek_tiles file")

	// Simulate a file left by a publish whose DB commit did not complete. The tombstone
	// says generation 1 is next, so the occupied path advances it to generation 2.
	orphanPath := seekTilesRelPath(recordingID, 1)
	orphanAbs, err := mediapath.Resolve(mediaDir, orphanPath)
	testutil.RequireNoError(t, err, "resolving orphan seek_tiles path")
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(orphanAbs), 0o755), "creating orphan seek_tiles directory")
	const orphanBytes = "left by an uncommitted publish"
	testutil.RequireNoError(t, os.WriteFile(orphanAbs, []byte(orphanBytes), 0o644), "writing orphan seek_tiles output")

	cmd.outputContent = []byte("replacement tile sheet")
	testutil.RequireNoError(t, runSeekTilesJob(t, w, recordingID), "recreating seek_tiles")
	row, err := pgstoretest.Media(pool).GetSeekTilesMediaAssetForServing(ctx, recordingID)
	testutil.RequireNoError(t, err, "loading recreated seek_tiles")
	if want := seekTilesRelPath(recordingID, 2); row.RelPath != want {
		t.Errorf("recreated rel_path = %q, want %q", row.RelPath, want)
	}
	var generation int64
	testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT derived_generation FROM media_assets WHERE id = $1`, assetID).Scan(&generation), "reading recreated generation")
	if generation != 2 {
		t.Errorf("recreated generation = %d, want 2", generation)
	}
	assertMediaAssetSHA256(t, pool, recordingID, store.AssetKindSeekTiles, cmd.outputContent)
	if got, err := os.ReadFile(orphanAbs); err != nil || string(got) != orphanBytes {
		t.Errorf("occupied generation path changed: got %q, err %v", got, err)
	}
}

// original が無ければ何もせず成功で終える（レベルトリガー。再試行しても埋まらない）。
func TestSeekTilesWorker_NoOriginal_Skips(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)

	cmd := &countingRunCmd{duration: "30"}
	w := &SeekTilesWorker{
		Objects:         testFSObjectStore(t.TempDir()),
		ObjectPublisher: pgstoretest.Media(pool), Media: pgstoretest.Media(pool), MediaDir: t.TempDir(), ScratchDir: t.TempDir(), runCmd: cmd.run}
	testutil.RequireNoError(t, runSeekTilesJob(t, w, recordingID), "SeekTilesWorker.Work")
	if cmd.calls != 0 {
		t.Errorf("ffmpeg calls without an original = %d, want 0", cmd.calls)
	}
}

// TestSeekTilesWorker_CommitsNothingOnFailure は途中で失敗した実行が行を作らないことを、
// 失敗の形ごとに固定する（行の存在 = 全部そろっている。行ができると定期パスは作り直さず、
// until_encoded の原本削除の条件まで満たしてしまう）。どの形でも、タイル格子は media に
// 出ず、scratch に残骸を残さない。
func TestSeekTilesWorker_CommitsNothingOnFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  countingRunCmd
		// noFFmpeg は長さが取れない形。ffmpeg は 1 回も呼ばれない。
		noFFmpeg bool
	}{
		// 3 枚中 2 枚目の抽出で落とす。最後の 1 枚は直前のタイルで埋めるので、途中の失敗で見る。
		{name: "PartialFailure", cmd: countingRunCmd{duration: "30", failOn: "000001.jpg"}},
		// 長さが取れないときは 1 枚だけの格子をコミットせずに失敗する。
		{name: "ProbeFailure", cmd: countingRunCmd{duration: "30", probeErr: true}, noFFmpeg: true},
		// ffmpeg が 0 フレームで終了コード 0 を返しても、穴の空いた格子をコミットしない
		// （image2 は連番の穴で読むのをやめ、以降が黒のまま合成される）。
		{name: "MissingTile", cmd: countingRunCmd{duration: "30", silentOn: "000001.jpg"}},
		// 長さ 0 秒も「取れない」と同じ扱いにする（1 枚の格子をコミットしない）。
		{name: "ZeroDuration", cmd: countingRunCmd{duration: "0.000000"}},
		// 1 枚しか無いときは埋める元が無いので失敗する。
		{name: "SingleTileFailure", cmd: countingRunCmd{duration: "5", failOn: "000000.jpg"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			mediaDir := t.TempDir()
			scratchDir := t.TempDir()
			recordingID := dbtest.Recording(t, pool)
			seedOriginalForSeekTiles(t, pool, mediaDir, recordingID, "shows/failure.m2ts")

			cmd := tt.cmd
			w := &SeekTilesWorker{
				Objects:         testFSObjectStore(mediaDir),
				ObjectPublisher: pgstoretest.Media(pool),
				Media:           pgstoretest.Media(pool), MediaDir: mediaDir, ScratchDir: scratchDir,
				runCmd: cmd.run,
			}
			if err := runSeekTilesJob(t, w, recordingID); err == nil {
				t.Fatal("Work() succeeded, want an error")
			}
			if has, err := pgstoretest.Media(pool).HasActiveSeekTilesMediaAsset(context.Background(), recordingID); err != nil {
				t.Fatalf("checking seek_tiles row: %v", err)
			} else if has {
				t.Error("seek_tiles row was committed even though the run failed")
			}
			if _, err := os.Stat(filepath.Join(mediaDir, "thumbnails", fmt.Sprintf("%d_tiles.jpg", recordingID))); err == nil {
				t.Error("tile sheet was written to media even though the run failed")
			}
			entries, err := os.ReadDir(filepath.Join(scratchDir, "seek_tiles"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("reading scratch dir after failure: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("scratch dirs left behind after a failed run: %v", entries)
			}
			if tt.noFFmpeg && cmd.calls != 0 {
				t.Errorf("ffmpeg calls = %d, want 0", cmd.calls)
			}
		})
	}
}

// 抜き出し位置は i×10 秒だが、映像の終端から 1 秒より後ろには置かない。
func TestSeekTileAt(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	seedOriginalForSeekTiles(t, pool, mediaDir, recordingID, "shows/tail.m2ts")

	cmd := &countingRunCmd{duration: "30.02", videoEnd: 30.0}
	w := &SeekTilesWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool), Media: pgstoretest.Media(pool), MediaDir: mediaDir, ScratchDir: t.TempDir(), runCmd: cmd.run}
	if err := runSeekTilesJob(t, w, recordingID); err != nil {
		t.Fatalf("Work() = %v, want success", err)
	}
	if want := []string{"0.000", "10.000", "20.000", "29.020"}; !slices.Equal(cmd.seeks, want) {
		t.Errorf("seeks = %v, want %v", cmd.seeks, want)
	}
	if has, err := pgstoretest.Media(pool).HasActiveSeekTilesMediaAsset(context.Background(), recordingID); err != nil || !has {
		t.Errorf("seek_tiles row was not committed: present %v, err %v", has, err)
	}
}

// GOP の長い H.264 では最後のキーフレームより後ろが取れない（x264 GOP 5 秒で、
// 終端の 2 秒手前でも失敗した）。最後の 1 枚だけは直前のタイルで埋めてコミットまで
// 進む。埋めないと毎回同じ枚で落ち、until_encoded の原本が消えなくなる。
func TestSeekTilesWorker_LastTileFallsBackToPrevious(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	seedOriginalForSeekTiles(t, pool, mediaDir, recordingID, "shows/longgop.m2ts")

	// 40.007 秒 → 5 枚、最後は 39.007 秒。最後のキーフレームは 35 秒。
	cmd := &countingRunCmd{duration: "40.007", lastKeyframe: 35}
	w := &SeekTilesWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool), Media: pgstoretest.Media(pool), MediaDir: mediaDir, ScratchDir: t.TempDir(), runCmd: cmd.run}
	if err := runSeekTilesJob(t, w, recordingID); err != nil {
		t.Fatalf("Work() = %v, want success", err)
	}
	if want := []string{"0.000", "10.000", "20.000", "30.000", "39.007"}; !slices.Equal(cmd.seeks, want) {
		t.Errorf("seeks = %v, want %v", cmd.seeks, want)
	}
	if has, err := pgstoretest.Media(pool).HasActiveSeekTilesMediaAsset(context.Background(), recordingID); err != nil || !has {
		t.Errorf("seek_tiles row was not committed: present %v, err %v", has, err)
	}
}

// 長さは format ではなく映像ストリームから取る。映像が音声より早く終わる TS
// （映像 19.5 秒・format 22 秒）で format を使うと 3 枚になり、最後の 20 秒は
// 映像の終端を越える。
func TestSeekTilesWorker_UsesVideoStreamDuration(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	seedOriginalForSeekTiles(t, pool, mediaDir, recordingID, "shows/shortvideo.m2ts")

	cmd := &countingRunCmd{duration: "19.5", formatDuration: "22"}
	w := &SeekTilesWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool), Media: pgstoretest.Media(pool), MediaDir: mediaDir, ScratchDir: t.TempDir(), runCmd: cmd.run}
	if err := runSeekTilesJob(t, w, recordingID); err != nil {
		t.Fatalf("Work() = %v, want success", err)
	}
	if want := []string{"0.000", "10.000"}; !slices.Equal(cmd.seeks, want) {
		t.Errorf("seeks = %v, want %v", cmd.seeks, want)
	}
	if has, err := pgstoretest.Media(pool).HasActiveSeekTilesMediaAsset(context.Background(), recordingID); err != nil || !has {
		t.Errorf("seek_tiles row was not committed: present %v, err %v", has, err)
	}
}
