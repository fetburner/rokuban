package pgstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 実体無し（missing_media_assets）のコピーしか無い録画について、各読み手が
// 含む／含まないを両方向で固定する。各読み手には実体の有る対照の録画を並べ、
// 対照は含まれ、実体無しの録画は決めた通りに扱われることを確かめる。
// 判定の一覧と理由は、各読み手の定義の COMMENT（active_present_media_asset_copies を
// 定義する migration と internal/db/queries/）にある。

const (
	presentCopiesDerivedStore = "derived-store"
	presentCopiesFSStore      = "fs-store"
)

// presentCopiesRecording は原本を持つ録画を作り、missing なら原本の置き場に実体無しを記録する。
func presentCopiesRecording(t *testing.T, pool *pgxpool.Pool, networkID, serviceID int32, missing bool) (recordingID, originalID int64) {
	t.Helper()
	recordingID = dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.NetworkID = networkID
		p.ServiceID = serviceID
	})
	originalID = dbtest.MediaAsset(t, pool, recordingID)
	if missing {
		observePresentCopyMissing(t, pool, originalID, dbtest.StoreName)
	}
	return recordingID, originalID
}

// presentCopiesDerived は派生資産を作り、その置き場を store に移す。missing なら実体無しを記録する。
func presentCopiesDerived(t *testing.T, pool *pgxpool.Pool, recordingID int64, kind string, profile *string, store string, missing bool) int64 {
	t.Helper()
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.Kind = kind
		p.Profile = profile
	})
	if _, err := pool.Exec(t.Context(), `UPDATE media_asset_copies SET store = $2 WHERE media_asset_id = $1`, assetID, store); err != nil {
		t.Fatalf("moving derived copy to %q: %v", store, err)
	}
	if missing {
		observePresentCopyMissing(t, pool, assetID, store)
	}
	return assetID
}

func observePresentCopyMissing(t *testing.T, pool *pgxpool.Pool, assetID int64, store string) {
	t.Helper()
	copyID := dbtest.MediaAssetCopyID(t, pool, assetID, store)
	if err := sqlcgen.New(pool).UpsertMissingMediaAsset(t.Context(), copyID); err != nil {
		t.Fatalf("recording missing copy %d: %v", copyID, err)
	}
}

func setPresentCopiesPolicy(t *testing.T, pool *pgxpool.Pool, recordingID int64, keep string, profiles []string, cmDetect bool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, $2, $3, $4)`, recordingID, keep, profiles, cmDetect); err != nil {
		t.Fatalf("freezing encode policy: %v", err)
	}
}

func existsIn(t *testing.T, pool *pgxpool.Pool, query string, args ...any) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS (`+query+`)`, args...).Scan(&ok); err != nil {
		t.Fatalf("querying %q: %v", query, err)
	}
	return ok
}

func TestPresentCopies_EncodableRecordingsKeepsMissingOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	present, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missing, _ := presentCopiesRecording(t, pool, 1, 1, true)

	for name, id := range map[string]int64{"present": present, "missing": missing} {
		if !existsIn(t, pool, `SELECT 1 FROM encodable_recordings WHERE recording_id = $1`, id) {
			t.Errorf("%s recording %d is not encodable, want included (missing originals stay encodable)", name, id)
		}
	}
}

func TestPresentCopies_CMDetectionDesiredExcludesMissingOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	present, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missing, _ := presentCopiesRecording(t, pool, 1, 1, true)
	setPresentCopiesPolicy(t, pool, present, "always", []string{}, true)
	setPresentCopiesPolicy(t, pool, missing, "always", []string{}, true)

	if !existsIn(t, pool, `SELECT 1 FROM cm_detection_desired WHERE recording_id = $1`, present) {
		t.Errorf("present recording %d is not CM-desired, want included", present)
	}
	if existsIn(t, pool, `SELECT 1 FROM cm_detection_desired WHERE recording_id = $1`, missing) {
		t.Errorf("missing recording %d is CM-desired, want excluded", missing)
	}
}

func TestPresentCopies_HasCMRecordingOriginalExcludesMissingOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	present, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missing, _ := presentCopiesRecording(t, pool, 1, 1, true)
	store := dbtest.StoreName

	gotPresent, err := q.HasCMRecordingOriginal(t.Context(), sqlcgen.HasCMRecordingOriginalParams{
		RecordingID: present, NetworkID: 32678, ServiceID: 5168, StoreName: &store,
	})
	if err != nil {
		t.Fatalf("HasCMRecordingOriginal(present): %v", err)
	}
	gotMissing, err := q.HasCMRecordingOriginal(t.Context(), sqlcgen.HasCMRecordingOriginalParams{
		RecordingID: missing, NetworkID: 1, ServiceID: 1, StoreName: &store,
	})
	if err != nil {
		t.Fatalf("HasCMRecordingOriginal(missing): %v", err)
	}
	if !gotPresent || gotMissing {
		t.Errorf("HasCMRecordingOriginal present/missing = %v/%v, want true/false", gotPresent, gotMissing)
	}
}

