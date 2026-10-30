package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func deleteEncoded(t *testing.T, base string, id int64, profile string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d/encoded/%s", base, id, profile), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// waitForLockWaiter は current_database() 内で行ロックを待っているセッションが
// 現れるまでポーリングする。
func waitForLockWaiter(t *testing.T, pool *pgxpool.Pool, operation string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database()
  AND wait_event_type = 'Lock'
  AND pid <> pg_backend_pid()`).Scan(&n); err != nil {
			t.Fatalf("polling pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("row lock waiter did not appear (%s)", operation)
}

// seedEncodedVersions は seedIngested 済みの録画に active な encoded を置き、desired も揃える。
func seedEncodedVersions(t *testing.T, pool *pgxpool.Pool, id int64, profiles ...string) {
	t.Helper()
	for _, p := range profiles {
		profile := p
		dbtest.MediaAsset(t, pool, id, func(fixtureParams *dbtest.MediaAssetParams) {
			fixtureParams.Kind = store.AssetKindEncoded
			fixtureParams.Profile = &profile
			fixtureParams.RelPath = fmt.Sprintf("encoded/%d_%s.mp4", id, p)
			fixtureParams.SizeBytes = 200
		})

	}
	setRecordingEncodeProfiles(t, pool, id, profiles)
}

func markOriginalDeleted(t *testing.T, pool *pgxpool.Pool, originalID int64) {
	t.Helper()
	dbtest.SetMediaAssetState(t, pool, originalID, "deleted")
}

func removalRequests(t *testing.T, pool *pgxpool.Pool, id int64) []string {
	t.Helper()
	var profiles []string
	if err := pool.QueryRow(context.Background(),
		`SELECT coalesce(array_agg(profile ORDER BY profile), '{}') FROM encoded_asset_removal_requests WHERE recording_id = $1`, id,
	).Scan(&profiles); err != nil {
		t.Fatalf("loading removal requests: %v", err)
	}
	return profiles
}

func listedEncodedProfiles(t *testing.T, base string, id int64) []string {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/api/recordings/%d", base, id))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET recording status = %d", resp.StatusCode)
	}
	var rec struct {
		EncodedAssets []struct {
			Profile string `json:"profile"`
		} `json:"encodedAssets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		t.Fatalf("decoding recording: %v", err)
	}
	profiles := []string{}
	for _, a := range rec.EncodedAssets {
		profiles = append(profiles, a.Profile)
	}
	return profiles
}

// 原本ありで h264 を外すと 204。desired から外れ、要求行が入り、応答直後の取得から
// h264 が消える。もう一度外しても 204（冪等）。足し直すと要求行も消える。
func TestRemoveRecordingEncodedAsset_WithOriginal_Returns204(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Ready: pool.Ping, RecordingQuery: pgstoretest.Recordings(pool), EncodePolicyStore: pgstoretest.Media(pool), EncodeProfileNames: []string{"h264", "h265"}}))
	defer srv.Close()

	id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "版を外す", time.Now().Truncate(time.Second), "finished", 701
	})
	seedIngested(t, pool, id, 1000, nil)
	seedEncodedVersions(t, pool, id, "h264", "h265")
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
		VALUES ($1, 'h264', '{[0,100)}'::int8multirange)`, id); err != nil {
		t.Fatalf("seeding rebuild request: %v", err)
	}

	if resp := deleteEncoded(t, srv.URL, id, "h264"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"h265"}) {
		t.Errorf("encode_profiles = %v, want [h265]", got)
	}
	if got := removalRequests(t, pool, id); !slices.Equal(got, []string{"h264"}) {
		t.Errorf("removal requests = %v, want [h264]", got)
	}
	var rebuildRequests int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM encoded_asset_rebuild_requests WHERE recording_id = $1 AND profile = 'h264'`, id).Scan(&rebuildRequests); err != nil {
		t.Fatal(err)
	}
	if rebuildRequests != 0 {
		t.Errorf("rebuild requests after removing h264 = %d, want 0", rebuildRequests)
	}
	if got := listedEncodedProfiles(t, srv.URL, id); !slices.Equal(got, []string{"h265"}) {
		t.Errorf("encodedAssets = %v, want [h265] right after the removal", got)
	}
	if resp := deleteEncoded(t, srv.URL, id, "h264"); resp.StatusCode != http.StatusNoContent {
		t.Errorf("repeated removal status = %d, want 204", resp.StatusCode)
	}

	if resp := postEncodeProfiles(t, encodeProfilesURL(srv.URL, id), []string{"h264"}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("re-add status = %d, want 204", resp.StatusCode)
	}
	if got := removalRequests(t, pool, id); len(got) != 0 {
		t.Errorf("removal requests after re-add = %v, want none", got)
	}
	if got := listedEncodedProfiles(t, srv.URL, id); !slices.Equal(got, []string{"h264", "h265"}) {
		t.Errorf("encodedAssets after re-add = %v, want [h264 h265]", got)
	}
}

