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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
)

// planEncodePublish は store の読み取りと共通の判定から、テストの前状態の公開計画を導出する。
func planEncodePublish(ctx context.Context, pool *pgxpool.Pool, recordingID int64, profile string, cut *cutContext, rebuildKeep ...[]chapters.Range) (encodePlan, error) {
	hasRebuildRequest := len(rebuildKeep) > 0
	var requestedKeep []chapters.Range
	if hasRebuildRequest {
		requestedKeep = rebuildKeep[0]
	}
	state, err := pgstoretest.Media(pool).GetEncodedPublishState(ctx, recordingID, profile, cut != nil, cut != nil && !hasRebuildRequest)
	if err != nil {
		return encodePlan{}, err
	}
	input := store.EncodedPublishInput{
		RecordingID:       recordingID,
		Profile:           profile,
		Cut:               cut != nil,
		HasRebuildRequest: hasRebuildRequest,
		RequestedKeep:     requestedKeep,
	}
	if cut != nil {
		input.KeepRanges = cut.keep
	}
	return planEncodePublishFromState(input, state), nil
}

// frozenKeepRanges は media_asset に凍結された keep 区間を store 契約から読む。
func frozenKeepRanges(ctx context.Context, pool *pgxpool.Pool, recordingID int64, profile string) ([]chapters.Range, error) {
	state, err := pgstoretest.Media(pool).GetEncodedPublishState(ctx, recordingID, profile, true, false)
	if err != nil {
		return nil, err
	}
	if state.EncodedAsset == nil {
		return nil, store.ErrNotFound
	}
	return state.EncodedAsset.KeepRanges, nil
}

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

// pauseBeforeEncodeObjectPublication は match した試行を、Put 完了後・DB 公開前で
// 止める。reached にはその試行の scratch 出力パスが届く。
func pauseBeforeEncodeObjectPublication(t *testing.T, match func(scratchOut string) bool) (reached <-chan string, release func()) {
	t.Helper()
	ch := make(chan string, 4)
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	orig := beforeEncodeObjectActivation
	t.Cleanup(func() {
		release()
		beforeEncodeObjectActivation = orig
	})
	beforeEncodeObjectActivation = func(scratchOut string) error {
		if !match(scratchOut) {
			return nil
		}
		ch <- scratchOut
		<-gate
		return nil
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

func jobWithID(recordingID int64) EncodeJobArgs {
	return EncodeJobArgs{RecordingID: recordingID, Profile: "h264"}
}

func inEncodeScratchAttempt(recordingID int64, profile string, attemptCount int32) func(string) bool {
	prefix := fmt.Sprintf("/encode/%s-%d-", encodeScratchScope(recordingID, profile), attemptCount)
	return func(scratchOut string) bool {
		return strings.Contains(filepath.ToSlash(scratchOut), prefix)
	}
}

func newH264Worker(pool *pgxpool.Pool, mediaDir, scratchDir, ffmpegPath string) *EncodeWorker {
	return &EncodeWorker{
		Publisher:       pgstoretest.Media(pool),
		ObjectPublisher: pgstoretest.Media(pool),
		Objects:         testFSObjectStore(mediaDir),
		Progress:        pgstoretest.Media(pool),
		Attempts:        pgstoretest.Media(pool),

		Media:      pgstoretest.Media(pool),
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

func beginEncodeAttemptForPublishTest(t *testing.T, pool *pgxpool.Pool, recordingID int64) int32 {
	return beginEncodeAttemptForProfilePublishTest(t, pool, recordingID, "h264")
}

func beginEncodeAttemptForProfilePublishTest(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile string) int32 {
	t.Helper()
	start, err := (&EncodeWorker{Publisher: pgstoretest.Media(pool), Progress: pgstoretest.Media(pool), Attempts: pgstoretest.Media(pool), Media: pgstoretest.Media(pool)}).beginEncodeAttempt(context.Background(), recordingID, profile)
	if err != nil {
		t.Fatal(err)
	}
	return start.Count
}

func assertNoEncodeTemp(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), mediapath.EncodeTempFilePrefix) || strings.HasPrefix(e.Name(), mediapath.GeneratedAssetTempFilePrefix) {
			t.Errorf("staged temp file was left behind: %s", e.Name())
		}
	}
}

// 派生物のキーは再利用しない。DB の行が無い既存オブジェクトも Put で上書きせず、
// active copy として公開しない。
func TestEncodeWorker_ExistingObjectKeyIsNotOverwritten(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ffmpegPath := installCountingFFmpeg(t)
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "20240101/t1.m2ts", []string{"h264"}, []byte("original payload t1"))

	finalPath := filepath.Join(mediaDir, "20240101", "t1_h264.mp4")
	previous := bytes.Repeat([]byte("PREVIOUS-CONTENT"), 1024)
	if err := os.WriteFile(finalPath, previous, 0o644); err != nil {
		t.Fatal(err)
	}

	w := newH264Worker(pool, mediaDir, scratchDir, ffmpegPath)
	err := runWorkerViaJobQFake(context.Background(), w, jobWithID(recordingID))
	if err == nil {
		t.Fatal("Work succeeded while the derived object key was already occupied")
	}
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, previous) {
		t.Errorf("existing object = %d bytes, want previous %d bytes untouched", len(got), len(previous))
	}
	var activeCount int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_assets a
		JOIN media_asset_copies c ON c.media_asset_id = a.id
		WHERE a.recording_id = $1 AND a.kind = 'encoded' AND a.profile = 'h264' AND c.state = 'active'`, recordingID).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 0 {
		t.Errorf("active encoded copies = %d, want none after conflicting Put", activeCount)
	}
	var pendingCount int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_assets a
		JOIN media_asset_copies c ON c.media_asset_id = a.id
		WHERE a.recording_id = $1 AND a.kind = 'encoded' AND a.profile = 'h264' AND c.state = 'pending'`, recordingID).Scan(&pendingCount); err != nil {
		t.Fatal(err)
	}
	if pendingCount != 0 {
		t.Errorf("pending encoded copies = %d, want none after rejecting a pre-existing object key", pendingCount)
	}
}

