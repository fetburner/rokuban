package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

type testCMDetectReconcileWorker struct {
	river.WorkerDefaults[jobs.CMDetectReconcileArgs]
}

func (testCMDetectReconcileWorker) Work(context.Context, *river.Job[jobs.CMDetectReconcileArgs]) error {
	return nil
}

type testCMLogoCandidateWorker struct {
	river.WorkerDefaults[jobs.CMLogoCandidateJobArgs]
}

func (testCMLogoCandidateWorker) Work(context.Context, *river.Job[jobs.CMLogoCandidateJobArgs]) error {
	return nil
}

func waitForTestRiverJobCompletion(t *testing.T, events <-chan *river.Event, kind string) {
	t.Helper()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event != nil && event.Job != nil && event.Job.Kind == kind {
				return
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s completion", kind)
		}
	}
}

func startCMDetectionAttemptForTest(t *testing.T, ctx context.Context, q *sqlcgen.Queries, recordingID int64) int32 {
	t.Helper()
	attempt, err := q.BeginCMDetectionAttempt(ctx, sqlcgen.BeginCMDetectionAttemptParams{
		RecordingID: recordingID, MaxAttempts: 3,
	})
	if err != nil || !attempt.ShouldRun {
		t.Fatalf("BeginCMDetectionAttempt(%d) = %#v, %v", recordingID, attempt, err)
	}
	return attempt.AttemptCount
}

func markCMDetectionFailureForTest(t *testing.T, ctx context.Context, q *sqlcgen.Queries, recordingID int64, attemptCount int32, stage, message *string) {
	t.Helper()
	n, err := q.MarkCMDetectionFailure(ctx, sqlcgen.MarkCMDetectionFailureParams{
		RecordingID: recordingID, AttemptCount: attemptCount,
		State: "failed", Stage: stage, Error: message,
	})
	if err != nil || n != 1 {
		t.Fatalf("MarkCMDetectionFailure(%d, %d) = %d, %v", recordingID, attemptCount, n, err)
	}
}

func patchCMDetection(t *testing.T, url string, enabled bool) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]bool{"cmDetect": enabled})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestSetRecordingEncodePolicy_CMDetectionFreezesOnlyWithActiveOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, CMDetectEnabled: true}))
	defer srv.Close()

	activeID := seedRecording(t, pool, "復旧原本", time.Now().Truncate(time.Second), "finished", 980)
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: activeID,
		Kind:        db.AssetKindOriginal,
		RelPath:     fmt.Sprintf("test/%d.ts", activeID),
		SizeBytes:   1000,
	}); err != nil {
		t.Fatal(err)
	}
	resp := patchCMDetection(t, encodePolicyURL(srv.URL, activeID), true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("enable status = %d, want 204", resp.StatusCode)
	}
	var keepOriginal string
	var profiles []string
	var cmDetect bool
	if err := pool.QueryRow(context.Background(), `
		SELECT keep_original, encode_profiles, cm_detect
		FROM recording_encode_policy WHERE recording_id = $1`, activeID,
	).Scan(&keepOriginal, &profiles, &cmDetect); err != nil {
		t.Fatal(err)
	}
	if keepOriginal != "always" || len(profiles) != 0 || !cmDetect {
		t.Fatalf("frozen policy = (%q, %v, %v), want (always, [], true)", keepOriginal, profiles, cmDetect)
	}

	noOriginalID := seedRecording(t, pool, "原本なし", time.Now().Add(time.Second).Truncate(time.Second), "finished", 981)
	resp = patchCMDetection(t, encodePolicyURL(srv.URL, noOriginalID), true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("enable without original status = %d, want 409", resp.StatusCode)
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recording_encode_policy WHERE recording_id = $1`, noOriginalID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("policy rows without original = %d, want 0", count)
	}

	resp = patchCMDetection(t, encodePolicyURL(srv.URL, noOriginalID), false)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disable before ingest status = %d, want 204", resp.StatusCode)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recording_encode_policy WHERE recording_id = $1`, noOriginalID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("false PATCH froze a policy row: %d", count)
	}
}