func TestRemoveRecordingEncodedAsset_AllowsTrashedRecording(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{
		Ready:              pool.Ping,
		EncodePolicyStore:  pgstoretest.Media(pool),
		EncodeProfileNames: []string{"h264", "h265"},
	}))
	defer srv.Close()

	id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "ごみ箱から版を外す", time.Now().Truncate(time.Second), "finished", 703
	})
	seedIngested(t, pool, id, 1000, nil)
	seedEncodedVersions(t, pool, id, "h264", "h265")
	if _, err := pool.Exec(context.Background(), `UPDATE recordings SET deleted_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("trashing recording: %v", err)
	}

	if resp := deleteEncoded(t, srv.URL, id, "h264"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status for trashed recording = %d, want 204", resp.StatusCode)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"h265"}) {
		t.Errorf("encode_profiles = %v, want [h265]", got)
	}
	if got := removalRequests(t, pool, id); !slices.Equal(got, []string{"h264"}) {
		t.Errorf("removal requests = %v, want [h264]", got)
	}
}

// 原本なしでも、外していない版が残るなら 204。最後の 1 本は 409 で、何も書かない。
func TestRemoveRecordingEncodedAsset_WithoutOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Ready: pool.Ping, EncodePolicyStore: pgstoretest.Media(pool)}))
	defer srv.Close()

	id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "原本なし", time.Now().Truncate(time.Second), "finished", 702
	})
	originalID := seedIngested(t, pool, id, 1000, nil)
	seedEncodedVersions(t, pool, id, "h264", "h265")
	markOriginalDeleted(t, pool, originalID)

	if resp := deleteEncoded(t, srv.URL, id, "h264"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first removal status = %d, want 204", resp.StatusCode)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
		VALUES ($1, 'h265', '{[0,100)}'::int8multirange)`, id); err != nil {
		t.Fatalf("seeding rebuild request before rejected removal: %v", err)
	}
	resp := deleteEncoded(t, srv.URL, id, "h265")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("last-copy removal status = %d, want 409", resp.StatusCode)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"h265"}) {
		t.Errorf("encode_profiles after 409 = %v, want [h265] (rolled back)", got)
	}
	if got := removalRequests(t, pool, id); !slices.Equal(got, []string{"h264"}) {
		t.Errorf("removal requests after 409 = %v, want [h264] (rolled back)", got)
	}
	var rebuildRequests int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM encoded_asset_rebuild_requests WHERE recording_id = $1 AND profile = 'h265'`, id).Scan(&rebuildRequests); err != nil {
		t.Fatal(err)
	}
	if rebuildRequests != 1 {
		t.Errorf("rebuild requests after rejected removal = %d, want 1 (rolled back)", rebuildRequests)
	}
}

// 該当する active な版が無い・録画が無いのはいずれも 404。
func TestRemoveRecordingEncodedAsset_NotFound(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Ready: pool.Ping, EncodePolicyStore: pgstoretest.Media(pool)}))
	defer srv.Close()

	id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "版なし", time.Now().Truncate(time.Second), "finished", 703
	})
	seedIngested(t, pool, id, 1000, nil)
	seedEncodedVersions(t, pool, id, "h264")

	if resp := deleteEncoded(t, srv.URL, id, "h265"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing profile status = %d, want 404", resp.StatusCode)
	}
	if resp := deleteEncoded(t, srv.URL, id+1000, "h264"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing recording status = %d, want 404", resp.StatusCode)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"h264"}) {
		t.Errorf("encode_profiles = %v, want [h264] untouched", got)
	}
}

// until_encoded で最後の desired を外すと、CHECK を満たすよう always に倒す。
// 残る desired があるときは until_encoded のまま。
func TestRemoveRecordingEncodedAsset_UntilEncodedClampsToAlways(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Ready: pool.Ping, EncodePolicyStore: pgstoretest.Media(pool)}))
	defer srv.Close()

	id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "クランプ", time.Now().Truncate(time.Second), "finished", 704
	})
	seedIngested(t, pool, id, 1000, nil)
	seedEncodedVersions(t, pool, id, "h264", "h265")
	if _, err := pool.Exec(context.Background(),
		`UPDATE recording_encode_policy SET keep_original = 'until_encoded' WHERE recording_id = $1`, id); err != nil {
		t.Fatal(err)
	}

	if resp := deleteEncoded(t, srv.URL, id, "h264"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first removal status = %d, want 204", resp.StatusCode)
	}
	if got := getRecordingKeepOriginal(t, pool, id); got != "until_encoded" {
		t.Errorf("keep_original with h265 left = %q, want until_encoded", got)
	}
	if resp := deleteEncoded(t, srv.URL, id, "h265"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("last desired removal status = %d, want 204", resp.StatusCode)
	}
	if got := getRecordingKeepOriginal(t, pool, id); got != "always" {
		t.Errorf("keep_original = %q, want always (clamped)", got)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); len(got) != 0 {
		t.Errorf("encode_profiles = %v, want empty", got)
	}
}

// 原本なしで h264 と h265 を同時に外す 2 本は直列化され、後の 1 本が 409 になる。
// 先の tx を開いたまま止め、handler がその commit を待ってから外した後の状態を見ることを
// 確かめる。待つのは policy 行のロックで、FOR UPDATE を外しても後続の UPDATE が同じ行を
// 待つのでこのテストは通る（変異で確認）。
func TestRemoveRecordingEncodedAsset_ConcurrentRemovalsLeaveOneCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Ready: pool.Ping, EncodePolicyStore: pgstoretest.Media(pool)}))
	defer srv.Close()
	ctx := context.Background()

	id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "並行", time.Now().Truncate(time.Second), "finished", 705
	})
	originalID := seedIngested(t, pool, id, 1000, nil)
	seedEncodedVersions(t, pool, id, "h264", "h265")
	markOriginalDeleted(t, pool, originalID)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	if _, err := q.LockRecordingEncodePolicy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := q.RemoveRecordingEncodeProfile(ctx, sqlcgen.RemoveRecordingEncodeProfileParams{RecordingID: id, Profile: "h265"}); err != nil {
		t.Fatal(err)
	}
	if err := q.InsertEncodedAssetRemovalRequest(ctx, sqlcgen.InsertEncodedAssetRemovalRequestParams{RecordingID: id, Profile: "h265"}); err != nil {
		t.Fatal(err)
	}

	status := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d/encoded/h264", srv.URL, id), nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			status <- -1
			return
		}
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()
	select {
	case got := <-status:
		t.Fatalf("removal returned %d while another removal held the policy row, want it to wait", got)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-status:
		if got != http.StatusConflict {
			t.Errorf("second removal status = %d, want 409", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("removal did not finish after the other removal committed")
	}
	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"h264"}) {
		t.Errorf("encode_profiles = %v, want [h264]", got)
	}
}

// Encoded asset の削除 API は録画行を policy 行より先にロックする。
// encode 公開側も録画行を先に取るため、逆順で policy を持ったまま録画行の FK
// 検査を待つ循環を作らない。
func TestRemoveRecordingEncodedAssetLocksRecordingBeforePolicy(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Ready: pool.Ping, EncodePolicyStore: pgstoretest.Media(pool)}))
	defer srv.Close()
	ctx := context.Background()

	id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "削除ロック順", time.Now().Truncate(time.Second), "finished", 711
	})
	seedIngested(t, pool, id, 1000, nil)
	seedEncodedVersions(t, pool, id, "h264", "h265")

	recordingTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recordingTx.Rollback(ctx) }()
	if _, err := sqlcgen.New(recordingTx).LockRecording(ctx, id); err != nil {
		t.Fatalf("locking recording row: %v", err)
	}

	policyTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = policyTx.Rollback(ctx) }()
	if _, err := sqlcgen.New(policyTx).LockRecordingEncodePolicy(ctx, id); err != nil {
		t.Fatalf("locking encode policy row: %v", err)
	}

	type response struct {
		status int
		err    error
	}
	result := make(chan response, 1)
	go func() {
		req, err := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d/encoded/h264", srv.URL, id), nil)
		if err != nil {
			result <- response{err: err}
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			result <- response{err: err}
			return
		}
		_ = resp.Body.Close()
		result <- response{status: resp.StatusCode}
	}()
	waitForLockWaiter(t, pool, "encoded removal handler")
	if err := policyTx.Commit(ctx); err != nil {
		t.Fatalf("committing policy lock: %v", err)
	}

	// もう一方の tx が録画行を保持している間、handler が待っている SQL は
	// recordings の FOR UPDATE でなければならない。旧順序では policy を先に取り、
	// encoded_asset_removal_requests の FK 検査でここに詰まる。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database()
				  AND wait_event_type = 'Lock'
				  AND pid <> pg_backend_pid()
				  AND query ILIKE '%FROM recordings%'
				  AND query ILIKE '%FOR UPDATE%'
			)`).Scan(&waiting)
		if err != nil {
			t.Fatalf("polling for recording row lock waiter: %v", err)
		}
		if waiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	var waiting bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND pid <> pg_backend_pid()
			  AND query ILIKE '%FROM recordings%'
			  AND query ILIKE '%FOR UPDATE%'
		)`).Scan(&waiting); err != nil {
		t.Fatalf("checking recording row lock waiter: %v", err)
	}
	if !waiting {
		t.Fatal("encoded removal handler did not wait on the recording row before acquiring policy")
	}

	if err := recordingTx.Commit(ctx); err != nil {
		t.Fatalf("committing recording lock: %v", err)
	}
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("encoded removal request: %v", got.err)
		}
		if got.status != http.StatusNoContent {
			t.Errorf("encoded removal status = %d, want 204", got.status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("encoded removal did not finish after the recording row lock was released")
	}
}

// 外す tx が要求行を書いて commit する前に足し直しが走っても、commit 後に要求行が残らない。
// 足し直し側は policy 行のロックを待ち、外す側の commit 後のスナップショットで要求行を消す。
func TestAddRecordingEncodeProfiles_WaitsForConcurrentRemovalAndClearsRequest(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Ready: pool.Ping, EncodePolicyStore: pgstoretest.Media(pool), EncodeProfileNames: []string{"h264", "h265"}}))
	defer srv.Close()
	ctx := context.Background()

	id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "足し直し競合", time.Now().Truncate(time.Second), "finished", 706
	})
	seedIngested(t, pool, id, 1000, nil)
	seedEncodedVersions(t, pool, id, "h264", "h265")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	if _, err := q.LockRecordingEncodePolicy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := q.RemoveRecordingEncodeProfile(ctx, sqlcgen.RemoveRecordingEncodeProfileParams{RecordingID: id, Profile: "h264"}); err != nil {
		t.Fatal(err)
	}
	if err := q.InsertEncodedAssetRemovalRequest(ctx, sqlcgen.InsertEncodedAssetRemovalRequestParams{RecordingID: id, Profile: "h264"}); err != nil {
		t.Fatal(err)
	}

	status := make(chan int, 1)
	go func() {
		resp := postEncodeProfiles(t, encodeProfilesURL(srv.URL, id), []string{"h264"})
		status <- resp.StatusCode
	}()
	select {
	case got := <-status:
		t.Fatalf("re-add returned %d while a removal held the policy row, want it to wait", got)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-status:
		if got != http.StatusNoContent {
			t.Errorf("re-add status = %d, want 204", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("re-add did not finish after the removal committed")
	}
	if got := removalRequests(t, pool, id); len(got) != 0 {
		t.Errorf("removal requests = %v, want none (h264 is desired again)", got)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"h264", "h265"}) {
		t.Errorf("encode_profiles = %v, want [h264 h265]", got)
	}
}

func policyRowCount(t *testing.T, pool *pgxpool.Pool, id int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM recording_encode_policy WHERE recording_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// catalog 無しの rescue は encoded を作っても policy 行を作らない。active な encoded が
// あれば凍結済みとみなし、always / 空 desired で行を作ってから外す。
func TestRemoveRecordingEncodedAsset_NoPolicyRowFreezesAndRemoves(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Ready: pool.Ping, RecordingQuery: pgstoretest.Recordings(pool), EncodePolicyStore: pgstoretest.Media(pool), EncodeProfileNames: []string{"h264", "h265"}}))
	defer srv.Close()

	id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "policy 無し", time.Now().Truncate(time.Second), "finished", 707
	})
	seedIngested(t, pool, id, 1000, nil)
	seedEncodedVersions(t, pool, id, "h264", "h265")
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM recording_encode_policy WHERE recording_id = $1`, id); err != nil {
		t.Fatal(err)
	}

	if resp := deleteEncoded(t, srv.URL, id, "h264"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := getRecordingKeepOriginal(t, pool, id); got != "always" {
		t.Errorf("keep_original = %q, want always", got)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); len(got) != 0 {
		t.Errorf("encode_profiles = %v, want empty", got)
	}
	if got := removalRequests(t, pool, id); !slices.Equal(got, []string{"h264"}) {
		t.Errorf("removal requests = %v, want [h264]", got)
	}
	if got := listedEncodedProfiles(t, srv.URL, id); !slices.Equal(got, []string{"h265"}) {
		t.Errorf("encodedAssets = %v, want [h265]", got)
	}
}