// T2: B の古い attempt が Put した後に A が新しい世代を active にし、B が DB 公開へ
// 進む順序。B の pending object は active copy を上書きせず、古い attempt は公開されない。
func TestEncodeWorker_CommittedByAnotherAttemptIsNotOverwritten(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	ffmpegPath := installCountingFFmpeg(t)
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "20240101/t2.m2ts", []string{"h264"}, []byte("original payload t2"))

	// 削除済みの行（rel_path = target）から始める。行なしだと B は (a) で止まる。
	encRel := "20240101/t2_h264.mp4"
	assetID := seedEncodedAsset(t, pool, recordingID, "h264", encRel)
	dbtest.SetRecordingMediaAssetState(t, pool, recordingID, store.AssetKindEncoded, "deleted")

	reached, release := pauseBeforeEncodeObjectPublication(t, inEncodeScratchAttempt(recordingID, "h264", 1))

	w := newH264Worker(pool, mediaDir, scratchDir, ffmpegPath)
	doneB := make(chan error, 1)
	go func() { doneB <- runWorkerViaJobQFake(ctx, w, jobWithID(recordingID)) }()
	waitReached(t, reached, doneB)

	if err := runWorkerViaJobQFake(ctx, w, jobWithID(recordingID)); err != nil {
		t.Fatalf("Work A: %v", err)
	}
	activeRelPath := currentCopyRelPath(t, pool, assetID)
	finalPath := filepath.Join(mediaDir, filepath.FromSlash(activeRelPath))
	contentA, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(contentA, []byte("attempt-2:")) {
		t.Fatalf("A's canonical = %q, want attempt-2 content", contentA)
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
	if err := pool.QueryRow(ctx, `SELECT a.size_bytes FROM media_assets a
		JOIN media_asset_copies c ON c.media_asset_id = a.id AND c.store = $2
		WHERE a.recording_id = $1 AND a.kind = 'encoded' AND a.profile = 'h264' AND c.state = 'active'`,
		recordingID, dbtest.StoreName).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if size != int64(len(contentA)) {
		t.Errorf("size_bytes = %d, want A's %d", size, len(contentA))
	}
	assertMediaAssetSHA256(t, pool, recordingID, store.AssetKindEncoded, "h264", contentA)
	assertNoEncodeTemp(t, filepath.Dir(finalPath))
}

