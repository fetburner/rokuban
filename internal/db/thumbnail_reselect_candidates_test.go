package db_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// reselectFixture は再選択候補の録画 1 件の状態を表す。
type reselectFixture struct {
	// detected は CM 検出の行を持つ。
	detected bool
	// owned は章の所有の行を持つ。
	owned bool
	// trash はごみ箱へ入れる。
	trash bool
	// noThumbnail はサムネイルの資産を作らない。
	noThumbnail bool
}

// seedReselectRecording は f の状態の録画を 1 件作り、その id を返す。
func seedReselectRecording(t *testing.T, db sqlcgen.DBTX, f reselectFixture) int64 {
	t.Helper()
	ctx := t.Context()
	q := sqlcgen.New(db)
	id := dbtest.Recording(t, db)
	if !f.noThumbnail {
		dbtest.MediaAsset(t, db, id, func(p *dbtest.MediaAssetParams) {
			p.Kind = "thumbnail"
			p.RelPath = fmt.Sprintf("thumbnails/%d.jpg", id)
		})
	}
	if f.detected {
		testutil.RequireNoError(t, q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
			RecordingID: id, CmRanges: "{[0,60000)}",
		}), "saving CM detection")
	}
	if f.owned {
		dbtest.AdoptChapters(t, db, id)
	}
	if f.trash {
		_, err := q.SoftDeleteRecording(ctx, id)
		testutil.RequireNoError(t, err, "soft deleting recording")
	}
	return id
}

// TestListThumbnailReselectCandidates は、既存サムネイルの再選択候補の WHERE 句
// （ごみ箱の除外、サムネイルの存在、CM 検出か章の所有のどちらか）を、条件 1 つずつの表で固定する。
// 各ケースは録画 1 件の状態を変えて、候補に含まれるかを見る。同じデータベースに
// 完備な対照録画（検出と所有の両方あり・サムネイルあり・ごみ箱でない）を 1 件別に作り、毎回含まれることを確かめる。
// 対照は検出と所有の両方を持つので、片方だけを残す変異は対照では落ちず、そのケース（Detected_Listed や
// Owned_Listed）の期待値で落ちる。対照が含まれなければ、クエリが空を返す変異を、ケースの除外と区別できないため。
// 窓（recording_id > cursor）と行数の上限は worker の窓のテストが検査する。
func TestListThumbnailReselectCandidates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		fixture    reselectFixture
		wantListed bool
	}{
		{name: "Detected_Listed", fixture: reselectFixture{detected: true}, wantListed: true},
		{name: "Owned_Listed", fixture: reselectFixture{owned: true}, wantListed: true},
		{name: "NeitherDetectedNorOwned_NotListed", fixture: reselectFixture{detected: false, owned: false}, wantListed: false},
		{name: "Trashed_NotListed", fixture: reselectFixture{detected: true, trash: true}, wantListed: false},
		{name: "NoThumbnail_NotListed", fixture: reselectFixture{detected: true, noThumbnail: true}, wantListed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			q := sqlcgen.New(pool)
			control := seedReselectRecording(t, pool, reselectFixture{detected: true, owned: true})
			recordingID := seedReselectRecording(t, pool, tc.fixture)

			rows, err := q.ListThumbnailReselectCandidates(t.Context(), sqlcgen.ListThumbnailReselectCandidatesParams{
				RowLimit: 100, DerivedStore: dbtest.StoreName,
			})
			testutil.RequireNoError(t, err, "ListThumbnailReselectCandidates")
			listed := make([]int64, 0, len(rows))
			for _, row := range rows {
				listed = append(listed, row.RecordingID)
			}
			if !slices.Contains(listed, control) {
				t.Fatalf("candidates = %v, want control recording %d (the query must list a complete recording)", listed, control)
			}
			if got := slices.Contains(listed, recordingID); got != tc.wantListed {
				t.Errorf("recording %d listed = %v, want %v (candidates %v)", recordingID, got, tc.wantListed, listed)
			}
		})
	}
}