func TestRetryRecordingCMDetectionClearsFailureAndEnqueues(t *testing.T) {
	pool := testutil.SetupDB(t)
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, CMDetectEnabled: true, RiverClient: riverClient}))
	defer srv.Close()

	id := seedRecording(t, pool, "再試行", time.Now().Truncate(time.Second), "finished", 982)
	if _, err := pool.Exec(context.Background(), `
		UPDATE recordings SET started_at = '2025-01-01T00:00:00Z', ended_at = '2025-01-01T00:45:00Z'
		WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id,
		Kind:        db.AssetKindOriginal,
		RelPath:     fmt.Sprintf("test/%d.ts", id),
		SizeBytes:   1000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
		VALUES ($1, 'always', '{}', true)`, id); err != nil {
		t.Fatal(err)
	}
	q := sqlcgen.New(pool)
	attemptCount := startCMDetectionAttemptForTest(t, context.Background(), q, id)
	markCMDetectionFailureForTest(t, context.Background(), q, id, attemptCount, nil, nil)
	if err := sqlcgen.New(pool).SaveCMDetection(context.Background(), sqlcgen.SaveCMDetectionParams{RecordingID: id, CmRanges: "{}"}); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/recordings/%d/cm-detection/retry", srv.URL, id), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		body := decodeErrorResponse(t, resp)
		t.Fatalf("retry status = %d (%s), want 204", resp.StatusCode, body.Error)
	}
	var detections, attempts int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recording_cm_detections WHERE recording_id = $1`, id).Scan(&detections); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	detectArgs := testutil.RequireRiverInserted(context.Background(), t, pool, jobs.CMDetectJobArgs{}, nil).Args
	if detections != 0 || attempts != 0 {
		t.Fatalf("after retry: detections=%d attempts=%d; want 0/0", detections, attempts)
	}
	if detectArgs.RecordingID != id || detectArgs.RecordingDurationMs != 45*60*1000 {
		t.Errorf("retry job args = %#v, want recording %d and 2700000ms", detectArgs, id)
	}
}

func TestCMLogoAPIListsFailuresAndForgetsLogo(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()

	id := seedRecording(t, pool, "ロゴ管理", time.Now().Truncate(time.Second), "finished", 983)
	q := sqlcgen.New(pool)
	if err := q.UpsertCMLogo(context.Background(), sqlcgen.UpsertCMLogoParams{
		NetworkID: 32678, ServiceID: 5168, Lgd: []byte("learned logo"),
		PreviewPng: []byte{1, 2, 3}, LearnedFrom: &id, CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}
	attemptCount := startCMDetectionAttemptForTest(t, context.Background(), q, id)
	stage := "logo"
	markCMDetectionFailureForTest(t, context.Background(), q, id, attemptCount, &stage, nil)

	resp, err := http.Get(srv.URL + "/api/cm-logos")
	if err != nil {
		t.Fatal(err)
	}
	var logos []CMLogoState
	if err := json.NewDecoder(resp.Body).Decode(&logos); err != nil {
		_ = resp.Body.Close()
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(logos) != 1 {
		t.Fatalf("logo list status=%d rows=%d; want 200/1", resp.StatusCode, len(logos))
	}
	if logos[0].State != CMLogoStateStateFailed || logos[0].FailedCount != 1 || logos[0].PendingCount != 0 ||
		logos[0].DetectedCount != 0 || logos[0].RedetectableCount != 0 || logos[0].Site != "default" ||
		logos[0].LearnedAt == nil || logos[0].PreviewPng == nil {
		t.Fatalf("logo state = %#v, want failed logo with preview", logos[0])
	}
	if logos[0].LastFailureStage == nil || *logos[0].LastFailureStage != stage {
		t.Fatalf("lastFailureStage = %v, want %q", logos[0].LastFailureStage, stage)
	}

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/cm-logos/32678/5168", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if _, err := q.GetCMLogo(context.Background(), sqlcgen.GetCMLogoParams{NetworkID: 32678, ServiceID: 5168}); err == nil {
		t.Fatal("logo still exists after DELETE")
	}
}

// 枠を教えると、学習済みロゴを残したまま候補解析を依頼する。
// 一覧は教えた枠と候補の無い待ち状態を返す（このテストは River 無し）。
func TestCMLogoAreaAPIForgetsLogoAndRequeuesFailures(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()
	ctx := context.Background()
	q := sqlcgen.New(pool)

	id := seedRecording(t, pool, "枠を教える", time.Now().Truncate(time.Second), "finished", 984)
	if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: id, Kind: db.AssetKindOriginal,
		RelPath: fmt.Sprintf("test/%d.ts", id), SizeBytes: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
		VALUES ($1, 'always', '{}', true)`, id); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertCMLogo(ctx, sqlcgen.UpsertCMLogoParams{
		NetworkID: 32678, ServiceID: 5168, Lgd: []byte("learned outside the area"), LearnedFrom: &id,
		CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}
	attemptCount := startCMDetectionAttemptForTest(t, ctx, q, id)
	message := "CM detection for recording 1: logoframe: no logo found"
	markCMDetectionFailureForTest(t, ctx, q, id, attemptCount, nil, &message)
	// 失敗した試行は、枠を教える前は候補ではない（前回の枠で既に試している）。
	desired, err := q.IsCMDetectionDesired(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if desired {
		t.Fatal("the recording is already a candidate before the area is taught")
	}

	// 枠の外を通す保存は 400。
	for _, body := range []string{
		fmt.Sprintf(`{"x":1300,"y":24,"w":240,"h":96,"codedWidth":1440,"codedHeight":1080,"recordingId":%d}`, id),
		fmt.Sprintf(`{"x":0,"y":0,"w":0,"h":96,"codedWidth":1440,"codedHeight":1080,"recordingId":%d}`, id),
	} {
		req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/cm-logos/32678/5168/area",
			strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("PUT %s: status = %d, want 400", body, resp.StatusCode)
		}
	}

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/cm-logos/32678/5168/area",
		strings.NewReader(fmt.Sprintf(`{"x":1180,"y":24,"w":240,"h":96,"codedWidth":1440,"codedHeight":1080,"recordingId":%d}`, id)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204", resp.StatusCode)
	}
	if _, err := q.GetCMLogo(ctx, sqlcgen.GetCMLogoParams{NetworkID: 32678, ServiceID: 5168}); err != nil {
		t.Errorf("the learned logo was removed before candidate adoption: %v", err)
	}
	area, err := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: 32678, ServiceID: 5168})
	if err != nil {
		t.Fatalf("taught area: %v", err)
	}
	if area.X != 1180 || area.Y != 24 || area.W != 240 || area.H != 96 ||
		area.CodedWidth != 1440 || area.CodedHeight != 1080 {
		t.Errorf("stored area = %#v, want 1180,24 240x96 at 1440x1080", area)
	}
	desired, err = q.IsCMDetectionDesired(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if desired {
		t.Error("the failed recording should wait for candidate adoption while the old logo remains")
	}
	missing, err := q.ListMissingCMDetections(ctx, sqlcgen.ListMissingCMDetectionsParams{AfterRecordingID: 0, RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("missing CM detections = %v, want none before candidate adoption", missing)
	}

	// 一覧は枠と直近の失敗理由と、コマを取り寄せる録画を返す。
	logos := fetchCMLogos(t, srv.URL)
	if len(logos) != 1 {
		t.Fatalf("logo list rows = %d, want 1", len(logos))
	}
	logo := logos[0]
	if logo.LogoArea == nil || logo.LogoArea.X != 1180 || logo.LogoArea.CodedWidth != 1440 {
		t.Errorf("logoArea = %#v, want the taught 1180 / 1440x1080", logo.LogoArea)
	}
	if logo.FailedCount != 1 || logo.PendingCount != 0 || logo.LastFailureStage != nil {
		t.Errorf("failure counters = failed:%d pending:%d stage:%v, want 1/0/nil while candidate is pending", logo.FailedCount, logo.PendingCount, logo.LastFailureStage)
	}
	if logo.FrameRecordingId != id {
		t.Errorf("frameRecordingId = %d, want the recording with an original (%d)", logo.FrameRecordingId, id)
	}

	// DELETE で自動に戻る。枠が消え、コマを取り寄せる録画も消える（原本が無い局）。
	req, err = http.NewRequest(http.MethodDelete, srv.URL+"/api/cm-logos/32678/5168/area", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", resp.StatusCode)
	}
	if _, err := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: 32678, ServiceID: 5168}); err == nil {
		t.Error("the taught area still exists after DELETE")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM media_assets WHERE recording_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	logos = fetchCMLogos(t, srv.URL)
	if len(logos) != 1 {
		t.Fatalf("logo list rows = %d, want 1", len(logos))
	}
	if logos[0].LogoArea != nil {
		t.Errorf("logoArea = %#v, want none after DELETE", logos[0].LogoArea)
	}
	if logos[0].FrameRecordingId != 0 {
		t.Errorf("frameRecordingId = %d, want 0 when no original is left", logos[0].FrameRecordingId)
	}
}