// 別の録画・プロファイルが同じ rel_path を導くことがある（`r_a` + `b` と `r` + `a_b`）。
// 既存 object key は pending reservation の前に拒み、通常の Work の経路はこの拒否を
// 失敗した試行として記録する。
func TestEncodeWorker_ConflictingActiveRelPathDoesNotOverwrite(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	ffmpegPath := installCountingFFmpeg(t)
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	ownerID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "20240101/r_a.m2ts", []string{"b"}, []byte("owner original"))
	candidateID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "20240101/r.m2ts", []string{"a_b"}, []byte("candidate original"))
	const sharedRelPath = "20240101/r_a_b.mp4"
	if got, err := EncodedRelPath("20240101/r.m2ts", "a_b", "mp4", 0, false); err != nil || got != sharedRelPath {
		t.Fatalf("candidate EncodedRelPath = %q, err %v; want %q", got, err, sharedRelPath)
	}
	const previous = "owner encoded bytes"
	finalPath := filepath.Join(mediaDir, filepath.FromSlash(sharedRelPath))
	if err := os.WriteFile(finalPath, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}
	ownerAssetID := seedEncodedAsset(t, pool, ownerID, "b", sharedRelPath)

	w := newH264Worker(pool, mediaDir, scratchDir, ffmpegPath)
	w.Profiles.Profiles[0].Name = "a_b"
	err := runWorkerViaJobQFake(ctx, w, EncodeJobArgs{RecordingID: candidateID, Profile: "a_b"})
	if err == nil {
		t.Fatal("Work succeeded with another active asset owning the rel_path")
	}

	got, readErr := os.ReadFile(finalPath)
	if readErr != nil || string(got) != previous {
		t.Errorf("existing canonical = %q, err %v; want %q untouched", got, readErr, previous)
	}
	var ownerIDAfter int64
	var ownerProfile, ownerState string
	if err := pool.QueryRow(ctx, `SELECT a.recording_id, a.profile, c.state FROM media_assets a JOIN media_asset_copies c ON c.media_asset_id = a.id WHERE a.id = $1`, ownerAssetID).
		Scan(&ownerIDAfter, &ownerProfile, &ownerState); err != nil {
		t.Fatal(err)
	}
	if ownerIDAfter != ownerID || ownerProfile != "b" || ownerState != "active" {
		t.Errorf("existing asset = (%d, %q, %q), want (%d, %q, active)", ownerIDAfter, ownerProfile, ownerState, ownerID, "b")
	}
	var candidateAssetCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'encoded' AND profile = 'a_b'`, candidateID).
		Scan(&candidateAssetCount); err != nil {
		t.Fatal(err)
	}
	if candidateAssetCount != 0 {
		t.Errorf("candidate encoded assets = %d, want 0 after failed publication", candidateAssetCount)
	}
	state, ok := encodeAttemptState(t, pool, candidateID, "a_b")
	if !ok || state != "failed" {
		t.Errorf("candidate attempt state = %q, exists %v; want failed", state, ok)
	}
	var attemptError *string
	if err := pool.QueryRow(ctx, `SELECT error FROM recording_encode_attempts WHERE recording_id = $1 AND profile = 'a_b'`, candidateID).
		Scan(&attemptError); err != nil {
		t.Fatal(err)
	}
	if attemptError == nil {
		t.Fatal("candidate attempt error is NULL, want the existing object key failure")
	}
	if !strings.Contains(*attemptError, "already occupied") ||
		!strings.Contains(*attemptError, "object key already exists") {
		t.Errorf("candidate attempt error = %s, want the existing object key failure", *attemptError)
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
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "20240101/t3.m2ts", []string{"cut"}, []byte("original payload t3"))
	if _, err := pool.Exec(ctx, `UPDATE recordings SET program_duration_ms = 2000 WHERE id = $1`, recordingID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, recordingID); err != nil {
		t.Fatal(err)
	}
	cutKeep, owned, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, recordingID)
	if err != nil || !owned || len(cutKeep) == 0 {
		t.Fatalf("currentCutKeep = %v, owned %v, err %v", cutKeep, owned, err)
	}
	attempt := beginEncodeAttemptForProfilePublishTest(t, pool, recordingID, "cut")

	g1, g2, g3 := "20240101/t3_cut.g1.mp4", "20240101/t3_cut.g2.mp4", "20240101/t3_cut.g3.mp4"
	abs := func(rel string) string { return filepath.Join(mediaDir, filepath.FromSlash(rel)) }
	assetID := seedEncodedAsset(t, pool, recordingID, "cut", g1)
	if err := setFrozenCuts(t, pool, assetID, []chapters.Range{{StartMs: 0, EndMs: 1000}}); err != nil {
		t.Fatal(err)
	}
	seedEncodedAssetRebuildRequest(t, pool, recordingID, cutKeep)

	cut := &cutContext{keep: cutKeep}
	observed, err := planEncodePublish(ctx, pool, recordingID, "cut", cut, cutKeep)
	if err != nil {
		t.Fatal(err)
	}
	if observed.skip || observed.generation != 2 {
		t.Fatalf("plan = %+v, want generation 2 and no skip", observed)
	}

	scratchOut := filepath.Join(scratchDir, "out.mp4")
	if err := os.WriteFile(scratchOut, []byte("X output"), 0o644); err != nil {
		t.Fatal(err)
	}
	reached, release := pauseBeforeEncodeObjectPublication(t, func(string) bool { return true })
	w := newH264Worker(pool, mediaDir, scratchDir, "")
	type result struct {
		published bool
		err       error
	}
	res := make(chan result, 1)
	never := make(chan error)
	go func() {
		_, published, err := w.publishEncoded(ctx, encodePublishInput{
			recordingID: recordingID, profile: "cut", attempt: attempt, relPath: g2,
			scratchOut: scratchOut, observed: observed, cut: cut, expectedKeep: cutKeep,
		})
		res <- result{published, err}
	}()
	waitReached(t, reached, never)

	// 別の実行が g3 へ進めた状態（区間も X と違う）。
	if err := os.WriteFile(abs(g3), []byte("g3 output"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbtest.ReplaceDerivedMediaAssetCopy(t, pool, assetID, dbtest.StoreName, g3, 3)
	if err := setFrozenCuts(t, pool, assetID, []chapters.Range{{StartMs: 0, EndMs: 500}}); err != nil {
		t.Fatal(err)
	}
	if rowState != "active" {
		dbtest.SetMediaAssetState(t, pool, assetID, rowState)
	}

	release()
	r := <-res
	if r.published {
		t.Fatalf("publishEncoded published over a newer row")
	}
	if rowState == "active" {
		if _, ok := jobq.SnoozeDelay(r.err); !ok {
			t.Fatalf("publishEncoded err = %v, want jobq snooze (replan via snooze, not success)", r.err)
		}
	} else if r.err != nil {
		t.Fatalf("publishEncoded err = %v, want a successful skip", r.err)
	}
	var rel string
	if err := pool.QueryRow(ctx, `SELECT rel_path FROM media_asset_copies WHERE media_asset_id = $1 ORDER BY generation DESC, id DESC LIMIT 1`, assetID).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	if rel != g3 {
		t.Errorf("row rel_path = %s, want %s (the row must not be rewound)", rel, g3)
	}
	if _, err := os.Stat(abs(g3)); err != nil {
		t.Errorf("g3 file is missing: %v", err)
	}
	if _, err := os.Stat(abs(g2)); err != nil {
		t.Errorf("g2 pending object is missing after Put (err=%v)", err)
	}
	if rowState == "active" && currentCopyRelPath(t, pool, assetID) != g3 {
		t.Errorf("active copy path = %q, want newer path %q", currentCopyRelPath(t, pool, assetID), g3)
	}
	assertNoEncodeTemp(t, filepath.Dir(abs(g2)))
}

// rebuildPublishScene は、g1（古い凍結区間）が active で、押した時点の keep で作った
// 出力を公開する直前の状態。
type rebuildPublishScene struct {
	recordingID   int64
	assetID       int64
	requestedKeep []chapters.Range
	finalRel      string
	finalPath     string
	in            encodePublishInput
	w             *EncodeWorker
}

func newRebuildPublishScene(t *testing.T, pool *pgxpool.Pool, name string) rebuildPublishScene {
	t.Helper()
	ctx := context.Background()
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "rebuild/"+name+".m2ts", []string{"cut"}, []byte("original"))
	if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, recordingID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(600000, 900000), 'CM', true)`, recordingID); err != nil {
		t.Fatal(err)
	}
	requestedKeep, owned, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, recordingID)
	if err != nil || !owned || len(requestedKeep) == 0 {
		t.Fatalf("currentCutKeep = %v, owned %v, err %v", requestedKeep, owned, err)
	}
	assetID := seedEncodedAsset(t, pool, recordingID, "cut", "rebuild/"+name+"_cut.g1.mp4")
	if err := setFrozenCuts(t, pool, assetID, []chapters.Range{{StartMs: 0, EndMs: 1800000}}); err != nil {
		t.Fatal(err)
	}
	seedEncodedAssetRebuildRequest(t, pool, recordingID, requestedKeep)
	cut := &cutContext{keep: requestedKeep}
	observed, err := planEncodePublish(ctx, pool, recordingID, "cut", cut, requestedKeep)
	if err != nil || observed.skip {
		t.Fatalf("observed plan = %+v, err %v; want a replacement", observed, err)
	}
	attempt := beginEncodeAttemptForProfilePublishTest(t, pool, recordingID, "cut")
	scratchOut := filepath.Join(scratchDir, "rebuild.mp4")
	if err := os.WriteFile(scratchOut, []byte("rebuilt cut"), 0o644); err != nil {
		t.Fatal(err)
	}
	finalRel := "rebuild/" + name + "_cut.g2.mp4"
	finalPath := filepath.Join(mediaDir, filepath.FromSlash(finalRel))
	return rebuildPublishScene{
		recordingID: recordingID, assetID: assetID, requestedKeep: requestedKeep,
		finalRel: finalRel, finalPath: finalPath,
		w: newH264Worker(pool, mediaDir, scratchDir, ""),
		in: encodePublishInput{
			recordingID:  recordingID,
			profile:      "cut",
			attempt:      attempt,
			relPath:      finalRel,
			scratchOut:   scratchOut,
			observed:     observed,
			cut:          cut,
			expectedKeep: requestedKeep,
		},
	}
}

