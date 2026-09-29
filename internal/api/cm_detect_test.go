package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

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
	if err := sqlcgen.New(pool).MarkCMDetectionRunning(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := sqlcgen.New(pool).MarkCMDetectionFailure(context.Background(), sqlcgen.MarkCMDetectionFailureParams{
		RecordingID: id, State: "failed",
	}); err != nil {
		t.Fatal(err)
	}
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
	var detections, attempts, jobsCount int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recording_cm_detections WHERE recording_id = $1`, id).Scan(&detections); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM river_job WHERE kind = $1`, jobs.CMDetectJobArgs{}.Kind()).Scan(&jobsCount); err != nil {
		t.Fatal(err)
	}
	if detections != 0 || attempts != 0 || jobsCount != 1 {
		t.Fatalf("after retry: detections=%d attempts=%d jobs=%d; want 0/0/1", detections, attempts, jobsCount)
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
		PreviewPng: []byte{1, 2, 3}, LearnedFrom: &id,
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.MarkCMDetectionRunning(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := q.MarkCMDetectionFailure(context.Background(), sqlcgen.MarkCMDetectionFailureParams{RecordingID: id, State: "failed"}); err != nil {
		t.Fatal(err)
	}

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
	if logos[0].State != CMLogoStateStateFailed || logos[0].FailedCount != 1 || logos[0].LearnedAt == nil || logos[0].PreviewPng == nil {
		t.Fatalf("logo state = %#v, want failed logo with preview", logos[0])
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
	var jobsCount int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM river_job WHERE kind = 'cm_detect'`).Scan(&jobsCount); err != nil {
		t.Fatal(err)
	}
	if jobsCount != 0 {
		t.Fatalf("cm_detect jobs after rejected retry = %d, want 0", jobsCount)
	}
}