func TestPresentCopies_CMLogoCandidateDesiredExcludesMissingOnlyStation(t *testing.T) {
	pool := testutil.SetupDB(t)
	present, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	_, _ = presentCopiesRecording(t, pool, 1, 1, true)
	for _, station := range [][2]int32{{32678, 5168}, {1, 1}} {
		if _, err := pool.Exec(t.Context(), `
INSERT INTO cm_logo_areas (network_id, service_id, x, y, w, h, coded_width, coded_height)
VALUES ($1, $2, 0, 0, 10, 10, 1920, 1080)`, station[0], station[1]); err != nil {
			t.Fatalf("inserting logo area: %v", err)
		}
	}

	var presentRecording *int64
	if err := pool.QueryRow(t.Context(), `
SELECT recording_id FROM cm_logo_candidate_desired WHERE network_id = 32678 AND service_id = 5168`).
		Scan(&presentRecording); err != nil {
		t.Fatalf("reading candidate for present station: %v", err)
	}
	if presentRecording == nil || *presentRecording != present {
		t.Errorf("present station candidate = %v, want recording %d", presentRecording, present)
	}
	if existsIn(t, pool, `SELECT 1 FROM cm_logo_candidate_desired WHERE network_id = 1 AND service_id = 1`) {
		t.Errorf("missing-only station has a logo candidate, want none")
	}
}

func TestPresentCopies_ListMissingThumbnailRecordingsExcludesMissingOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	present, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missing, _ := presentCopiesRecording(t, pool, 1, 1, true)
	store := dbtest.StoreName

	got, err := q.ListMissingThumbnailRecordings(t.Context(), sqlcgen.ListMissingThumbnailRecordingsParams{
		OriginalStore: &store, AfterRecordingID: 0, DerivedStore: presentCopiesDerivedStore, RowLimit: 100,
	})
	if err != nil {
		t.Fatalf("ListMissingThumbnailRecordings: %v", err)
	}
	if !containsID(got, present) || containsID(got, missing) {
		t.Errorf("thumbnail candidates = %v, want present %d in and missing %d out", got, present, missing)
	}
}

func TestPresentCopies_ListMissingTSScanRecordingsExcludesMissingOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	present, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missing, _ := presentCopiesRecording(t, pool, 1, 1, true)
	store := dbtest.StoreName

	got, err := q.ListMissingTSScanRecordings(t.Context(), sqlcgen.ListMissingTSScanRecordingsParams{
		StoreName: &store, AfterRecordingID: 0, RowLimit: 100,
	})
	if err != nil {
		t.Fatalf("ListMissingTSScanRecordings: %v", err)
	}
	if !containsID(got, present) || containsID(got, missing) {
		t.Errorf("TS scan candidates = %v, want present %d in and missing %d out", got, present, missing)
	}
}

func TestPresentCopies_UnencodedDesiredCountsMissingEncodedAsPresent(t *testing.T) {
	pool := testutil.SetupDB(t)
	none, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missingEncoded, _ := presentCopiesRecording(t, pool, 1, 1, false)
	profile := "p1"
	presentCopiesDerived(t, pool, missingEncoded, "encoded", &profile, presentCopiesDerivedStore, true)
	setPresentCopiesPolicy(t, pool, none, "always", []string{profile}, false)
	setPresentCopiesPolicy(t, pool, missingEncoded, "always", []string{profile}, false)

	if !existsIn(t, pool, `SELECT 1 FROM unencoded_desired_profiles WHERE recording_id = $1 AND profile = $2`, none, profile) {
		t.Errorf("recording without encoded is not unencoded, want included")
	}
	if existsIn(t, pool, `SELECT 1 FROM unencoded_desired_profiles WHERE recording_id = $1 AND profile = $2`, missingEncoded, profile) {
		t.Errorf("recording with missing encoded is unencoded, want excluded (missing derived copy counts as present)")
	}
}