// unsatisfiedRebuildCount は unsatisfied_encode_rebuilds の録画の行数を返す。
func unsatisfiedRebuildCount(t *testing.T, pool *pgxpool.Pool, recordingID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM unsatisfied_encode_rebuilds WHERE recording_id = $1`, recordingID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestEncodeWorker_PublishKeepsRebuildRequestAndItIsSatisfied は、公開後も要求行が
// 残り、凍結区間との一致で満たされたとみなされる（未充足の view から外れる）ことを固定する。
func TestEncodeWorker_PublishKeepsRebuildRequestAndItIsSatisfied(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	sc := newRebuildPublishScene(t, pool, "publish")
	if got := unsatisfiedRebuildCount(t, pool, sc.recordingID); got != 1 {
		t.Fatalf("unsatisfied rebuilds before publish = %d, want 1", got)
	}
	if _, published, err := sc.w.publishEncoded(ctx, sc.in); err != nil || !published {
		t.Fatalf("publish = %v, err %v; want committed replacement", published, err)
	}
	if !rebuildRequestExists(t, pool, sc.recordingID) {
		t.Fatal("rebuild request row after publish is missing; want it kept (only api removes it)")
	}
	if got := unsatisfiedRebuildCount(t, pool, sc.recordingID); got != 0 {
		t.Fatalf("unsatisfied rebuilds after publish = %d, want 0 (satisfied)", got)
	}
	asset, err := sqlcgen.New(pool).GetEncodedMediaAssetForProfile(ctx, sqlcgen.GetEncodedMediaAssetForProfileParams{
		RecordingID: sc.recordingID,
		Profile:     stringPtr("cut"),
	})
	if err != nil || asset.ID != sc.assetID || currentCopyRelPath(t, pool, asset.ID) != sc.finalRel {
		t.Fatalf("encoded asset after publish = %+v, err %v; want id %d at %s", asset, err, sc.assetID, sc.finalRel)
	}
	frozen, err := frozenKeepRanges(ctx, pool, sc.recordingID, "cut")
	if err != nil || !chapters.SameRanges(frozen, sc.requestedKeep) {
		t.Fatalf("frozen keep after publish = %v, err %v; want %v", frozen, err, sc.requestedKeep)
	}
	if _, err := os.Stat(sc.finalPath); err != nil {
		t.Errorf("published output missing: %v", err)
	}
}

// TestEncodeWorker_PublishUsesRequestedKeepWhenChaptersChangeAfterRebuildRequest は、
// 押した後にチャプターを編集しても、押した時点の keep で公開されることを固定する。
func TestEncodeWorker_PublishUsesRequestedKeepWhenChaptersChangeAfterRebuildRequest(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	sc := newRebuildPublishScene(t, pool, "edited-before-publish")
	if _, err := pool.Exec(ctx, `UPDATE recording_chapter_spans SET span = int8range(500000, 900000) WHERE recording_id = $1`, sc.recordingID); err != nil {
		t.Fatalf("editing chapters before publish: %v", err)
	}
	if _, published, err := sc.w.publishEncoded(ctx, sc.in); err != nil || !published {
		t.Fatalf("publish = %v, err %v; want the requested keep published despite the edit", published, err)
	}
	frozen, err := frozenKeepRanges(ctx, pool, sc.recordingID, "cut")
	if err != nil || !chapters.SameRanges(frozen, sc.requestedKeep) {
		t.Fatalf("frozen keep = %v, err %v; want the requested %v", frozen, err, sc.requestedKeep)
	}
	if got := unsatisfiedRebuildCount(t, pool, sc.recordingID); got != 0 {
		t.Errorf("unsatisfied rebuilds after publish = %d, want 0", got)
	}
}

// TestEncodeWorker_PublishSkipsWhenRequestIsPressedAgain は、押し直しで requested_keep が
// 上書きされていれば、古い keep のジョブが公開しないことを固定する。
func TestEncodeWorker_PublishSkipsWhenRequestIsPressedAgain(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	sc := newRebuildPublishScene(t, pool, "pressed-again")
	seedEncodedAssetRebuildRequest(t, pool, sc.recordingID, []chapters.Range{{StartMs: 0, EndMs: 2000}})
	if _, published, err := sc.w.publishEncoded(ctx, sc.in); err != nil || published {
		t.Fatalf("publish = %v, err %v; want a skip for the overwritten request", published, err)
	}
	if _, err := os.Stat(sc.finalPath); err != nil {
		t.Errorf("pending output stat err = %v, want the unexposed Put result", err)
	}
	var pendingState string
	if err := pool.QueryRow(ctx, `SELECT c.state FROM media_asset_copies c WHERE c.store = $1 AND c.rel_path = $2`,
		dbtest.StoreName, sc.finalRel).Scan(&pendingState); err != nil {
		t.Fatalf("reading skipped output state: %v", err)
	}
	if pendingState != "pending" {
		t.Errorf("skipped output state = %q, want pending", pendingState)
	}
	asset, err := sqlcgen.New(pool).GetEncodedMediaAssetForProfile(ctx, sqlcgen.GetEncodedMediaAssetForProfileParams{
		RecordingID: sc.recordingID,
		Profile:     stringPtr("cut"),
	})
	if err != nil || asset.ID != sc.assetID || currentCopyRelPath(t, pool, asset.ID) != "rebuild/pressed-again_cut.g1.mp4" {
		t.Fatalf("encoded asset after skip = %+v, err %v; want the previous generation", asset, err)
	}
	if got := unsatisfiedRebuildCount(t, pool, sc.recordingID); got != 1 {
		t.Errorf("unsatisfied rebuilds = %d, want 1 (the new request is still pending)", got)
	}
}

// TestEncodeWorker_InitialCutPublishChecksCurrentChapters は、要求に由来しない初回の
// カット版が、エンコード中にチャプターが編集されたら公開しないことを固定する
// （公開すると編集前の版が desired を満たし、until_encoded の原本が消える）。
// 編集が無ければ公開される（逆方向）。
func TestEncodeWorker_InitialCutPublishChecksCurrentChapters(t *testing.T) {
	for _, tc := range []struct {
		name          string
		edit          bool
		wantPublished bool
	}{
		{"edited-during-encode", true, false},
		{"not-edited", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := setupTestPool(t)
			if pool == nil {
				return
			}
			ctx := context.Background()
			sc := newRebuildPublishScene(t, pool, "initial-"+tc.name)
			// 初回のジョブ: active な encoded も要求行も無い。
			if _, err := pool.Exec(ctx, `DELETE FROM media_assets WHERE id = $1`, sc.assetID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `DELETE FROM encoded_asset_rebuild_requests WHERE recording_id = $1`, sc.recordingID); err != nil {
				t.Fatal(err)
			}
			observed, err := planEncodePublish(ctx, pool, sc.recordingID, "cut", sc.in.cut)
			if err != nil {
				t.Fatal(err)
			}
			in := sc.in
			in.expectedKeep = nil
			in.observed = observed
			if tc.edit {
				if _, err := pool.Exec(ctx, `UPDATE recording_chapter_spans SET span = int8range(500000, 900000) WHERE recording_id = $1`, sc.recordingID); err != nil {
					t.Fatal(err)
				}
			}
			if _, published, err := sc.w.publishEncoded(ctx, in); err != nil || published != tc.wantPublished {
				t.Fatalf("publish = %v, err %v; want published=%v", published, err, tc.wantPublished)
			}
		})
	}
}

func TestEncodeWorker_LateAttemptCannotPublish(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "20240101/late-attempt.m2ts", []string{"h264"}, []byte("original"))
	w := newH264Worker(pool, mediaDir, scratchDir, "")
	oldAttempt, err := w.beginEncodeAttempt(ctx, recordingID, "h264")
	if err != nil {
		t.Fatal(err)
	}
	newAttempt, err := w.beginEncodeAttempt(ctx, recordingID, "h264")
	if err != nil {
		t.Fatal(err)
	}
	if oldAttempt.Count != 1 || newAttempt.Count != 2 {
		t.Fatalf("attempt counts = %d then %d, want 1 then 2", oldAttempt.Count, newAttempt.Count)
	}

	relPath := "20240101/late-attempt_h264.mp4"
	scratchOut := filepath.Join(scratchDir, "old-attempt.mp4")
	if err := os.WriteFile(scratchOut, []byte("old attempt output"), 0o644); err != nil {
		t.Fatal(err)
	}
	observed, err := planEncodePublish(ctx, pool, recordingID, "h264", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, published, err := w.publishEncoded(ctx, encodePublishInput{
		recordingID: recordingID,
		profile:     "h264",
		attempt:     oldAttempt.Count,
		relPath:     relPath,
		scratchOut:  scratchOut,
		observed:    observed,
	}); err != nil || published {
		t.Fatalf("old attempt publish = published %v, err %v; want fenced skip", published, err)
	}
	var activeCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_assets a JOIN media_asset_copies c ON c.media_asset_id = a.id
		WHERE a.recording_id = $1 AND a.kind = 'encoded' AND a.profile = 'h264' AND c.state = 'active'`, recordingID).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 0 {
		t.Errorf("active encoded copies after stale attempt = %d, want none", activeCount)
	}
	if count, ok := encodeAttemptCount(t, pool, recordingID, "h264"); !ok || count != newAttempt.Count {
		t.Errorf("attempt_count after stale publish = %d, ok=%v, want %d", count, ok, newAttempt.Count)
	}
}

