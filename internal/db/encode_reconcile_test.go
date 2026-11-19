package db_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// encodeKnownProfiles は、設定に存在するプロファイル（テストの設定）。
// encodeAttemptLimit と encodeRowLimit は本番と同じ値（jobs.EncodeAttemptLimit と既定の窓の大きさ）。
var encodeKnownProfiles = []string{"h264", "h265"}

const (
	encodeAttemptLimit = 25
	encodeRowLimit     = 1000
)

// seedMissingEncodeBase は、不足プロファイルの候補になる録画を 1 件作る。
// 既定の状態は、原本が active（keep_original = always）、凍結済みの desired は h265 1 件、
// encoded は無し。ケースはこの状態から条件を 1 つ崩す。
func seedMissingEncodeBase(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	id := dbtest.Recording(t, pool)
	dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) { p.Kind = "original" })
	dbtest.ProcessingPolicy(t, pool, id, func(p *dbtest.ProcessingPolicyParams) {
		p.KeepOriginal = "always"
		p.EncodeProfiles = []string{"h265"}
	})
	return id
}

// listMissingEncode は ListMissingEncodeProfiles を、継続カーソル・録画指定なし・cut なしで呼ぶ。
func listMissingEncode(t *testing.T, pool *pgxpool.Pool, afterRecordingID int64) []sqlcgen.ListMissingEncodeProfilesRow {
	t.Helper()
	got, err := sqlcgen.New(pool).ListMissingEncodeProfiles(t.Context(), sqlcgen.ListMissingEncodeProfilesParams{
		AfterRecordingID: afterRecordingID,
		RecordingID:      nil,
		KnownProfiles:    encodeKnownProfiles,
		CutProfiles:      []string{},
		AttemptLimit:     encodeAttemptLimit,
		RowLimit:         encodeRowLimit,
	})
	testutil.RequireNoError(t, err, "ListMissingEncodeProfiles")
	return got
}

// listDroppedEncode は ListUnsatisfiableEncodeProfiles が数える、落とした側の (プロファイル名 → 録画数) を返す。
func listDroppedEncode(t *testing.T, pool *pgxpool.Pool) map[string]int64 {
	t.Helper()
	rows, err := sqlcgen.New(pool).ListUnsatisfiableEncodeProfiles(t.Context(), encodeKnownProfiles)
	testutil.RequireNoError(t, err, "ListUnsatisfiableEncodeProfiles")
	got := map[string]int64{}
	for _, r := range rows {
		got[r.Profile] = r.Recordings
	}
	return got
}