func TestPresentCopies_UntilEncodedExcludesMissingDerivedCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	profile := "p1"
	ok, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missing, _ := presentCopiesRecording(t, pool, 1, 1, false)
	for _, rec := range []struct {
		id      int64
		missing bool
	}{{ok, false}, {missing, true}} {
		setPresentCopiesPolicy(t, pool, rec.id, "until_encoded", []string{profile}, false)
		presentCopiesDerived(t, pool, rec.id, "encoded", &profile, presentCopiesDerivedStore, false)
		presentCopiesDerived(t, pool, rec.id, "thumbnail", nil, presentCopiesDerivedStore, rec.missing)
		presentCopiesDerived(t, pool, rec.id, "seek_tiles", nil, presentCopiesDerivedStore, false)
		var originalID int64
		if err := pool.QueryRow(t.Context(), `SELECT id FROM media_assets WHERE recording_id = $1 AND kind = 'original'`, rec.id).Scan(&originalID); err != nil {
			t.Fatalf("reading original: %v", err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes) VALUES ($1, 1)`, originalID); err != nil {
			t.Fatalf("recording TS scan: %v", err)
		}
	}

	if !existsIn(t, pool, `SELECT 1 FROM until_encoded_deletable_originals($1) WHERE recording_id = $2`, presentCopiesDerivedStore, ok) {
		t.Errorf("recording with present derived copies is not deletable, want included")
	}
	if existsIn(t, pool, `SELECT 1 FROM until_encoded_deletable_originals($1) WHERE recording_id = $2`, presentCopiesDerivedStore, missing) {
		t.Errorf("recording with missing thumbnail is deletable, want excluded")
	}
}

func TestPresentCopies_StorageTransferCandidateExcludesMissingSource(t *testing.T) {
	pool := testutil.SetupDB(t)
	_, presentAsset := presentCopiesRecording(t, pool, 32678, 5168, false)
	_, missingAsset := presentCopiesRecording(t, pool, 1, 1, true)
	rules := `[{"store":"` + presentCopiesFSStore + `"}]`
	fsStores := []string{presentCopiesFSStore, dbtest.StoreName}

	check := func(assetID int64) bool {
		var ok bool
		if err := pool.QueryRow(t.Context(),
			`SELECT storage_transfer_candidate($1, $2::jsonb, $3::text[], $4)`,
			assetID, rules, fsStores, presentCopiesDerivedStore).Scan(&ok); err != nil {
			t.Fatalf("storage_transfer_candidate(%d): %v", assetID, err)
		}
		return ok
	}
	if !check(presentAsset) {
		t.Errorf("present original is not a transfer candidate, want candidate")
	}
	if check(missingAsset) {
		t.Errorf("original with missing source is a transfer candidate, want excluded")
	}
}

func TestPresentCopies_MediaAssetCopyPrunableExcludesMissingDesiredCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	present, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missing, _ := presentCopiesRecording(t, pool, 1, 1, false)
	presentThumb := presentCopiesDerived(t, pool, present, "thumbnail", nil, dbtest.StoreName, false)
	missingThumb := presentCopiesDerived(t, pool, missing, "thumbnail", nil, dbtest.StoreName, false)
	for _, assetID := range []int64{presentThumb, missingThumb} {
		if _, err := pool.Exec(t.Context(), `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES ($1, $2, $3, 0, 'active')`, assetID, presentCopiesDerivedStore, fmt.Sprintf("desired/%d.jpg", assetID)); err != nil {
			t.Fatalf("inserting desired copy: %v", err)
		}
	}
	observePresentCopyMissing(t, pool, missingThumb, presentCopiesDerivedStore)

	check := func(assetID int64) bool {
		var copyID int64
		if err := pool.QueryRow(t.Context(),
			`SELECT id FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2`,
			assetID, dbtest.StoreName).Scan(&copyID); err != nil {
			t.Fatalf("reading retiring copy: %v", err)
		}
		var ok bool
		if err := pool.QueryRow(t.Context(),
			`SELECT media_asset_copy_prunable($1, '[]'::jsonb, $2, now() + interval '1 hour')`,
			copyID, presentCopiesDerivedStore).Scan(&ok); err != nil {
			t.Fatalf("media_asset_copy_prunable(%d): %v", copyID, err)
		}
		return ok
	}
	if !check(presentThumb) {
		t.Errorf("retiring copy with a present desired copy is not prunable, want prunable")
	}
	if check(missingThumb) {
		t.Errorf("retiring copy with a missing desired copy is prunable, want kept")
	}
}

func TestPresentCopies_OldGenerationNeedsPresentCurrentCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	present, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missing, _ := presentCopiesRecording(t, pool, 1, 1, false)
	presentThumb := presentCopiesDerived(t, pool, present, "thumbnail", nil, presentCopiesDerivedStore, false)
	missingThumb := presentCopiesDerived(t, pool, missing, "thumbnail", nil, presentCopiesDerivedStore, false)
	for _, assetID := range []int64{presentThumb, missingThumb} {
		dbtest.ReplaceDerivedMediaAssetCopy(t, pool, assetID, presentCopiesDerivedStore, fmt.Sprintf("new/%d.jpg", assetID), 1)
	}
	observePresentCopyMissing(t, pool, missingThumb, presentCopiesDerivedStore)

	if !existsIn(t, pool, `SELECT 1 FROM old_generation_media_asset_copies WHERE media_asset_id = $1`, presentThumb) {
		t.Errorf("old generation with a present current copy is not listed, want listed")
	}
	if existsIn(t, pool, `SELECT 1 FROM old_generation_media_asset_copies WHERE media_asset_id = $1`, missingThumb) {
		t.Errorf("old generation with a missing current copy is listed, want excluded")
	}
}

func containsID(ids []int64, id int64) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

// insertPresentCopiesLogoArea は局のロゴ領域を 1 件置く（ロゴの学習結果は要らない）。
func insertPresentCopiesLogoArea(t *testing.T, pool *pgxpool.Pool, networkID, serviceID int32) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO cm_logo_areas (network_id, service_id, x, y, w, h, coded_width, coded_height)
VALUES ($1, $2, 0, 0, 10, 10, 1920, 1080)`, networkID, serviceID); err != nil {
		t.Fatalf("inserting logo area: %v", err)
	}
}