// policy 行が無く最後の版を外そうとして 409 になると、作りかけの policy 行も残らない。
func TestRemoveRecordingEncodedAsset_NoPolicyRowLastCopyRollsBackFreeze(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Ready: pool.Ping, EncodePolicyStore: pgstoretest.Media(pool), EncodeProfileNames: []string{"h264", "h265"}}))
	defer srv.Close()

	id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "policy 無し最後", time.Now().Truncate(time.Second), "finished", 708
	})
	originalID := seedIngested(t, pool, id, 1000, nil)
	seedEncodedVersions(t, pool, id, "h264")
	markOriginalDeleted(t, pool, originalID)
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM recording_encode_policy WHERE recording_id = $1`, id); err != nil {
		t.Fatal(err)
	}

	if resp := deleteEncoded(t, srv.URL, id, "h264"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if n := policyRowCount(t, pool, id); n != 0 {
		t.Errorf("policy rows after 409 = %d, want 0 (rolled back)", n)
	}
}

func cutRouterForRemoval(pool *pgxpool.Pool, live bool) http.Handler {
	return NewRouter(RouterConfig{
		Ready:               pool.Ping,
		EncodePolicyStore:   pgstoretest.Media(pool),
		EncodeProfileNames:  []string{"h264", "cut", "cut2"},
		CutProfileNames:     []string{"cut", "cut2"},
		RecordingHLSEnabled: live,
		MediaStoreName:      dbtest.StoreName,
	})
}

// live 無効で、外した後の desired がカット版だけになる削除は 400 で何も書かない。
// cut を外す削除（h264 が残る）は 204。live 有効なら cut だけが残っても 204。
func TestRemoveRecordingEncodedAsset_CutOnlyResultRejectedUnlessLive(t *testing.T) {
	pool := testutil.SetupDB(t)
	srvOff := httptest.NewServer(cutRouterForRemoval(pool, false))
	defer srvOff.Close()
	srvOn := httptest.NewServer(cutRouterForRemoval(pool, true))
	defer srvOn.Close()

	id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "cut 規則", time.Now().Truncate(time.Second), "finished", 709
	})
	seedIngested(t, pool, id, 1000, nil)
	seedEncodedVersions(t, pool, id, "h264", "cut")

	if resp := deleteEncoded(t, srvOff.URL, id, "h264"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("live off, remove h264 status = %d, want 400", resp.StatusCode)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"h264", "cut"}) {
		t.Errorf("encode_profiles after 400 = %v, want [h264 cut]", got)
	}
	if got := removalRequests(t, pool, id); len(got) != 0 {
		t.Errorf("removal requests after 400 = %v, want none", got)
	}
	if resp := deleteEncoded(t, srvOn.URL, id, "h264"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("live on, remove h264 status = %d, want 204", resp.StatusCode)
	}

	id2 := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "cut 規則 2", time.Now().Truncate(time.Second), "finished", 710
	})
	seedIngested(t, pool, id2, 1000, nil)
	seedEncodedVersions(t, pool, id2, "h264", "cut")
	if resp := deleteEncoded(t, srvOff.URL, id2, "cut"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("live off, remove cut status = %d, want 204", resp.StatusCode)
	}

	// desired が前から [cut] だけ（live が有効だった頃の録画など）でも、desired に無い版を
	// 外すのは desired を変えないので拒否しない。
	id3 := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "cut 規則 3", time.Now().Truncate(time.Second), "finished", 712
	})
	seedIngested(t, pool, id3, 1000, nil)
	seedEncodedVersions(t, pool, id3, "h264", "cut")
	setRecordingEncodeProfiles(t, pool, id3, []string{"cut"})
	if resp := deleteEncoded(t, srvOff.URL, id3, "h264"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("live off, remove h264 outside a cut-only desired status = %d, want 204", resp.StatusCode)
	}
}

// 足し直しの cut 判定は、外す tx の commit 後の desired で行う。古い [h264 cut] で
// 判定すると [cut2] の追加が通り、live 無効で禁止の「カット版だけ」になる。
func TestAddRecordingEncodeProfiles_CutRuleUsesDesiredAfterConcurrentRemoval(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(cutRouterForRemoval(pool, false))
	defer srv.Close()
	ctx := context.Background()

	id := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "足し直し cut 競合", time.Now().Truncate(time.Second), "finished", 711
	})
	seedIngested(t, pool, id, 1000, nil)
	seedEncodedVersions(t, pool, id, "h264", "cut")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	if _, err := q.LockRecordingEncodePolicy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := q.RemoveRecordingEncodeProfile(ctx, sqlcgen.RemoveRecordingEncodeProfileParams{RecordingID: id, Profile: "h264"}); err != nil {
		t.Fatal(err)
	}
	if err := q.InsertEncodedAssetRemovalRequest(ctx, sqlcgen.InsertEncodedAssetRemovalRequestParams{RecordingID: id, Profile: "h264"}); err != nil {
		t.Fatal(err)
	}

	status := make(chan int, 1)
	go func() {
		resp := postEncodeProfiles(t, encodeProfilesURL(srv.URL, id), []string{"cut2"})
		status <- resp.StatusCode
	}()
	select {
	case got := <-status:
		t.Fatalf("re-add returned %d while a removal held the policy row, want it to wait", got)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-status:
		if got != http.StatusBadRequest {
			t.Errorf("re-add status = %d, want 400", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("re-add did not finish after the removal committed")
	}
	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"cut"}) {
		t.Errorf("encode_profiles = %v, want [cut]", got)
	}
}
