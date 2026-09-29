package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestGetRecording_EncodedAssetCutAndStale は受け入れの「編集後に『編集前の
// 内容です』になり、判定が量子化後の値どうしで行われる」を API の経路
// （SQL の射影 → 導出）で固定する。
//
// 印は原本時間軸の ms で保存され、keep 区間は chapters.Derive がフレーム境界へ
// 量子化して作る。**量子化前の値どうしで比べる形に壊すと、1 フレーム以内のずれを
// 見逃す**（下の (c) がその向きを押さえる）。
func TestGetRecording_EncodedAssetCutAndStale(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{
		Pool:               pool,
		EncodeProfileNames: []string{"h264", "cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)

	base := time.Now().Truncate(time.Second)
	id := seedRecording(t, pool, "カット版", base, "finished", 1)
	seedIngested(t, pool, id, 1000, nil)
	// KeepRanges の終端は program_duration_ms（Derive の end）。
	if _, err := pool.Exec(context.Background(),
		`UPDATE recordings SET program_duration_ms = 60000 WHERE id = $1`, id); err != nil {
		t.Fatalf("setting duration: %v", err)
	}
	setEncodeProfiles(t, pool, id, []string{"h264", "cut"})

	q := sqlcgen.New(pool)
	// cut でない版（凍結区間なし）と cut 版（凍結区間あり）。
	h264 := "h264"
	if _, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id, Kind: db.AssetKindEncoded, Profile: &h264, RelPath: "a_h264.mp4", SizeBytes: 50,
	}); err != nil {
		t.Fatalf("seed h264: %v", err)
	}
	cut := "cut"
	assetID, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id, Kind: db.AssetKindEncoded, Profile: &cut, RelPath: "a_cut.g1.mp4", SizeBytes: 40,
	})
	if err != nil {
		t.Fatalf("seed cut: %v", err)
	}

	// ユーザー層: 10-20 秒を CM として切る。
	userSpans := []chapters.Span{{StartMs: 10000, EndMs: 20000, Label: "CM", Cut: true}}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, id); err != nil {
		t.Fatalf("adopting chapters: %v", err)
	}
	insertChapterSpan(t, pool, id, userSpans[0])
	onDayOne := chapters.KeepRanges(chapters.Derive(true, userSpans, nil, 60000))

	// (a) 凍結区間が現在のタイムラインと一致 → cut=true / cutStale=false。
	setFrozenCutsText(t, pool, assetID, onDayOne)
	rec := getRecordingForTest(t, srv.URL, id)
	assertCutAsset(t, rec, false)

	// (b) チャプターを直す（もう 1 本 CM を足す）→ 凍結区間が古くなる。
	insertChapterSpan(t, pool, id, chapters.Span{StartMs: 30000, EndMs: 40000, Label: "CM", Cut: true})
	rec = getRecordingForTest(t, srv.URL, id)
	assertCutAsset(t, rec, true)

	// (c) 1 フレーム以内のずれも「編集前」として拾う。印をちょうど 1 フレーム
	// ぶんずらして凍結すると、量子化後の値どうしの比較では一致しない。
	shifted := make([]chapters.Range, len(onDayOne))
	copy(shifted, onDayOne)
	shifted[0].EndMs += 34 // 1 フレーム ≈ 33.4ms
	setFrozenCutsText(t, pool, assetID, shifted)
	rec = getRecordingForTest(t, srv.URL, id)
	assertCutAsset(t, rec, true)
}

func insertChapterSpan(t *testing.T, pool *pgxpool.Pool, recordingID int64, s chapters.Span) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO recording_chapter_spans (recording_id, span, label, cut)
		 VALUES ($1, int8range($2, $3), $4, $5)`, recordingID, s.StartMs, s.EndMs, s.Label, s.Cut); err != nil {
		t.Fatalf("seeding chapter span: %v", err)
	}
}

// setFrozenCutsText は media_asset_cuts を SQL のリテラルで直接書く。
// **worker の Go 側エンコーダを通さない** --- 期待値を作る側が実装を呼ぶと、
// 実装が壊れたときにテストが一緒に壊れる。
func setFrozenCutsText(t *testing.T, pool *pgxpool.Pool, assetID int64, keep []chapters.Range) {
	t.Helper()
	parts := make([]string, 0, len(keep))
	for _, r := range keep {
		parts = append(parts, fmt.Sprintf("[%d,%d)", r.StartMs, r.EndMs))
	}
	literal := "{" + strings.Join(parts, ",") + "}"
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM media_asset_cuts WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("clearing media_asset_cuts: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO media_asset_cuts (media_asset_id, keep_ranges) VALUES ($1, $2::int8multirange)`,
		assetID, literal); err != nil {
		t.Fatalf("inserting media_asset_cuts %s: %v", literal, err)
	}
}

func getRecordingForTest(t *testing.T, base string, id int64) Recording {
	t.Helper()
	var rec Recording
	if resp := getJSON(t, fmt.Sprintf("%s/api/recordings/%d", base, id), &rec); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	return rec
}

// assertCutAsset は cut 版と cut でない版が期待どおりに見えることを確かめる。
func assertCutAsset(t *testing.T, rec Recording, wantStale bool) {
	t.Helper()
	if rec.EncodedAssets == nil {
		t.Fatal("encodedAssets is nil")
	}
	byProfile := map[string]EncodedAsset{}
	for _, a := range *rec.EncodedAssets {
		byProfile[a.Profile] = a
	}
	h264, ok := byProfile["h264"]
	if !ok {
		t.Fatalf("h264 asset missing: %+v", *rec.EncodedAssets)
	}
	if h264.Cut != nil && *h264.Cut {
		t.Error("the non-cut asset was reported as cut")
	}
	if h264.CutStale != nil {
		t.Error("cutStale must be omitted for a non-cut asset")
	}
	cut, ok := byProfile["cut"]
	if !ok {
		t.Fatalf("cut asset missing: %+v", *rec.EncodedAssets)
	}
	if cut.Cut == nil || !*cut.Cut {
		t.Error("the cut asset was not reported as cut")
	}
	if cut.CutStale == nil {
		t.Fatal("cutStale must be present for a cut asset")
	}
	if *cut.CutStale != wantStale {
		t.Errorf("cutStale = %v, want %v", *cut.CutStale, wantStale)
	}
}