// insertPresentCopiesDetection は録画に CM 検出の結果を 1 件置く。
func insertPresentCopiesDetection(t *testing.T, pool *pgxpool.Pool, recordingID int64) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO recording_cm_detections (recording_id, cm_ranges) VALUES ($1, '{[0,1000)}'::int8multirange)`, recordingID); err != nil {
		t.Fatalf("inserting CM detection for %d: %v", recordingID, err)
	}
}

func hasPresentCopiesDetection(t *testing.T, pool *pgxpool.Pool, recordingID int64) bool {
	t.Helper()
	return existsIn(t, pool, `SELECT 1 FROM recording_cm_detections WHERE recording_id = $1`, recordingID)
}

func TestPresentCopies_GetActiveOriginalForTSScanExcludesMissingOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	present, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missing, _ := presentCopiesRecording(t, pool, 1, 1, true)
	store := dbtest.StoreName

	if _, err := q.GetActiveOriginalForTSScan(t.Context(), sqlcgen.GetActiveOriginalForTSScanParams{
		StoreName: store, RecordingID: present,
	}); err != nil {
		t.Errorf("present original is not a TS scan candidate: %v, want found", err)
	}
	if _, err := q.GetActiveOriginalForTSScan(t.Context(), sqlcgen.GetActiveOriginalForTSScanParams{
		StoreName: store, RecordingID: missing,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("missing original TS scan lookup err = %v, want pgx.ErrNoRows", err)
	}
}

func TestPresentCopies_LockActiveOriginalForTSScanExcludesMissingOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	_, presentOriginal := presentCopiesRecording(t, pool, 32678, 5168, false)
	_, missingOriginal := presentCopiesRecording(t, pool, 1, 1, true)

	lock := func(assetID int64) error {
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(t.Context()) }()
		_, err = sqlcgen.New(pool).WithTx(tx).LockActiveOriginalForTSScan(t.Context(), sqlcgen.LockActiveOriginalForTSScanParams{
			StoreName: dbtest.StoreName, MediaAssetID: assetID,
		})
		return err
	}
	if err := lock(presentOriginal); err != nil {
		t.Errorf("lock present original: %v, want locked", err)
	}
	if err := lock(missingOriginal); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("lock missing original err = %v, want pgx.ErrNoRows", err)
	}
}

func TestPresentCopies_GetCMRetryOriginalExcludesMissingOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	present, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missing, _ := presentCopiesRecording(t, pool, 1, 1, true)
	store := dbtest.StoreName

	gotPresent, err := q.GetCMRetryOriginal(t.Context(), sqlcgen.GetCMRetryOriginalParams{RecordingID: present, StoreName: &store})
	if err != nil {
		t.Fatalf("GetCMRetryOriginal(present): %v", err)
	}
	gotMissing, err := q.GetCMRetryOriginal(t.Context(), sqlcgen.GetCMRetryOriginalParams{RecordingID: missing, StoreName: &store})
	if err != nil {
		t.Fatalf("GetCMRetryOriginal(missing): %v", err)
	}
	if !gotPresent || gotMissing {
		t.Errorf("GetCMRetryOriginal present/missing = %v/%v, want true/false", gotPresent, gotMissing)
	}
}

func TestPresentCopies_GetCMLogoAnalysisRecordingExcludesMissingOnlyStation(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	present, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	_, _ = presentCopiesRecording(t, pool, 1, 1, true)
	insertPresentCopiesLogoArea(t, pool, 32678, 5168)
	insertPresentCopiesLogoArea(t, pool, 1, 1)
	store := dbtest.StoreName

	got, err := q.GetCMLogoAnalysisRecording(t.Context(), sqlcgen.GetCMLogoAnalysisRecordingParams{NetworkID: 32678, ServiceID: 5168, StoreName: &store})
	if err != nil {
		t.Fatalf("GetCMLogoAnalysisRecording(present station): %v", err)
	}
	if got.ID != present {
		t.Errorf("analysis recording = %d, want %d", got.ID, present)
	}
	if _, err := q.GetCMLogoAnalysisRecording(t.Context(), sqlcgen.GetCMLogoAnalysisRecordingParams{NetworkID: 1, ServiceID: 1, StoreName: &store}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("missing-only station analysis err = %v, want pgx.ErrNoRows", err)
	}
}

func TestPresentCopies_DeleteCMDetectionsForStationKeepsMissingOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	present, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missing, _ := presentCopiesRecording(t, pool, 1, 1, true)
	insertPresentCopiesDetection(t, pool, present)
	insertPresentCopiesDetection(t, pool, missing)
	store := dbtest.StoreName

	// 実体無しの局を先に消す。そこで消えてしまうと、後の present 側の結果とは区別がつかない。
	if err := q.DeleteCMDetectionsForStationWithActiveOriginal(t.Context(), sqlcgen.DeleteCMDetectionsForStationWithActiveOriginalParams{
		NetworkID: 1, ServiceID: 1, StoreName: &store,
	}); err != nil {
		t.Fatalf("delete for missing-only station: %v", err)
	}
	if !hasPresentCopiesDetection(t, pool, missing) {
		t.Errorf("detection of missing-only station was deleted, want kept")
	}
	if err := q.DeleteCMDetectionsForStationWithActiveOriginal(t.Context(), sqlcgen.DeleteCMDetectionsForStationWithActiveOriginalParams{
		NetworkID: 32678, ServiceID: 5168, StoreName: &store,
	}); err != nil {
		t.Fatalf("delete for present station: %v", err)
	}
	if hasPresentCopiesDetection(t, pool, present) {
		t.Errorf("detection of present station was kept, want deleted")
	}
}

func TestPresentCopies_ListCMLogoStatesCountsOnlyPresentOriginals(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	// 局 A: 古い録画は実体有り、新しい録画は実体無し。どちらも CM 検出済み。
	presentA, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missingA, _ := presentCopiesRecording(t, pool, 32678, 5168, true)
	insertPresentCopiesDetection(t, pool, presentA)
	insertPresentCopiesDetection(t, pool, missingA)
	// 局 B: 実体無しの録画しか無い。
	missingB, _ := presentCopiesRecording(t, pool, 1, 1, true)
	insertPresentCopiesDetection(t, pool, missingB)
	store := dbtest.StoreName

	rows, err := q.ListCMLogoStates(t.Context(), &store)
	if err != nil {
		t.Fatalf("ListCMLogoStates: %v", err)
	}
	byStation := map[[2]int32]sqlcgen.ListCMLogoStatesRow{}
	for _, row := range rows {
		byStation[[2]int32{row.NetworkID, row.ServiceID}] = row
	}
	a, okA := byStation[[2]int32{32678, 5168}]
	b, okB := byStation[[2]int32{1, 1}]
	if !okA || !okB {
		t.Fatalf("stations in ListCMLogoStates = %v, want both", byStation)
	}
	if a.RedetectableCount != 1 {
		t.Errorf("station A redetectable_count = %d, want 1 (the present recording only)", a.RedetectableCount)
	}
	if a.FrameRecordingID != presentA {
		t.Errorf("station A frame_recording_id = %d, want %d (newest present original)", a.FrameRecordingID, presentA)
	}
	if b.RedetectableCount != 0 {
		t.Errorf("station B redetectable_count = %d, want 0", b.RedetectableCount)
	}
	if b.FrameRecordingID != 0 {
		t.Errorf("station B frame_recording_id = %d, want 0 (no present original)", b.FrameRecordingID)
	}
}

func TestPresentCopies_SetRecordingCMDetectionExcludesMissingOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	present, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missing, _ := presentCopiesRecording(t, pool, 1, 1, true)

	// 方向 1: 凍結済みの policy が無い録画（INSERT ... SELECT の経路）。
	if rows, err := q.SetRecordingCMDetection(t.Context(), missing); err != nil || rows != 0 {
		t.Errorf("SetRecordingCMDetection(missing, no policy) = %d, %v; want 0, nil", rows, err)
	}
	if existsIn(t, pool, `SELECT 1 FROM recording_encode_policy WHERE recording_id = $1`, missing) {
		t.Errorf("missing recording got an encode policy row, want none")
	}
	if rows, err := q.SetRecordingCMDetection(t.Context(), present); err != nil || rows != 1 {
		t.Errorf("SetRecordingCMDetection(present, no policy) = %d, %v; want 1, nil", rows, err)
	}

	// 方向 2: 凍結済みの policy があり cm_detect が false の録画（ON CONFLICT の経路）。
	frozenPresent, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	frozenMissing, _ := presentCopiesRecording(t, pool, 1, 1, true)
	setPresentCopiesPolicy(t, pool, frozenPresent, "always", []string{}, false)
	setPresentCopiesPolicy(t, pool, frozenMissing, "always", []string{}, false)
	if rows, err := q.SetRecordingCMDetection(t.Context(), frozenMissing); err != nil || rows != 0 {
		t.Errorf("SetRecordingCMDetection(missing, frozen) = %d, %v; want 0, nil", rows, err)
	}
	if existsIn(t, pool, `SELECT 1 FROM recording_encode_policy WHERE recording_id = $1 AND cm_detect`, frozenMissing) {
		t.Errorf("missing recording with frozen policy got cm_detect = true, want false")
	}
	if rows, err := q.SetRecordingCMDetection(t.Context(), frozenPresent); err != nil || rows != 1 {
		t.Errorf("SetRecordingCMDetection(present, frozen) = %d, %v; want 1, nil", rows, err)
	}
	if !existsIn(t, pool, `SELECT 1 FROM recording_encode_policy WHERE recording_id = $1 AND cm_detect`, frozenPresent) {
		t.Errorf("present recording with frozen policy did not get cm_detect = true")
	}
}

func TestPresentCopies_PrepareMediaAssetTransferSkipsMissingNewestSource(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	_, asset := presentCopiesRecording(t, pool, 32678, 5168, false)
	presentCopyID := dbtest.MediaAssetCopyID(t, pool, asset, dbtest.StoreName)
	// 新しい方の転送元は実体無し。選択がこれを拾うと、実体有りの古い方を選べない。
	const newerStore = "fs-newer"
	if _, err := pool.Exec(t.Context(), `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state, created_at)
