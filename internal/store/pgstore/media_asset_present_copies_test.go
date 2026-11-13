package pgstore

import (
	"fmt"
	"testing"

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