func fetchCMLogos(t *testing.T, baseURL string) []CMLogoState {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/cm-logos")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logo list status = %d, want 200", resp.StatusCode)
	}
	var logos []CMLogoState
	if err := json.NewDecoder(resp.Body).Decode(&logos); err != nil {
		t.Fatal(err)
	}
	return logos
}

func TestCMLogoCandidateAPIAdoptsAndRedetectsOnlyWhenRequested(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	id1 := seedRecording(t, pool, "候補採用1", time.Now().Truncate(time.Second), "finished", 986)
	id2 := seedRecording(t, pool, "候補採用2", time.Now().Add(time.Second).Truncate(time.Second), "finished", 987)
	q := sqlcgen.New(pool)
	for _, id := range []int64{id1, id2} {
		if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
			RecordingID: id, Kind: db.AssetKindOriginal, RelPath: fmt.Sprintf("test/%d.ts", id), SizeBytes: 1000,
		}); err != nil {
			t.Fatal(err)
		}
		if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{RecordingID: id, CmRanges: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	// 原本の無い録画・原本が missing の録画の検出は、再検出で消さない（消すと永遠に
	// 「検出中」のまま回復できない）。失敗していた録画は採用で desired に入る。
	noOriginal := seedRecording(t, pool, "原本なし", time.Now().Add(2*time.Second).Truncate(time.Second), "finished", 988)
	missingOriginal := seedRecording(t, pool, "原本欠落", time.Now().Add(3*time.Second).Truncate(time.Second), "finished", 989)
	failedRecording := seedRecording(t, pool, "失敗録画", time.Now().Add(4*time.Second).Truncate(time.Second), "finished", 990)
	for _, id := range []int64{noOriginal, missingOriginal} {
		if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{RecordingID: id, CmRanges: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{missingOriginal, failedRecording} {
		if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
			RecordingID: id, Kind: db.AssetKindOriginal, RelPath: fmt.Sprintf("test/%d.ts", id), SizeBytes: 1000,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO missing_media_assets (media_asset_id)
		SELECT id FROM media_assets WHERE recording_id = $1 AND kind = 'original'`, missingOriginal); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{id1, id2, failedRecording} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
			VALUES ($1, 'always', '{}', true)`, id); err != nil {
			t.Fatal(err)
		}
	}
	attemptCount := startCMDetectionAttemptForTest(t, ctx, q, failedRecording)
	logoStage := "logo"
	markCMDetectionFailureForTest(t, ctx, q, failedRecording, attemptCount, &logoStage, nil)
	if err := q.UpsertCMLogoArea(ctx, sqlcgen.UpsertCMLogoAreaParams{
		NetworkID: 32678, ServiceID: 5168, X: 1180, Y: 24, W: 240, H: 96, CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}
	area, err := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: 32678, ServiceID: 5168})
	if err != nil {
		t.Fatal(err)
	}
	insertCandidate := func(lgd []byte) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO cm_logo_candidates (
				network_id, service_id, state, x, y, w, h, coded_width, coded_height,
				recording_id, observed_area_updated_at, lgd
			) VALUES ($1, $2, 'ready', 1180, 24, 240, 96, 1440, 1080, $3, $4, $5)`,
			32678, 5168, id1, area.UpdatedAt, lgd); err != nil {
			t.Fatal(err)
		}
	}
	insertCandidate([]byte("candidate-one"))
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()

	request := func(body string) *http.Response {
		t.Helper()
		var reader *strings.Reader
		if body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/cm-logos/32678/5168/candidate/adopt", reader)
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := request(`{"redetect":false}`)
	if resp.StatusCode != http.StatusNoContent {
		body := decodeErrorResponse(t, resp)
		t.Fatalf("adopt redetect=false status = %d (%s), want 204", resp.StatusCode, body.Error)
	}
	_ = resp.Body.Close()
	logo, err := q.GetCMLogo(ctx, sqlcgen.GetCMLogoParams{NetworkID: 32678, ServiceID: 5168})
	if err != nil || string(logo.Lgd) != "candidate-one" {
		t.Fatalf("adopted logo = %#v err=%v, want candidate-one", logo, err)
	}
	if _, err := q.GetCMLogoCandidate(ctx, sqlcgen.GetCMLogoCandidateParams{NetworkID: 32678, ServiceID: 5168}); err == nil {
		t.Fatal("candidate still exists after adoption")
	}
	var detections int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_cm_detections WHERE recording_id IN ($1, $2)`, id1, id2).Scan(&detections); err != nil {
		t.Fatal(err)
	}
	if detections != 2 {
		t.Fatalf("detections after redetect=false = %d, want 2", detections)
	}

	insertCandidate([]byte("candidate-two"))
	resp = request("")
	if resp.StatusCode != http.StatusNoContent {
		body := decodeErrorResponse(t, resp)
		t.Fatalf("adopt default redetect status = %d (%s), want 204", resp.StatusCode, body.Error)
	}
	_ = resp.Body.Close()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_cm_detections WHERE recording_id IN ($1, $2)`, id1, id2).Scan(&detections); err != nil {
		t.Fatal(err)
	}
	if detections != 0 {
		t.Fatalf("detections after default redetect = %d, want 0", detections)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_cm_detections WHERE recording_id IN ($1, $2)`, noOriginal, missingOriginal).Scan(&detections); err != nil {
		t.Fatal(err)
	}
	if detections != 2 {
		t.Fatalf("detections of recordings without an active original = %d, want 2 kept", detections)
	}
	for _, id := range []int64{id1, id2, failedRecording} {
		if desired, err := q.IsCMDetectionDesired(ctx, id); err != nil || !desired {
			t.Errorf("IsCMDetectionDesired(%d) after adoption = %v, %v; want true", id, desired, err)
		}
	}
	for _, id := range []int64{noOriginal, missingOriginal} {
		if desired, err := q.IsCMDetectionDesired(ctx, id); err != nil || desired {
			t.Errorf("IsCMDetectionDesired(%d) = %v, %v; want false", id, desired, err)
		}
	}
}