VALUES ($1, $2, 'newer.ts', 0, 'active', now() + interval '1 second')`, asset, newerStore); err != nil {
		t.Fatalf("inserting newer source copy: %v", err)
	}
	observePresentCopyMissing(t, pool, asset, newerStore)

	got, err := q.PrepareMediaAssetTransfer(t.Context(), sqlcgen.PrepareMediaAssetTransferParams{
		OriginalRules: json.RawMessage(`[{"store":"` + presentCopiesFSStore + `"}]`),
		DerivedStore:  presentCopiesDerivedStore,
		MediaAssetID:  asset,
		TargetStore:   presentCopiesFSStore,
		SourceStores:  []string{dbtest.StoreName, newerStore, presentCopiesFSStore},
		FsStores:      []string{dbtest.StoreName, newerStore, presentCopiesFSStore},
		MountedStores: []string{},
		AttemptLimit:  int64(10),
		CopyToken:     "tok",
	})
	if err != nil {
		t.Fatalf("PrepareMediaAssetTransfer: %v", err)
	}
	if got.SourceStore != dbtest.StoreName || got.SourceCopyID != presentCopyID {
		t.Errorf("transfer source = %s/%d, want present %s/%d (missing newer source skipped)",
			got.SourceStore, got.SourceCopyID, dbtest.StoreName, presentCopyID)
	}
}

func TestPresentCopies_ListMediaAssetTransferCandidatesSourceStoreSkipsMissing(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	_, asset := presentCopiesRecording(t, pool, 32678, 5168, false)
	const newerStore = "fs-newer"
	if _, err := pool.Exec(t.Context(), `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state, created_at)