// TestListMissingEncodeProfiles は、不足プロファイルの候補集合（ListMissingEncodeProfiles）と
// 落とした側の集計（ListUnsatisfiableEncodeProfiles）を、条件 1 つずつの表で固定する。
//
// 投入されないことをジョブ数で見ても意味が無いケースがある。例えば「原本が無い（ingest 未完了）」は
// 候補の定義側でも弾かれるので、クエリから条件を落としてもジョブ数のアサーションは通ってしまう。
// そこで候補集合そのものを、録画 1 件の表で見る。録画は 1 件だけなので、返った行は
// その録画の profile 名になる。
func TestListMissingEncodeProfiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// mutate は既定の状態（seedMissingEncodeBase）から崩す。nil は既定のまま。
		mutate func(t *testing.T, pool *pgxpool.Pool, recordingID int64)
		// want は候補になる profile 名。候補が無いケースは nil。
		want []string
		// dropped は ListUnsatisfiableEncodeProfiles が数える (profile → 録画数)。無いケースは nil。
		dropped map[string]int64
	}{
		{name: "MissingDesiredProfile_Listed", mutate: nil, want: []string{"h265"}, dropped: nil},
		{name: "CompleteDesiredProfile_NotListed", mutate: func(t *testing.T, pool *pgxpool.Pool, id int64) {
			profile := "h265"
			dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) {
				p.Kind = "encoded"
				p.Profile = &profile
			})
		}, want: nil, dropped: nil},
		{name: "NoProcessingPolicyRow_NotListed", mutate: func(t *testing.T, pool *pgxpool.Pool, id int64) {
			testutil.MustExec(t, pool, "deleting processing policy",
				`DELETE FROM recording_processing_policy WHERE recording_id = $1`, id)
		}, want: nil, dropped: nil},
		{name: "IngestIncomplete_NotListed", mutate: func(t *testing.T, pool *pgxpool.Pool, id int64) {
			testutil.MustExec(t, pool, "deleting media asset copies",
				`DELETE FROM media_asset_copies WHERE media_asset_id IN (SELECT id FROM media_assets WHERE recording_id = $1)`, id)
			testutil.MustExec(t, pool, "deleting media asset ts scans",
				`DELETE FROM media_asset_ts_scans WHERE media_asset_id IN (SELECT id FROM media_assets WHERE recording_id = $1)`, id)
			testutil.MustExec(t, pool, "deleting media assets",
				`DELETE FROM media_assets WHERE recording_id = $1`, id)
		}, want: nil, dropped: nil},
		{name: "OriginalPhysicallyDeleted_NotListed", mutate: func(t *testing.T, pool *pgxpool.Pool, id int64) {
			dbtest.SetRecordingMediaAssetState(t, pool, id, "original", "deleted")
		}, want: nil, dropped: nil},
		{name: "TrashedRecording_NotListed", mutate: func(t *testing.T, pool *pgxpool.Pool, id int64) {
			testutil.MustExec(t, pool, "marking recording deleted",
				`UPDATE recordings SET deleted_at = now() WHERE id = $1`, id)
		}, want: nil, dropped: nil},
		{name: "DeletedEncodedAsset_Listed", mutate: func(t *testing.T, pool *pgxpool.Pool, id int64) {
			profile := "h265"
			dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) {
				p.Kind = "encoded"
				p.Profile = &profile
			})
			dbtest.SetRecordingMediaAssetState(t, pool, id, "encoded", "deleted")
		}, want: []string{"h265"}, dropped: nil},
		{name: "ProfileMissingFromConfig_NotListed", mutate: func(t *testing.T, pool *pgxpool.Pool, id int64) {
			testutil.MustExec(t, pool, "renaming desired profile",
				`UPDATE recording_processing_policy SET encode_profiles = $2 WHERE recording_id = $1`, id, []string{"gone"})
		}, want: nil, dropped: map[string]int64{"gone": 1}},
		{name: "EmptyProfileName_NotListed", mutate: func(t *testing.T, pool *pgxpool.Pool, id int64) {
			testutil.MustExec(t, pool, "setting empty desired profile",
				`UPDATE recording_processing_policy SET encode_profiles = $2 WHERE recording_id = $1`, id, []string{""})
		}, want: nil, dropped: map[string]int64{"": 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			id := seedMissingEncodeBase(t, pool)
			if tc.mutate != nil {
				tc.mutate(t, pool, id)
			}

			got := listMissingEncode(t, pool, 0)
			var gotProfiles []string
			for _, r := range got {
				if r.RecordingID != id {
					t.Fatalf("candidate recording = %d, want only %d (rows %v)", r.RecordingID, id, got)
				}
				if r.IsRebuild {
					t.Errorf("candidate (%d, %q) is a rebuild, want a missing profile", r.RecordingID, r.Profile)
				}
				gotProfiles = append(gotProfiles, r.Profile)
			}
			if !slices.Equal(gotProfiles, tc.want) {
				t.Errorf("missing encode profiles = %v, want %v", gotProfiles, tc.want)
			}

			// 落とした側は数えて見せる。黙って落とすと「エンコードされない録画」が静かに増える。
			if gotDropped := listDroppedEncode(t, pool); !maps.Equal(gotDropped, tc.dropped) {
				t.Errorf("unsatisfiable encode profiles = %v, want %v", gotDropped, tc.dropped)
			}
		})
	}
}