// snooze（計画のやり直し）は失敗ではないので試行に数えない。
func TestShouldCountEncodeFailure_SnoozeIsNotAFailure(t *testing.T) {
	if shouldCountEncodeFailure(fmt.Errorf("wrapped: %w", jobq.Snooze(time.Second)), nil) {
		t.Error("a snooze must not count as an encode failure")
	}
	if !shouldCountEncodeFailure(errors.New("boom"), nil) {
		t.Error("a real failure must count")
	}
}

// T4: scratch は recording/profile/domain attempt ごと。別の実行中 attempt の scratch は
// 各試行が自分の scratch dir に持つ flock により、cleanup は実行中の試行の dir を消さず、先行 attempt が終わるまで残る。
func TestEncodeWorker_ConcurrentScratchCleanupSkipsActiveAttempts(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	ffmpegPath := installCountingFFmpeg(t)
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "20240101/t4.m2ts", []string{"h264"}, []byte("original payload t4"))

	var first atomic.Bool
	reached, release := pauseBeforeEncodeObjectPublication(t, func(string) bool { return first.CompareAndSwap(false, true) })
	w := newH264Worker(pool, mediaDir, scratchDir, ffmpegPath)
	doneB := make(chan error, 1)
	go func() { doneB <- runWorkerViaJobQFake(ctx, w, jobWithID(recordingID)) }()
	waitReached(t, reached, doneB)

	if err := runWorkerViaJobQFake(ctx, w, jobWithID(recordingID)); err != nil {
		t.Fatalf("Work A: %v", err)
	}
	bScratchPattern := filepath.Join(scratchDir, "encode", encodeScratchScope(recordingID, "h264")+"-1-*", "out.mp4")
	bScratch, err := filepath.Glob(bScratchPattern)
	if err != nil {
		t.Fatalf("finding B's scratch output: %v", err)
	}
	if len(bScratch) != 1 {
		t.Fatalf("B's scratch outputs = %v, want exactly one attempt-specific output", bScratch)
	}
	if _, err := os.Stat(bScratch[0]); err != nil {
		t.Errorf("B's scratch output is gone after A finished: %v", err)
	}
	release()
	if err := waitDone(t, doneB); err != nil {
		t.Fatalf("Work B: %v", err)
	}
	bScratch, err = filepath.Glob(filepath.Join(scratchDir, "encode", encodeScratchScope(recordingID, "h264")+"-1-*"))
	if err != nil || len(bScratch) != 0 {
		t.Errorf("B's scratch was not cleaned up: paths=%v err=%v", bScratch, err)
	}
}