VALUES ($1, $2, 'newer.ts', 0, 'active', now() + interval '1 second')`, asset, newerStore); err != nil {
		t.Fatalf("inserting newer source copy: %v", err)
	}
	observePresentCopyMissing(t, pool, asset, newerStore)

	got, err := q.ListMediaAssetTransferCandidates(t.Context(), sqlcgen.ListMediaAssetTransferCandidatesParams{
		OriginalRules: json.RawMessage(`[{"store":"` + presentCopiesFSStore + `"}]`),
		DerivedStore:  presentCopiesDerivedStore,
		FsStores:      []string{dbtest.StoreName, newerStore, presentCopiesFSStore},
		AttemptLimit:  10,
		RowLimit:      100,
	})
	if err != nil {
		t.Fatalf("ListMediaAssetTransferCandidates: %v", err)
	}
	for _, row := range got {
		if row.MediaAssetID == asset {
			if row.SourceStore != dbtest.StoreName {
				t.Errorf("source_store = %q, want %q (missing newer copy skipped)", row.SourceStore, dbtest.StoreName)
			}
			return
		}
	}
	t.Errorf("asset %d is not a transfer candidate, want listed", asset)
}

func TestPresentCopies_MarkMediaAssetCopyCorruptDeletingNeedsPresentOriginal(t *testing.T) {
	for _, tc := range []struct {
		name        string
		origMissing bool
		wantRows    int64
		wantState   string
	}{
		{name: "present original allows retiring the derived copy", origMissing: false, wantRows: 1, wantState: "deleting"},
		{name: "missing original does not allow it", origMissing: true, wantRows: 0, wantState: "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			recordingID, _ := presentCopiesRecording(t, pool, 32678, 5168, tc.origMissing)
			thumb := presentCopiesDerived(t, pool, recordingID, "thumbnail", nil, presentCopiesDerivedStore, false)
			copyID := dbtest.MediaAssetCopyID(t, pool, thumb, presentCopiesDerivedStore)

			rows, err := sqlcgen.New(pool).MarkMediaAssetCopyCorruptDeleting(t.Context(), sqlcgen.MarkMediaAssetCopyCorruptDeletingParams{
				CopyID: copyID, MediaAssetID: thumb, Store: presentCopiesDerivedStore, RecordingID: recordingID,
			})
			if err != nil {
				t.Fatalf("MarkMediaAssetCopyCorruptDeleting: %v", err)
			}
			if rows != tc.wantRows {
				t.Errorf("rows = %d, want %d", rows, tc.wantRows)
			}
			var state string
			if err := pool.QueryRow(t.Context(), `SELECT state FROM media_asset_copies WHERE id = $1`, copyID).Scan(&state); err != nil {
				t.Fatalf("reading copy state: %v", err)
			}
			if state != tc.wantState {
				t.Errorf("derived copy state = %q, want %q", state, tc.wantState)
			}
		})
	}
}

func TestPresentCopies_ThumbnailReselectSkipsMissingOriginalAndEncoded(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	profile := "p1"
	store := dbtest.StoreName
	derived := presentCopiesDerivedStore

	present, presentOriginal := presentCopiesRecording(t, pool, 32678, 5168, false)
	presentCopiesDerived(t, pool, present, "thumbnail", nil, derived, false)
	presentEncoded := presentCopiesDerived(t, pool, present, "encoded", &profile, derived, false)
	insertPresentCopiesDetection(t, pool, present)

	missing, _ := presentCopiesRecording(t, pool, 1, 1, true)
	presentCopiesDerived(t, pool, missing, "thumbnail", nil, derived, false)
	presentCopiesDerived(t, pool, missing, "encoded", &profile, derived, true)
	insertPresentCopiesDetection(t, pool, missing)

	rows, err := q.ListThumbnailReselectCandidates(t.Context(), sqlcgen.ListThumbnailReselectCandidatesParams{
		AfterRecordingID: 0, RowLimit: 100, OriginalStore: &store, DerivedStore: derived,
	})
	if err != nil {
		t.Fatalf("ListThumbnailReselectCandidates: %v", err)
	}
	byRecording := map[int64]sqlcgen.ListThumbnailReselectCandidatesRow{}
	for _, row := range rows {
		byRecording[row.RecordingID] = row
	}
	p, okP := byRecording[present]
	m, okM := byRecording[missing]
	if !okP || !okM {
		t.Fatalf("reselect candidates = %v, want both recordings (the query does not drop rows for a missing original)", byRecording)
	}
	if p.OriginalMediaAssetID != presentOriginal {
		t.Errorf("present original id = %d, want %d", p.OriginalMediaAssetID, presentOriginal)
	}
	var presentEncodedAssets, missingEncodedAssets []json.RawMessage
	if err := json.Unmarshal(p.EncodedAssets, &presentEncodedAssets); err != nil || len(presentEncodedAssets) != 1 {
		t.Errorf("present encoded assets = %s, want 1 entry (%d)", p.EncodedAssets, presentEncoded)
	}
	if m.OriginalMediaAssetID != 0 {
		t.Errorf("missing original id = %d, want 0 (not usable as the reselect input)", m.OriginalMediaAssetID)
	}
	if err := json.Unmarshal(m.EncodedAssets, &missingEncodedAssets); err != nil || len(missingEncodedAssets) != 0 {
		t.Errorf("missing encoded assets = %s, want []", m.EncodedAssets)
	}
}

func TestPresentCopies_ListMissingSeekTilesExcludesMissingOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	present, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	missing, _ := presentCopiesRecording(t, pool, 1, 1, true)
	store := dbtest.StoreName

	got, err := q.ListMissingSeekTilesRecordings(t.Context(), sqlcgen.ListMissingSeekTilesRecordingsParams{
		OriginalStore: &store, AfterRecordingID: 0, DerivedStore: presentCopiesDerivedStore, RowLimit: 100,
	})
	if err != nil {
		t.Fatalf("ListMissingSeekTilesRecordings: %v", err)
	}
	if !containsID(got, present) || containsID(got, missing) {
		t.Errorf("seek tiles candidates = %v, want present %d in and missing %d out", got, present, missing)
	}
}

func TestPresentCopies_IsActiveThumbnailInputNeedsPresentOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	_, presentOriginal := presentCopiesRecording(t, pool, 32678, 5168, false)
	_, missingOriginal := presentCopiesRecording(t, pool, 1, 1, true)

	gotPresent, err := q.IsActiveThumbnailInput(t.Context(), sqlcgen.IsActiveThumbnailInputParams{MediaAssetID: presentOriginal, StoreName: dbtest.StoreName})
	if err != nil {
		t.Fatalf("IsActiveThumbnailInput(present): %v", err)
	}
	gotMissing, err := q.IsActiveThumbnailInput(t.Context(), sqlcgen.IsActiveThumbnailInputParams{MediaAssetID: missingOriginal, StoreName: dbtest.StoreName})
	if err != nil {
		t.Fatalf("IsActiveThumbnailInput(missing): %v", err)
	}
	if !gotPresent || gotMissing {
		t.Errorf("IsActiveThumbnailInput present/missing = %v/%v, want true/false", gotPresent, gotMissing)
	}
}

// 以下は除かない側（view の現状の挙動の固定）。実体無しを「有る」と数えることを、
// 実体有りの対照と並べて確かめる。判断の理由は各 view の COMMENT にある。

func requestPresentCopiesEncodedRebuild(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
VALUES ($1, $2, '{[0,1000)}'::int8multirange)`, recordingID, profile); err != nil {
		t.Fatalf("inserting rebuild request: %v", err)
	}
}