func TestCMLogoMutationsEnqueueReconcile(t *testing.T) {
	pool := testutil.SetupDB(t)
	workers := river.NewWorkers()
	river.AddWorker(workers, &testCMDetectReconcileWorker{})
	river.AddWorker(workers, &testCMLogoCandidateWorker{})
	riverClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  map[string]river.QueueConfig{jobs.CMDetectQueue: {MaxWorkers: 1}},
		Workers: workers,
	})
	if err != nil {
		t.Fatal(err)
	}
	completedEvents, unsubscribe := riverClient.Subscribe(river.EventKindJobCompleted)
	defer unsubscribe()
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, RiverClient: riverClient, CMDetectEnabled: true}))
	defer srv.Close()

	queued := func(want int) {
		t.Helper()
		got := len(testutil.MustListRiverJobsOfKind(t, context.Background(), pool, jobs.CMDetectReconcileArgs{}.Kind()))
		if got != want {
			t.Fatalf("CM reconcile jobs = %d, want %d", got, want)
		}
	}
	candidateQueued := func(want int) {
		t.Helper()
		got := len(testutil.MustListRiverJobsOfKind(t, context.Background(), pool, jobs.CMLogoCandidateJobArgs{}.Kind()))
		if got != want {
			t.Fatalf("CM logo candidate jobs = %d, want %d", got, want)
		}
	}

	id := seedRecording(t, pool, "候補解析", time.Now().Truncate(time.Second), "finished", 985)
	if _, err := pool.Exec(context.Background(), `
		UPDATE recordings SET started_at = '2025-01-01T00:00:00Z', ended_at = '2025-01-01T00:45:00Z'
		WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id, Kind: db.AssetKindOriginal, RelPath: fmt.Sprintf("test/%d.ts", id), SizeBytes: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	putBody := fmt.Sprintf(`{"x":10,"y":20,"w":100,"h":80,"codedWidth":1920,"codedHeight":1080,"recordingId":%d}`, id)
	putReq, err := http.NewRequest(http.MethodPut, srv.URL+"/api/cm-logos/32678/5168/area", strings.NewReader(putBody))
	if err != nil {
		t.Fatal(err)
	}
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(putResp.Body)
	if err != nil {
		_ = putResp.Body.Close()
		t.Fatalf("reading PUT response body: %v", err)
	}
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d (%s), want 204", putResp.StatusCode, body)
	}
	candidateQueued(1)
	var candidateArgs jobs.CMLogoCandidateJobArgs
	if err := json.Unmarshal(testutil.MustListRiverJobsOfKind(t, context.Background(), pool, jobs.CMLogoCandidateJobArgs{}.Kind())[0].EncodedArgs, &candidateArgs); err != nil {
		t.Fatal(err)
	}
	if candidateArgs.RecordingID != id || candidateArgs.RecordingDurationMs != 45*60*1000 {
		t.Errorf("candidate job args = %#v, want recording %d and 2700000ms", candidateArgs, id)
	}

	deleteAreaReq, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/cm-logos/32678/5168/area", nil)
	if err != nil {
		t.Fatal(err)
	}
	deleteAreaResp, err := http.DefaultClient.Do(deleteAreaReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = deleteAreaResp.Body.Close()
	if deleteAreaResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE area status = %d, want 204", deleteAreaResp.StatusCode)
	}
	queued(1)

	clientCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := riverClient.Start(clientCtx); err != nil {
		t.Fatalf("starting River client: %v", err)
	}
	defer func() {
		cancel()
		<-riverClient.Stopped()
	}()
	waitForTestRiverJobCompletion(t, completedEvents, jobs.CMDetectReconcileArgs{}.Kind())

	deleteLogoReq, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/cm-logos/32678/5168", nil)
	if err != nil {
		t.Fatal(err)
	}
	deleteLogoResp, err := http.DefaultClient.Do(deleteLogoReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = deleteLogoResp.Body.Close()
	if deleteLogoResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE logo status = %d, want 204", deleteLogoResp.StatusCode)
	}
	queued(2)
}

func TestCMDetectionRejectedWhenDeploymentDisablesIt(t *testing.T) {
	pool := testutil.SetupDB(t)
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, RiverClient: riverClient}))
	defer srv.Close()

	id := seedRecording(t, pool, "無効構成", time.Now().Truncate(time.Second), "finished", 984)
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id, Kind: db.AssetKindOriginal, RelPath: fmt.Sprintf("test/%d.ts", id), SizeBytes: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	if resp := patchCMDetection(t, encodePolicyURL(srv.URL, id), true); resp.StatusCode != http.StatusConflict {
		t.Fatalf("cmDetect=true status = %d, want 409", resp.StatusCode)
	}
	var policies int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recording_encode_policy WHERE recording_id = $1`, id).Scan(&policies); err != nil {
		t.Fatal(err)
	}
	if policies != 0 {
		t.Fatalf("policy rows after rejected enable = %d, want 0", policies)
	}
	if resp := patchCMDetection(t, encodePolicyURL(srv.URL, id), false); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cmDetect=false status = %d, want 204", resp.StatusCode)
	}

	resp, err := http.Post(fmt.Sprintf("%s/api/recordings/%d/cm-detection/retry", srv.URL, id), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("retry status = %d, want 409", resp.StatusCode)
	}
	jobsCount := len(testutil.MustListRiverJobsOfKind(t, context.Background(), pool, jobs.CMDetectJobArgs{}.Kind()))
	if jobsCount != 0 {
		t.Fatalf("cm_detect jobs after rejected retry = %d, want 0", jobsCount)
	}
}