// Put が完了してから DB transaction が active copy を公開することを固定する。
func TestEncodeWorker_PutPrecedesActiveCommit(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "20240101/put-order.m2ts", []string{"h264"}, []byte("original"))
	attempt := beginEncodeAttemptForPublishTest(t, pool, recordingID)
	observed, err := planEncodePublish(ctx, pool, recordingID, "h264", nil)
	if err != nil {
		t.Fatal(err)
	}
	relPath, err := EncodedRelPath("20240101/put-order.m2ts", "h264", "mp4", int(observed.generation), false)
	if err != nil {
		t.Fatal(err)
	}
	scratchOut := filepath.Join(scratchDir, "out.mp4")
	if err := os.WriteFile(scratchOut, []byte("encoded bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	worker := newH264Worker(pool, mediaDir, scratchDir, "")
	originalCommitHook := beforeEncodeCommit
	t.Cleanup(func() { beforeEncodeCommit = originalCommitHook })
	beforeEncodeCommit = func(string) error {
		if _, err := worker.Objects.Head(ctx, relPath); err != nil {
			return fmt.Errorf("object is not durable before DB commit: %w", err)
		}
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, dbtest.StoreName, relPath).Scan(&state); err != nil {
			return err
		}
		if state != "pending" {
			return fmt.Errorf("copy state before DB commit = %q, want pending", state)
		}
		return nil
	}
	if _, published, err := worker.publishEncoded(ctx, encodePublishInput{
		recordingID: recordingID, profile: "h264", attempt: attempt,
		relPath: relPath, scratchOut: scratchOut, observed: observed,
	}); err != nil || !published {
		t.Fatalf("publishEncoded = published %t, err %v; want committed publication", published, err)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, dbtest.StoreName, relPath).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "active" {
		t.Fatalf("copy state after commit = %q, want active", state)
	}
}