// presentCopiesEncodedWithCut は requested_keep と同じ区間のカット版を作る。
func presentCopiesEncodedWithCut(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile string, missing bool) {
	t.Helper()
	assetID := presentCopiesDerived(t, pool, recordingID, "encoded", &profile, presentCopiesDerivedStore, missing)
	if _, err := pool.Exec(t.Context(), `
INSERT INTO media_asset_cuts (media_asset_id, keep_ranges) VALUES ($1, '{[0,1000)}'::int8multirange)`, assetID); err != nil {
		t.Fatalf("inserting cut for %d: %v", assetID, err)
	}
}

func TestPresentCopies_UnsatisfiedEncodeRebuildsCountsMissingEncodedAsPresent(t *testing.T) {
	pool := testutil.SetupDB(t)
	profile := "p1"
	none, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	presentEncoded, _ := presentCopiesRecording(t, pool, 1, 1, false)
	missingEncoded, _ := presentCopiesRecording(t, pool, 2, 2, false)
	for _, rec := range []int64{none, presentEncoded, missingEncoded} {
		requestPresentCopiesEncodedRebuild(t, pool, rec, profile)
	}
	presentCopiesEncodedWithCut(t, pool, presentEncoded, profile, false)
	presentCopiesEncodedWithCut(t, pool, missingEncoded, profile, true)

	if !existsIn(t, pool, `SELECT 1 FROM unsatisfied_encode_rebuilds WHERE recording_id = $1`, none) {
		t.Errorf("rebuild request without an encoded version is not unsatisfied, want listed")
	}
	if existsIn(t, pool, `SELECT 1 FROM unsatisfied_encode_rebuilds WHERE recording_id = $1`, presentEncoded) {
		t.Errorf("rebuild request with a present matching encoded is unsatisfied, want not listed")
	}
	// 現状の挙動の固定: 実体無しの encoded も「有る」と数える。作り直しの根拠にしない。
	if existsIn(t, pool, `SELECT 1 FROM unsatisfied_encode_rebuilds WHERE recording_id = $1`, missingEncoded) {
		t.Errorf("rebuild request with a missing matching encoded is unsatisfied, want not listed (missing counts as present)")
	}
}