func cmLogoRequest(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// 採用できない候補（無い・running・failed・解析中に枠が変わった）は 409 で、ロゴを差し替えない。
func TestCMLogoCandidateAdoptRejectsUnadoptableCandidates(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	id := seedRecording(t, pool, "採用拒否", time.Now().Truncate(time.Second), "finished", 991)
	q := sqlcgen.New(pool)
	if err := q.UpsertCMLogoArea(ctx, sqlcgen.UpsertCMLogoAreaParams{
		NetworkID: 32678, ServiceID: 5168, X: 1180, Y: 24, W: 240, H: 96, CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}
	area, err := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: 32678, ServiceID: 5168})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()
	insert := func(state string, observed time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `DELETE FROM cm_logo_candidates`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO cm_logo_candidates (
				network_id, service_id, state, x, y, w, h, coded_width, coded_height,
				recording_id, observed_area_updated_at, lgd
			) VALUES (32678, 5168, $1, 1180, 24, 240, 96, 1440, 1080, $2, $3, 'lgd')`, state, id, observed); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name  string
		setup func()
	}{
		{"no candidate", func() {
			if _, err := pool.Exec(ctx, `DELETE FROM cm_logo_candidates`); err != nil {
				t.Fatal(err)
			}
		}},
		{"running", func() { insert("running", area.UpdatedAt) }},
		{"failed", func() { insert("failed", area.UpdatedAt) }},
		{"area changed after analysis", func() { insert("ready", area.UpdatedAt.Add(-time.Second)) }},
	}
	for _, tc := range cases {
		tc.setup()
		resp := cmLogoRequest(t, http.MethodPost, srv.URL+"/api/cm-logos/32678/5168/candidate/adopt", "")
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("%s: adopt status = %d, want 409", tc.name, resp.StatusCode)
		}
		if _, err := q.GetCMLogo(ctx, sqlcgen.GetCMLogoParams{NetworkID: 32678, ServiceID: 5168}); err == nil {
			t.Errorf("%s: a logo was adopted from an unadoptable candidate", tc.name)
		}
	}
}

// 候補の破棄は候補だけを消し、枠と採用済みのロゴは残す。
func TestCMLogoCandidateDiscardKeepsAreaAndLogo(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	id := seedRecording(t, pool, "候補破棄", time.Now().Truncate(time.Second), "finished", 992)
	q := sqlcgen.New(pool)
	if err := q.UpsertCMLogo(ctx, sqlcgen.UpsertCMLogoParams{
		NetworkID: 32678, ServiceID: 5168, Lgd: []byte("old"), LearnedFrom: &id, CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertCMLogoArea(ctx, sqlcgen.UpsertCMLogoAreaParams{
		NetworkID: 32678, ServiceID: 5168, X: 1180, Y: 24, W: 240, H: 96, CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}
	area, err := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: 32678, ServiceID: 5168})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO cm_logo_candidates (
			network_id, service_id, state, x, y, w, h, coded_width, coded_height,
			recording_id, observed_area_updated_at, lgd
		) VALUES (32678, 5168, 'ready', 1180, 24, 240, 96, 1440, 1080, $1, $2, 'new')`, id, area.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()

	resp := cmLogoRequest(t, http.MethodDelete, srv.URL+"/api/cm-logos/32678/5168/candidate", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE candidate status = %d, want 204", resp.StatusCode)
	}
	if _, err := q.GetCMLogoCandidate(ctx, sqlcgen.GetCMLogoCandidateParams{NetworkID: 32678, ServiceID: 5168}); err == nil {
		t.Error("candidate still exists after discard")
	}
	if logo, err := q.GetCMLogo(ctx, sqlcgen.GetCMLogoParams{NetworkID: 32678, ServiceID: 5168}); err != nil || string(logo.Lgd) != "old" {
		t.Errorf("adopted logo = %#v, %v; want the old logo kept", logo, err)
	}
	if _, err := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: 32678, ServiceID: 5168}); err != nil {
		t.Errorf("taught area was removed by discarding the candidate: %v", err)
	}
}