// TestListMissingEncodeProfiles_AfterRecordingIDSkipsEarlierRecordings は、継続カーソルの
// keyset 述語（recording_id > after_recording_id）を固定する。reconcile の窓の回転が使う。
// 候補が 2 件あるとき、0 なら両方、先頭の id を渡すと後ろの 1 件だけが返る。
func TestListMissingEncodeProfiles_AfterRecordingIDSkipsEarlierRecordings(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	first := seedMissingEncodeBase(t, pool)
	second := seedMissingEncodeBase(t, pool)
	if first >= second {
		t.Fatalf("expected first (%d) < second (%d) for the keyset assertion to mean anything", first, second)
	}

	all := listMissingEncode(t, pool, 0)
	wantAll := []sqlcgen.ListMissingEncodeProfilesRow{
		{RecordingID: first, Profile: "h265"},
		{RecordingID: second, Profile: "h265"},
	}
	if !equalMissingRows(all, wantAll) {
		t.Errorf("missing encode profiles with after=0 = %v, want %v", all, wantAll)
	}

	after := listMissingEncode(t, pool, first)
	wantAfter := []sqlcgen.ListMissingEncodeProfilesRow{{RecordingID: second, Profile: "h265"}}
	if !equalMissingRows(after, wantAfter) {
		t.Errorf("missing encode profiles with after=%d = %v, want %v", first, after, wantAfter)
	}
}

func equalMissingRows(got, want []sqlcgen.ListMissingEncodeProfilesRow) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].RecordingID != want[i].RecordingID || got[i].Profile != want[i].Profile ||
			got[i].IsRebuild != want[i].IsRebuild || !slices.Equal(got[i].RequestedKeep, want[i].RequestedKeep) {
			return false
		}
	}
	return true
}

// TestUnsatisfiedEncodeRebuilds_MatchesActiveAssetOfSameProfileOnly は、作り直し要求が
// 満たされたかを判定する view（unsatisfied_encode_rebuilds）が、同じプロファイルの active な
// encoded だけを見ることを固定する。別の cut プロファイルの版の凍結区間が requested_keep と
// 同じでも、未充足のままである（満たされた扱いにすると until_encoded の原本が不可逆に消える）。
//
// 要求は profile cut、凍結区間は [0,1000) で固定する。行は独立した録画 1 件ずつ。
// 別プロファイルだけの行は未充足（1）、同じプロファイルの行を足すと満たされる（0）。
func TestUnsatisfiedEncodeRebuilds_MatchesActiveAssetOfSameProfileOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// encodedProfiles は、凍結区間 [0,1000) を持つ active な encoded のプロファイル。
		encodedProfiles []string
		want            int
	}{
		{name: "OtherProfileMatchingKeep_StaysUnsatisfied", encodedProfiles: []string{"cut2"}, want: 1},
		{name: "SameProfileMatchingKeep_Satisfies", encodedProfiles: []string{"cut2", "cut"}, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			id := dbtest.Recording(t, pool)
			dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) { p.Kind = "original" })
			testutil.MustExec(t, pool, "requesting cut rebuild",
				`INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
				 VALUES ($1, 'cut', '{[0,1000)}'::int8multirange)`, id)
			for _, profile := range tc.encodedProfiles {
				assetID := dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) {
					p.Kind = "encoded"
					p.Profile = &profile
				})
				testutil.MustExec(t, pool, "freezing encoded keep",
					`INSERT INTO media_asset_cuts (media_asset_id, keep_ranges) VALUES ($1, '{[0,1000)}'::int8multirange)`, assetID)
			}

			var got int
			testutil.RequireNoError(t, pool.QueryRow(t.Context(),
				`SELECT count(*) FROM unsatisfied_encode_rebuilds WHERE recording_id = $1`, id).Scan(&got),
				"counting unsatisfied_encode_rebuilds")
			if got != tc.want {
				t.Fatalf("unsatisfied rebuilds with encoded profiles %v = %d, want %d", tc.encodedProfiles, got, tc.want)
			}
		})
	}
}