// TestEncodeWorker_PublishSkipLeavesSatisfiedRebuildRequest は、観測後に別の公開が
// 要求の keep を満たす版へ置き換えていたとき、公開 tx が何も公開せず、要求行にも
// 触れない（行は残り、満たされたとみなされる）ことを固定する。
func TestEncodeWorker_PublishSkipLeavesSatisfiedRebuildRequest(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "rebuild/txskip.m2ts", []string{"cut"}, []byte("original"))
	if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, recordingID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(600000, 900000), 'CM', true)`, recordingID); err != nil {
		t.Fatal(err)
	}
	keep, owned, err := pgstoretest.Media(pool).GetCurrentCutKeep(ctx, recordingID)
	if err != nil || !owned || len(keep) == 0 {
		t.Fatalf("currentCutKeep = %v, owned %v, err %v", keep, owned, err)
	}
	assetID := seedEncodedAsset(t, pool, recordingID, "cut", "rebuild/txskip_cut.g1.mp4")
	if err := setFrozenCuts(t, pool, assetID, []chapters.Range{{StartMs: 0, EndMs: 1800000}}); err != nil {
		t.Fatal(err)
	}
	seedEncodedAssetRebuildRequest(t, pool, recordingID, keep)
	cut := &cutContext{keep: keep}
	observed, err := planEncodePublish(ctx, pool, recordingID, "cut", cut, keep)
	if err != nil || observed.skip {
		t.Fatalf("observed plan = %+v, err %v; want a replacement", observed, err)
	}
	attempt := beginEncodeAttemptForProfilePublishTest(t, pool, recordingID, "cut")
	// 観測の後に、別の公開が要求どおりの版へ置き換えた状況を作る。
	dbtest.ReplaceDerivedMediaAssetCopy(t, pool, assetID, dbtest.StoreName, "rebuild/txskip_cut.g2.mp4", 2)
	if err := setFrozenCuts(t, pool, assetID, keep); err != nil {
		t.Fatal(err)
	}
	scratchOut := filepath.Join(scratchDir, "rebuild.mp4")
	if err := os.WriteFile(scratchOut, []byte("late"), 0o644); err != nil {
		t.Fatal(err)
	}
	finalRel := "rebuild/txskip_cut.g2.mp4"
	w := newH264Worker(pool, mediaDir, scratchDir, "")
	if _, published, err := w.publishEncoded(ctx, encodePublishInput{
		recordingID:  recordingID,
		profile:      "cut",
		attempt:      attempt,
		relPath:      finalRel,
		scratchOut:   scratchOut,
		observed:     observed,
		cut:          cut,
		expectedKeep: keep,
	}); err != nil || published {
		t.Fatalf("publish = %v, err %v; want a skip", published, err)
	}
	if !rebuildRequestExists(t, pool, recordingID) {
		t.Fatal("rebuild request row after satisfied skip is missing; want it kept")
	}
	if got := unsatisfiedRebuildCount(t, pool, recordingID); got != 0 {
		t.Fatalf("unsatisfied rebuilds after satisfied skip = %d, want 0", got)
	}
}

// currentCopyRelPath は論理資産の現行世代の active コピーのパスを返す。無ければ空文字列を返す。
func currentCopyRelPath(t *testing.T, pool *pgxpool.Pool, assetID int64) string {
	t.Helper()
	var relPath string
	err := pool.QueryRow(context.Background(), `SELECT rel_path FROM active_media_asset_copies($1, NULL) LIMIT 1`, assetID).Scan(&relPath)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reading the current copy of asset %d: %v", assetID, err)
	}
	return relPath
}