// 枠の削除（自動に戻す）は、その局の adopt 待ち attempt だけを消す。別の局の adopt 待ちは残す。
func TestCMLogoAreaDeleteClearsOnlyThisStationsAdoptAttempts(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	here := seedRecording(t, pool, "この局", time.Now().Truncate(time.Second), "finished", 993)
	other := seedRecording(t, pool, "別の局", time.Now().Truncate(time.Second), "finished", 994)
	if _, err := pool.Exec(ctx, `UPDATE recordings SET network_id = 32679 WHERE id = $1`, other); err != nil {
		t.Fatal(err)
	}
	adopt, logo := "adopt", "logo"
	hereLogo := seedRecording(t, pool, "この局の別失敗", time.Now().Truncate(time.Second), "finished", 995)
	for id, stage := range map[int64]*string{here: &adopt, other: &adopt, hereLogo: &logo} {
		attemptCount := startCMDetectionAttemptForTest(t, ctx, q, id)
		markCMDetectionFailureForTest(t, ctx, q, id, attemptCount, stage, nil)
	}
	if err := q.UpsertCMLogoArea(ctx, sqlcgen.UpsertCMLogoAreaParams{
		NetworkID: 32678, ServiceID: 5168, X: 1180, Y: 24, W: 240, H: 96, CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()

	resp := cmLogoRequest(t, http.MethodDelete, srv.URL+"/api/cm-logos/32678/5168/area", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE area status = %d, want 204", resp.StatusCode)
	}
	has := func(id int64) bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if has(here) {
		t.Error("the adopt attempt of this station survived the area deletion")
	}
	if !has(other) {
		t.Error("the adopt attempt of another station was deleted")
	}
	if !has(hereLogo) {
		t.Error("a non-adopt failure of this station was deleted")
	}
}