func TestPresentCopies_RemovedEncodedAssetsKeepsMissingAsRemaining(t *testing.T) {
	pool := testutil.SetupDB(t)
	keep := "p2"
	removed := "p1"
	removedAsset := func(recordingID int64, missing bool) int64 {
		return presentCopiesDerived(t, pool, recordingID, "encoded", &removed, presentCopiesDerivedStore, missing)
	}
	requestRemoval := func(recordingID int64) {
		if _, err := pool.Exec(t.Context(), `INSERT INTO encoded_asset_removal_requests (recording_id, profile) VALUES ($1, $2)`, recordingID, removed); err != nil {
			t.Fatalf("inserting removal request: %v", err)
		}
	}

	// 対照: 外した版も残る版も実体有り。
	control, _ := presentCopiesRecording(t, pool, 32678, 5168, false)
	setPresentCopiesPolicy(t, pool, control, "always", []string{keep}, false)
	controlAsset := removedAsset(control, false)
	presentCopiesDerived(t, pool, control, "encoded", &keep, presentCopiesDerivedStore, false)
	requestRemoval(control)

	// 外した版自身が実体無し。view は外した側の実体を見ないので列挙される。
	selfMissing, _ := presentCopiesRecording(t, pool, 1, 1, false)
	setPresentCopiesPolicy(t, pool, selfMissing, "always", []string{keep}, false)
	selfMissingAsset := removedAsset(selfMissing, true)
	presentCopiesDerived(t, pool, selfMissing, "encoded", &keep, presentCopiesDerivedStore, false)
	requestRemoval(selfMissing)

	// 残る版が実体無しの原本だけ。現状は実体無しの原本も「残る版」として数える。
	originalOnly, _ := presentCopiesRecording(t, pool, 2, 2, true)
	setPresentCopiesPolicy(t, pool, originalOnly, "always", []string{}, false)
	originalOnlyAsset := removedAsset(originalOnly, false)
	requestRemoval(originalOnly)

	for name, id := range map[string]int64{
		"control (both present)":             controlAsset,
		"removed version missing":            selfMissingAsset,
		"only remaining is missing original": originalOnlyAsset,
	} {
		if !existsIn(t, pool, `SELECT 1 FROM removed_encoded_assets WHERE asset_id = $1`, id) {
			t.Errorf("%s: removed asset %d is not listed, want listed (current behavior)", name, id)
		}
	}
}
