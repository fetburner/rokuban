package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// seedIngested は録画に原本 media_asset と PID 別 drop_stats を付け、
// 作った原本アセットの ID を返す。
//
// 実運用の ingest（internal/worker/ingest.go の commit）は原本 media_asset の
// INSERT と同一 tx で recording_encode_policy を必ず凍結する（issue #159。
// 解決に失敗しても既定値で凍結する。resolveAndSnapshotEncodePolicy の doc
// コメント参照）。この関数は本物の ingest ワーカーを経由しないテスト用
// フィクスチャなので、同じ事後条件（原本コミット済みなら凍結済み）を
// 手で再現しておく --- これを怠ると、seedIngested 後に事後追加 API
// （AddRecordingEncodeProfiles）を呼ぶテストが「行が無い」で 500 になる。
func seedIngested(t *testing.T, pool *pgxpool.Pool, recordingID, size int64, stats map[int32][4]int64) int64 {
	t.Helper()
	ctx := context.Background()
	q := sqlcgen.New(pool)
	assetID := dbtest.OriginalAsset(t, pool, recordingID, func(fixtureParams *sqlcgen.CreateMediaAssetParams) {
		fixtureParams.RelPath = fmt.Sprintf("test/%d.m2ts", recordingID)
		fixtureParams.SizeBytes = size
	})

	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles)
		 VALUES ($1, 'always', '{}')`, recordingID); err != nil {
		t.Fatalf("seeding recording_encode_policy: %v", err)
	}
	params := make([]sqlcgen.InsertDropStatParams, 0, len(stats))
	for pid, s := range stats {
		params = append(params, sqlcgen.InsertDropStatParams{
			MediaAssetID: assetID,
			Pid:          pid,
			Packets:      s[0],
			Drops:        s[1],
			Errors:       s[2],
			Scrambled:    s[3],
		})
	}
	batch := q.InsertDropStat(ctx, params)
	batch.Exec(nil)
	if err := batch.Close(); err != nil {
		t.Fatalf("seeding drop_stat batch: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes) VALUES ($1, $2)`, assetID, size); err != nil {
		t.Fatalf("seeding media_asset_ts_scan: %v", err)
	}
	return assetID
}

func TestListRecordings(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	older := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "古い録画", base.Add(-2*time.Hour), "finished", 1
	})
	newer := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "新しい録画", base.Add(-time.Hour), "finished", 2
	})
	dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "未 ingest", base.Add(-3*time.Hour), "recording", 3
	})

	seedIngested(t, pool, older, 1000, map[int32][4]int64{
		0x100: {500, 2, 1, 0},
		0x110: {300, 0, 0, 5},
	})
	seedIngested(t, pool, newer, 2000, map[int32][4]int64{
		0x100: {800, 0, 0, 0},
	})

	var got []Recording
	resp := getJSON(t, srv.URL+"/api/recordings", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 3 {
		t.Fatalf("recordings = %d, want 3", len(got))
	}
	// program_start_at 降順
	if got[0].Title != "新しい録画" || got[1].Title != "古い録画" || got[2].Title != "未 ingest" {
		t.Errorf("order = %q, %q, %q", got[0].Title, got[1].Title, got[2].Title)
	}

	// ドロップ統計は PID 横断で合計される
	old := got[1]
	if old.DropSummary == nil {
		t.Fatal("ingested recording has no dropSummary")
	}
	want := DropSummary{Packets: 800, Drops: 2, Errors: 1, Scrambled: 5}
	if *old.DropSummary != want {
		t.Errorf("dropSummary = %+v, want %+v", *old.DropSummary, want)
	}
	if old.SizeBytes == nil || *old.SizeBytes != 1000 {
		t.Errorf("sizeBytes = %v, want 1000", old.SizeBytes)
	}
	if old.KeepOriginal != RecordingKeepOriginalAlways {
		t.Errorf("keepOriginal = %q, want always", old.KeepOriginal)
	}

	// 未 ingest は「統計が全部 0」と区別できるよう dropSummary を省略する
	pending := got[2]
	if pending.DropSummary != nil {
		t.Errorf("un-ingested recording should omit dropSummary, got %+v", pending.DropSummary)
	}
	if pending.SizeBytes != nil {
		t.Errorf("un-ingested recording should omit sizeBytes, got %v", pending.SizeBytes)
	}
	if pending.EncodedAssets != nil {
		t.Errorf("un-ingested recording should omit encodedAssets, got %v", pending.EncodedAssets)
	}
	if pending.Status != "recording" {
		t.Errorf("status = %q, want recording", pending.Status)
	}
	if pending.KeepOriginal != RecordingKeepOriginalAlways {
		t.Errorf("un-ingested keepOriginal = %q, want always", pending.KeepOriginal)
	}

	// 正常な録画も dropSummary は付く（全 0）
	if got[0].DropSummary == nil || *got[0].DropSummary != (DropSummary{Packets: 800}) {
		t.Errorf("clean recording dropSummary = %+v", got[0].DropSummary)
	}
}

// dropSummary は原本の存在ではなく、保存サイズに一致した TS 計測記録がある場合だけ返す。
func TestRecordingDropSummary_RequiresCurrentTSScan(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)

	unmeasured := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "計測記録なし", base, "finished", 90
	})
	unmeasuredAssetID := seedIngested(t, pool, unmeasured, 1000, map[int32][4]int64{
		0x100: {500, 2, 1, 0},
	})
	if _, err := pool.Exec(ctx, `DELETE FROM media_asset_ts_scans WHERE media_asset_id = $1`, unmeasuredAssetID); err != nil {
		t.Fatalf("deleting TS scan marker: %v", err)
	}

	stale := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "原本サイズ変更後", base.Add(time.Minute), "finished", 91
	})
	staleAssetID := seedIngested(t, pool, stale, 2000, map[int32][4]int64{
		0x100: {500, 3, 0, 0},
	})
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET size_bytes = size_bytes + 1 WHERE id = $1`, staleAssetID); err != nil {
		t.Fatalf("changing original size: %v", err)
	}

	measuredZero := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "計測済みドロップ 0", base.Add(2*time.Minute), "finished", 92
	})
	seedIngested(t, pool, measuredZero, 3000, nil)

	zeroSummary := &DropSummary{}
	want := map[int64]*DropSummary{
		unmeasured:   nil,
		stale:        nil,
		measuredZero: zeroSummary,
	}
	assertSummary := func(recording Recording, source string) {
		t.Helper()
		wantSummary, ok := want[recording.Id]
		if !ok {
			t.Fatalf("%s returned unexpected recording %d", source, recording.Id)
		}
		if !reflect.DeepEqual(recording.DropSummary, wantSummary) {
			t.Errorf("%s dropSummary for recording %d = %+v, want %+v", source, recording.Id, recording.DropSummary, wantSummary)
		}
	}

	var list []Recording
	resp := getJSON(t, srv.URL+"/api/recordings", &list)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	if len(list) != len(want) {
		t.Fatalf("recordings = %d, want %d", len(list), len(want))
	}
	for _, recording := range list {
		assertSummary(recording, "list")
	}

	for id := range want {
		var detail Recording
		resp := getJSON(t, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id), &detail)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("detail %d status = %d, want 200", id, resp.StatusCode)
		}
		assertSummary(detail, "detail")

		// PID 別内訳も同じ判定で、3 件とも空配列（null ではない）。unmeasured と stale は
		// PID 行があっても未計測として返さず、measuredZero は計測済みだが行が無いから。
		var stats []DropStat
		resp = getJSON(t, fmt.Sprintf("%s/api/recordings/%d/drop-stats", srv.URL, id), &stats)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("drop-stats %d status = %d, want 200", id, resp.StatusCode)
		}
		if stats == nil || len(stats) != 0 {
			t.Errorf("drop-stats for recording %d = %+v, want empty non-null array", id, stats)
		}
	}
}

// TestListRecordings_FailedFilterExcludesSuperseded は `?status=failed` から
// supersede 済み行だけを除き、無条件一覧には置換前後の履歴を残すことを固定する。
func TestListRecordings_FailedFilterExcludesSuperseded(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	// watcher の createRecording と同じ順序: 生きた failed 行を supersede してから
	// 同一 active-event の成功行を作る。
	const eventID = 700
	dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "擬似失敗（置換済み）", base.Add(-time.Hour), "failed", eventID
	})
	n, err := sqlcgen.New(pool).SupersedeFailedRecording(context.Background(), sqlcgen.SupersedeFailedRecordingParams{
		Site:           store.DefaultSite,
		NetworkID:      32678,
		ServiceID:      5168,
		EventID:        eventID,
		ProgramStartAt: base.Add(-time.Hour),
	})
	if err != nil || n != 1 {
		t.Fatalf("SupersedeFailedRecording: rows=%d err=%v", n, err)
	}
	dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "本物の成功", base, "finished", eventID
	})

	// `?status=failed` は superseded 済みの failed 行を返さない（0 件）。
	var failedOnly []Recording
	resp := getJSON(t, srv.URL+"/api/recordings?status=failed", &failedOnly)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(failedOnly) != 0 {
		t.Errorf("?status=failed = %d rows, want 0 (superseded failed row must be excluded)", len(failedOnly))
	}

	// 無条件一覧は履歴 2 行（supersede 元 + supersede 先）を残す。
	var all []Recording
	resp = getJSON(t, srv.URL+"/api/recordings", &all)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(all) != 2 {
		t.Errorf("unconditional list = %d rows, want 2 (both history rows retained)", len(all))
	}
}

// 一覧の射影（recordingsFromJoins、internal/store/pgstore/recordingquery.go）は
// `a.state <> 'deleted'` で結合するため、`state = 'deleting'`（unlink 待ち）の
// 原本でも sizeBytes が付き「原本あり」に見える --- 一方サーバーの 409 判定
// （GetActiveOriginalMediaAsset）は `state = 'active'` だけを見るので、この
// 非対称の上でボタンを押すと決定的に 409 になる。openapi.yaml の 409
// description と docs/storage/retention.md はこの契約を根拠に文言を書いている
// ため、射影が `state = 'active'` に絞られてこのテストが落ちたら、その docs も
// 合わせて直す必要がある。
func TestListRecordings_DeletingOriginal_StillShowsSizeBytes(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "原本 unlink 待ち", time.Now().Truncate(time.Second), "finished", 401
	})
	seedIngested(t, pool, id, 500, nil)
	if _, err := pool.Exec(context.Background(),
		`UPDATE media_assets SET state = 'deleting'
		 WHERE recording_id = $1 AND kind = 'original'`, id,
	); err != nil {
		t.Fatalf("marking original deleting: %v", err)
	}

	var got []Recording
	resp := getJSON(t, srv.URL+"/api/recordings", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 1 {
		t.Fatalf("recordings = %d, want 1", len(got))
	}
	if got[0].SizeBytes == nil || *got[0].SizeBytes != 500 {
		t.Errorf("sizeBytes = %v, want 500 (deleting original still counted as present in the list projection)", got[0].SizeBytes)
	}
}

// 原本の media_asset が deleted になっても、原本の現在可用性を表す射影と
// 不可逆な drop 履歴の射影を混同せず、一覧・詳細・PID 別 API が削除前と同じ
// 観測結果を返すことを固定する。sizeBytes は従来どおり省略し、ingest は
// committed のままにする（docs/storage/retention.md §6「安全性」）。
func TestRecordingDropHistorySurvivesOriginalDeletion(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()

	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "原本削除後も drop 履歴を表示", time.Now().Truncate(time.Second), "finished", 402
	})
	assetID := seedIngested(t, pool, id, 500, map[int32][4]int64{
		0x100: {500, 2, 1, 0},
		0x110: {300, 0, 0, 5},
	})
	q := sqlcgen.New(pool)
	elapsed := int64(1000)
	positionParams := make([]sqlcgen.InsertDropPositionParams, 0, 2)
	for _, p := range []struct {
		pid     int32
		offset  int64
		elapsed *int64
	}{
		{pid: 0x100, offset: 188, elapsed: &elapsed},
		{pid: 0x110, offset: 376},
	} {
		positionParams = append(positionParams, sqlcgen.InsertDropPositionParams{
			MediaAssetID: assetID,
			ByteOffset:   p.offset,
			Pid:          p.pid,
			ElapsedMs:    p.elapsed,
		})
	}
	batch := q.InsertDropPosition(ctx, positionParams)
	batch.Exec(nil)
	if err := batch.Close(); err != nil {
		t.Fatalf("seeding drop position batch: %v", err)
	}

	fetchListRecording := func() Recording {
		t.Helper()
		var recordings []Recording
		resp := getJSON(t, srv.URL+"/api/recordings", &recordings)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list status = %d, want 200", resp.StatusCode)
		}
		if len(recordings) != 1 || recordings[0].Id != id {
			t.Fatalf("list recordings = %+v, want recording %d", recordings, id)
		}
		return recordings[0]
	}
	fetchDetail := func() Recording {
		t.Helper()
		var recording Recording
		resp := getJSON(t, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id), &recording)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("detail status = %d, want 200", resp.StatusCode)
		}
		return recording
	}
	fetchDropStats := func() []DropStat {
		t.Helper()
		var stats []DropStat
		resp := getJSON(t, fmt.Sprintf("%s/api/recordings/%d/drop-stats", srv.URL, id), &stats)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("drop-stats status = %d, want 200", resp.StatusCode)
		}
		return stats
	}

	beforeList := fetchListRecording()
	beforeDetail := fetchDetail()
	beforeStats := fetchDropStats()
	wantSummary := DropSummary{Packets: 800, Drops: 2, Errors: 1, Scrambled: 5}
	if beforeList.DropSummary == nil || *beforeList.DropSummary != wantSummary {
		t.Fatalf("list dropSummary before deletion = %+v, want %+v", beforeList.DropSummary, wantSummary)
	}
	if beforeDetail.DropSummary == nil || *beforeDetail.DropSummary != wantSummary {
		t.Fatalf("detail dropSummary before deletion = %+v, want %+v", beforeDetail.DropSummary, wantSummary)
	}
	if len(beforeStats) != 2 || beforeStats[0].Pid != 0x100 || beforeStats[1].Pid != 0x110 {
		t.Fatalf("drop stats before deletion = %+v, want two sorted PIDs", beforeStats)
	}
	if len(beforeStats[0].Positions) != 1 || beforeStats[0].Positions[0].ByteOffset != 188 {
		t.Fatalf("PID 0x100 positions before deletion = %+v", beforeStats[0].Positions)
	}
	if len(beforeStats[1].Positions) != 1 || beforeStats[1].Positions[0].ByteOffset != 376 {
		t.Fatalf("PID 0x110 positions before deletion = %+v", beforeStats[1].Positions)
	}

	if _, err := pool.Exec(ctx,
		"UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE id = $1", assetID); err != nil {
		t.Fatalf("tombstoning original media asset: %v", err)
	}

	afterList := fetchListRecording()
	afterDetail := fetchDetail()
	afterStats := fetchDropStats()
	if afterList.DropSummary == nil || !reflect.DeepEqual(*afterList.DropSummary, *beforeList.DropSummary) {
		t.Errorf("list dropSummary after deletion = %+v, want %+v", afterList.DropSummary, beforeList.DropSummary)
	}
	if afterDetail.DropSummary == nil || !reflect.DeepEqual(*afterDetail.DropSummary, *beforeDetail.DropSummary) {
		t.Errorf("detail dropSummary after deletion = %+v, want %+v", afterDetail.DropSummary, beforeDetail.DropSummary)
	}
	if !reflect.DeepEqual(afterStats, beforeStats) {
		t.Errorf("drop stats after deletion = %+v, want unchanged %+v", afterStats, beforeStats)
	}
	if afterList.SizeBytes != nil || afterDetail.SizeBytes != nil {
		t.Errorf("sizeBytes after deletion = list %v, detail %v; want omitted", afterList.SizeBytes, afterDetail.SizeBytes)
	}
	if afterList.Ingest == nil || afterList.Ingest.State != Committed {
		t.Errorf("list ingest after deletion = %+v, want committed", afterList.Ingest)
	}
	if afterDetail.Ingest == nil || afterDetail.Ingest.State != Committed {
		t.Errorf("detail ingest after deletion = %+v, want committed", afterDetail.Ingest)
	}
}

// ごみ箱 API は動的 SQL の RecordingQuery.List/buildRecordingsQuery を通り、原本の
// media_asset が deleted になった後も dropSummary を保つ（issue #737）。
func TestListTrashRecordings_DropSummarySurvivesOriginalDeletion(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()

	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "原本削除後も drop 履歴を表示", time.Now().Truncate(time.Second), "finished", 404
	})
	assetID := seedIngested(t, pool, id, 500, map[int32][4]int64{
		0x100: {500, 2, 1, 0},
	})
	if _, err := pool.Exec(ctx, "UPDATE recordings SET deleted_at = now() WHERE id = $1", id); err != nil {
		t.Fatalf("soft-deleting recording: %v", err)
	}

	fetchTrash := func() Recording {
		t.Helper()
		var recordings []Recording
		resp := getJSON(t, srv.URL+"/api/recordings?trash=true", &recordings)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("trash list status = %d, want 200", resp.StatusCode)
		}
		if len(recordings) != 1 || recordings[0].Id != id {
			t.Fatalf("trash recordings = %+v, want recording %d", recordings, id)
		}
		return recordings[0]
	}

	before := fetchTrash()
	want := DropSummary{Packets: 500, Drops: 2, Errors: 1}
	if before.DropSummary == nil || *before.DropSummary != want {
		t.Fatalf("dropSummary before original deletion = %+v, want %+v", before.DropSummary, want)
	}

	if _, err := pool.Exec(ctx,
		"UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE id = $1", assetID); err != nil {
		t.Fatalf("tombstoning original media asset: %v", err)
	}

	after := fetchTrash()
	if after.DropSummary == nil || *after.DropSummary != want {
		t.Errorf("dropSummary after original deletion = %+v, want unchanged %+v", after.DropSummary, want)
	}
	if after.SizeBytes != nil {
		t.Errorf("sizeBytes after original deletion = %v, want omitted", after.SizeBytes)
	}
}

// ListRecordings は active な encoded 派生物（プロファイル名 + サイズ）を返すこと
// （ブラウザ再生用。issue #236 M7-3 で単なる名前の配列からサイズ付きに変わった）。
func TestListRecordings_EncodedProfiles(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "encoded あり", base, "finished", 10
	})
	seedIngested(t, pool, id, 500, nil)

	h264 := "h264"
	h265 := "h265"
	legacy := "legacy"
	dbtest.OriginalAsset(t, pool, id, func(fixtureParams *sqlcgen.CreateMediaAssetParams) {
		fixtureParams.Kind = store.AssetKindEncoded
		fixtureParams.Profile = &h264
		fixtureParams.RelPath = "a_h264.mp4"
		fixtureParams.SizeBytes = 100
	})
	dbtest.OriginalAsset(t, pool, id, func(fixtureParams *sqlcgen.CreateMediaAssetParams) {
		fixtureParams.Kind = store.AssetKindEncoded
		fixtureParams.Profile = &h265
		fixtureParams.RelPath = "a_h265.mp4"
		fixtureParams.SizeBytes = 80
	})
	// deleted は載せない
	dbtest.OriginalAsset(t, pool, id, func(fixtureParams *sqlcgen.CreateMediaAssetParams) {
		fixtureParams.Kind = store.AssetKindEncoded
		fixtureParams.Profile = &legacy
		fixtureParams.RelPath = "a_legacy.mp4"
		fixtureParams.SizeBytes = 10
	})
	if _, err := pool.Exec(context.Background(),
		`UPDATE media_assets SET state = 'deleted', deleted_at = now()
		 WHERE recording_id = $1 AND profile = 'legacy'`, id); err != nil {
		t.Fatalf("delete legacy: %v", err)
	}

	var got []Recording
	resp := getJSON(t, srv.URL+"/api/recordings", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var rec *Recording
	for i := range got {
		if got[i].Id == id {
			rec = &got[i]
			break
		}
	}
	if rec == nil {
		t.Fatal("recording not found")
	}
	if rec.EncodedAssets == nil {
		t.Fatal("encodedAssets is nil")
	}
	assets := *rec.EncodedAssets
	if len(assets) != 2 {
		t.Fatalf("encodedAssets = %+v, want 2 elements", assets)
	}
	if assets[0].Profile != "h264" || assets[0].SizeBytes == nil || *assets[0].SizeBytes != 100 {
		t.Errorf("encodedAssets[0] = %+v, want {h264 100}", assets[0])
	}
	if assets[1].Profile != "h265" || assets[1].SizeBytes == nil || *assets[1].SizeBytes != 80 {
		t.Errorf("encodedAssets[1] = %+v, want {h265 80}", assets[1])
	}
}

func TestListRecordings_QualityEvents(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "問題あり", time.Now().Truncate(time.Second), "failed", 1
	})

	events, err := json.Marshal([]db.QualityEvent{{
		At:     time.Now(),
		Event:  "recording.failed",
		Reason: json.RawMessage(`{"type":"tuner-unavailable"}`),
	}})
	if err != nil {
		t.Fatalf("marshalling events: %v", err)
	}
	// `at` を固定した fixture を作るため、store の書き込みを通さず直接書く。
	if _, err := pool.Exec(context.Background(),
		`UPDATE recordings SET quality_events = $2::jsonb WHERE id = $1`, id, events); err != nil {
		t.Fatalf("appending quality events: %v", err)
	}

	var got []Recording
	getJSON(t, srv.URL+"/api/recordings", &got)
	if len(got) != 1 {
		t.Fatalf("recordings = %d, want 1", len(got))
	}
	if got[0].QualityEvents == nil || len(*got[0].QualityEvents) != 1 {
		t.Fatalf("qualityEvents = %v, want 1 event", got[0].QualityEvents)
	}
	if (*got[0].QualityEvents)[0]["event"] != "recording.failed" {
		t.Errorf("event = %v", (*got[0].QualityEvents)[0])
	}

	// イベントが無い録画では省略される
	dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "正常", time.Now().Add(time.Hour).Truncate(time.Second), "finished", 2
	})
	got = nil
	getJSON(t, srv.URL+"/api/recordings", &got)
	for _, r := range got {
		if r.Title == "正常" && r.QualityEvents != nil {
			t.Errorf("clean recording should omit qualityEvents, got %v", r.QualityEvents)
		}
	}
}

func TestListRecordingDropStats(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "録画", time.Now().Truncate(time.Second), "finished", 1
	})
	seedIngested(t, pool, id, 1000, map[int32][4]int64{
		0x110: {300, 0, 0, 5},
		0x100: {500, 2, 1, 0},
	})

	var got []DropStat
	resp := getJSON(t, fmt.Sprintf("%s/api/recordings/%d/drop-stats", srv.URL, id), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 2 {
		t.Fatalf("drop stats = %d, want 2", len(got))
	}
	// PID 昇順
	if got[0].Pid != 0x100 || got[1].Pid != 0x110 {
		t.Errorf("pid order = %d, %d", got[0].Pid, got[1].Pid)
	}
	if got[0].Drops != 2 || got[0].Errors != 1 {
		t.Errorf("stat[0] = %+v", got[0])
	}
	if got[1].Scrambled != 5 {
		t.Errorf("stat[1] = %+v", got[1])
	}
	for _, stat := range got {
		if stat.Positions == nil {
			t.Errorf("PID 0x%04x positions should be an empty array, not null", stat.Pid)
		}
	}

	// 未 ingest の録画は空配列（null ではない）
	bare := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "未 ingest", time.Now().Add(time.Hour).Truncate(time.Second), "recording", 2
	})
	var empty []DropStat
	resp = getJSON(t, fmt.Sprintf("%s/api/recordings/%d/drop-stats", srv.URL, bare), &empty)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if empty == nil {
		t.Error("drop stats should be an empty array, not null")
	}
	if len(empty) != 0 {
		t.Errorf("drop stats = %d, want 0", len(empty))
	}
}

// PID 種別（M2-13）。値がある行は pidType が付き、無い行では省略される。
func TestListRecordingDropStats_PIDType(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "種別あり", time.Now().Truncate(time.Second), "finished", 1
	})
	assetID := seedIngested(t, pool, id, 1000, map[int32][4]int64{
		0x200: {100, 0, 0, 0}, // 種別なし（分類できなかった PID）
	})

	// api にとって pid_type は素通しの文字列（値の権威は internal/tsstat）。
	// ここで定数を参照しないのは、この層が値集合を知らないことを表すため。
	q := sqlcgen.New(pool)
	typed := map[int32]string{
		0x000: "pat",
		0x100: "video",
		0x110: "audio",
	}
	statParams := make([]sqlcgen.InsertDropStatParams, 0, len(typed))
	for pid, pidType := range typed {
		statParams = append(statParams, sqlcgen.InsertDropStatParams{
			MediaAssetID: assetID,
			Pid:          pid,
			Packets:      10,
			PidType:      &pidType,
		})
	}
	batch := q.InsertDropStat(context.Background(), statParams)
	batch.Exec(nil)
	if err := batch.Close(); err != nil {
		t.Fatalf("seeding typed drop_stat batch: %v", err)
	}

	var got []DropStat
	resp := getJSON(t, fmt.Sprintf("%s/api/recordings/%d/drop-stats", srv.URL, id), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 4 {
		t.Fatalf("drop stats = %d, want 4", len(got))
	}

	byPID := map[int]DropStat{}
	for _, s := range got {
		byPID[s.Pid] = s
	}
	for pid, wantType := range typed {
		s, ok := byPID[int(pid)]
		if !ok {
			t.Errorf("PID 0x%04x の行が無い", pid)
			continue
		}
		if s.PidType == nil {
			t.Errorf("PID 0x%04x pidType = nil, want %q", pid, wantType)
			continue
		}
		if *s.PidType != wantType {
			t.Errorf("PID 0x%04x pidType = %q, want %q", pid, *s.PidType, wantType)
		}
	}
	if s := byPID[0x200]; s.PidType != nil {
		t.Errorf("分類できなかった PID の pidType = %q, want 省略", *s.PidType)
	}

	// JSON でもキーが省略されていること（空文字を返してはいけない）
	var decoded []map[string]any
	getJSON(t, fmt.Sprintf("%s/api/recordings/%d/drop-stats", srv.URL, id), &decoded)
	for _, m := range decoded {
		pid := int(m["pid"].(float64))
		_, has := m["pidType"]
		if pid == 0x200 && has {
			t.Errorf("PID 0x0200 に pidType キーがある: %v", m)
		}
		if pid != 0x200 && !has {
			t.Errorf("PID 0x%04x に pidType キーが無い: %v", pid, m)
		}
	}
}

func TestListRecordingDropStats_Positions(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "位置付き録画", time.Now().Truncate(time.Second), "finished", 1
	})
	assetID := seedIngested(t, pool, id, 1000, map[int32][4]int64{
		0x100: {500, 2, 0, 0},
	})
	q := sqlcgen.New(pool)
	elapsed := int64(1000)
	positionParams := make([]sqlcgen.InsertDropPositionParams, 0, 2)
	for _, p := range []struct {
		offset  int64
		elapsed *int64
	}{
		{offset: 188, elapsed: &elapsed},
		{offset: 376},
	} {
		positionParams = append(positionParams, sqlcgen.InsertDropPositionParams{
			MediaAssetID: assetID,
			ByteOffset:   p.offset,
			Pid:          0x100,
			ElapsedMs:    p.elapsed,
		})
	}
	batch := q.InsertDropPosition(context.Background(), positionParams)
	batch.Exec(nil)
	if err := batch.Close(); err != nil {
		t.Fatalf("seeding drop position batch: %v", err)
	}

	var got []DropStat
	resp := getJSON(t, fmt.Sprintf("%s/api/recordings/%d/drop-stats", srv.URL, id), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 1 || len(got[0].Positions) != 2 {
		t.Fatalf("drop stats = %+v, want one PID with two positions", got)
	}
	if got[0].Drops != 2 {
		t.Errorf("drops = %d, want 2", got[0].Drops)
	}
	if got[0].Positions[0].ByteOffset != 188 || got[0].Positions[1].ByteOffset != 376 {
		t.Errorf("byte offsets = %+v, want [188 376]", got[0].Positions)
	}
	if got[0].Positions[0].ElapsedMs == nil || *got[0].Positions[0].ElapsedMs != 1000 {
		t.Errorf("first elapsed_ms = %v, want 1000", got[0].Positions[0].ElapsedMs)
	}
	if got[0].Positions[1].ElapsedMs != nil {
		t.Errorf("second elapsed_ms = %d, want nil", *got[0].Positions[1].ElapsedMs)
	}

	// 原本の tombstone 化では位置行を消さない。API の既存の active-only
	// 表示規約とは別に、不可逆な観測が DB に残ることを直接固定する。
	if _, err := pool.Exec(context.Background(),
		"UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE id = $1", assetID); err != nil {
		t.Fatalf("tombstoning media asset: %v", err)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM drop_positions WHERE media_asset_id = $1", assetID).Scan(&count); err != nil {
		t.Fatalf("counting drop positions after tombstone: %v", err)
	}
	if count != 2 {
		t.Errorf("drop_positions after tombstone = %d, want 2", count)
	}
}

func doRecordingMethod(t *testing.T, method, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
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

// soft delete → 通常一覧から消え → ごみ箱に出る → restore → 通常一覧に戻る。
// ファイル I/O は伴わない（api ロールは DB のみ）。
func TestRecordingTrash_SoftDeleteRestoreRoundTrip(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "捨てる録画", base, "finished", 10
	})

	// 通常一覧にいる
	var list []Recording
	getJSON(t, srv.URL+"/api/recordings", &list)
	if len(list) != 1 || list[0].Id != id {
		t.Fatalf("initial list = %+v, want id=%d", list, id)
	}

	// soft delete
	resp := doRecordingMethod(t, http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}

	// 通常一覧から消える
	list = nil
	getJSON(t, srv.URL+"/api/recordings", &list)
	if len(list) != 0 {
		t.Fatalf("list after delete = %d, want 0", len(list))
	}

	// ごみ箱に出る（deletedAt 付き）
	var trash []Recording
	getJSON(t, srv.URL+"/api/recordings?trash=true", &trash)
	if len(trash) != 1 || trash[0].Id != id {
		t.Fatalf("trash = %+v, want id=%d", trash, id)
	}
	if trash[0].DeletedAt == nil {
		t.Error("trash item should include deletedAt")
	}
	if trash[0].Title != "捨てる録画" {
		t.Errorf("title = %q", trash[0].Title)
	}

	// restore
	resp = doRecordingMethod(t, http.MethodPost, fmt.Sprintf("%s/api/recordings/%d/restore", srv.URL, id))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("restore status = %d, want 204", resp.StatusCode)
	}

	// 通常一覧に戻る
	list = nil
	getJSON(t, srv.URL+"/api/recordings", &list)
	if len(list) != 1 || list[0].Id != id {
		t.Fatalf("list after restore = %+v, want id=%d", list, id)
	}
	if list[0].DeletedAt != nil {
		t.Error("restored recording should omit deletedAt")
	}

	// ごみ箱は空
	trash = nil
	getJSON(t, srv.URL+"/api/recordings?trash=true", &trash)
	if len(trash) != 0 {
		t.Fatalf("trash after restore = %d, want 0", len(trash))
	}
}

// soft delete は冪等。既に削除済みでも 204。存在しない id は 404。
func TestDeleteRecording_IdempotentAndNotFound(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "冪等", time.Now().Truncate(time.Second), "finished", 11
	})

	resp := doRecordingMethod(t, http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first delete status = %d", resp.StatusCode)
	}
	resp = doRecordingMethod(t, http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second delete status = %d, want 204 (idempotent)", resp.StatusCode)
	}

	resp = doRecordingMethod(t, http.MethodDelete, fmt.Sprintf("%s/api/recordings/999999", srv.URL))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing delete status = %d, want 404", resp.StatusCode)
	}
}

// restore は論理削除を解除し、即時 purge 要求も取り下げる。
func TestRestoreRecording_Success(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()
	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "復元", time.Now().Truncate(time.Second), "finished", 111
	})
	base := fmt.Sprintf("%s/api/recordings/%d", srv.URL, id)

	resp := doRecordingMethod(t, http.MethodDelete, base)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	resp = doRecordingMethod(t, http.MethodPost, base+"/purge")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("purge status = %d, want 204", resp.StatusCode)
	}
	resp = doRecordingMethod(t, http.MethodPost, base+"/restore")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("restore status = %d, want 204", resp.StatusCode)
	}

	var deletedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM recordings WHERE id = $1`, id).Scan(&deletedAt); err != nil {
		t.Fatalf("query deleted_at: %v", err)
	}
	if deletedAt != nil {
		t.Errorf("deleted_at after restore = %v, want nil", deletedAt)
	}
	var requests int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_purge_requests WHERE recording_id = $1`, id).Scan(&requests); err != nil {
		t.Fatalf("query purge request count: %v", err)
	}
	if requests != 0 {
		t.Errorf("purge request count after restore = %d, want 0", requests)
	}
}

// restore はごみ箱に無い行に対して 404。
func TestRestoreRecording_NotInTrash(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "生きている", time.Now().Truncate(time.Second), "finished", 12
	})

	resp := doRecordingMethod(t, http.MethodPost, fmt.Sprintf("%s/api/recordings/%d/restore", srv.URL, id))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("restore live status = %d, want 404", resp.StatusCode)
	}

	resp = doRecordingMethod(t, http.MethodPost, fmt.Sprintf("%s/api/recordings/999999/restore", srv.URL))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("restore missing status = %d, want 404", resp.StatusCode)
	}
}

// purge は recording_purge_requests に行を入れ、必要なら soft-delete も兼ねる。
// ファイルは消さない。
func TestPurgeRecording_MarksPurgeRequested(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()

	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "即時削除", time.Now().Truncate(time.Second), "finished", 13
	})

	resp := doRecordingMethod(t, http.MethodPost, fmt.Sprintf("%s/api/recordings/%d/purge", srv.URL, id))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("purge status = %d, want 204", resp.StatusCode)
	}

	// DB に deleted_at が立ち、要求の行ができていること（ファイル I/O は無い）
	var deletedAt *time.Time
	err := pool.QueryRow(ctx,
		`SELECT deleted_at FROM recordings WHERE id = $1`, id,
	).Scan(&deletedAt)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if deletedAt == nil {
		t.Error("purge should also soft-delete (deleted_at set)")
	}
	purgeRequestCount := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM recording_purge_requests WHERE recording_id = $1`, id,
		).Scan(&n); err != nil {
			t.Fatalf("query recording_purge_requests: %v", err)
		}
		return n
	}
	if got := purgeRequestCount(); got != 1 {
		t.Errorf("recording_purge_requests rows = %d, want 1", got)
	}
	// requested_at は最初の要求のまま据え置く（下の 2 回目の purge で見る）。
	var firstRequestedAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT requested_at FROM recording_purge_requests WHERE recording_id = $1`, id,
	).Scan(&firstRequestedAt); err != nil {
		t.Fatalf("query requested_at: %v", err)
	}

	// ごみ箱に出る
	var trash []Recording
	getJSON(t, srv.URL+"/api/recordings?trash=true", &trash)
	if len(trash) != 1 || trash[0].Id != id {
		t.Fatalf("trash after purge = %+v", trash)
	}

	// 冪等。行は増えず、requested_at も上書きされない。
	resp = doRecordingMethod(t, http.MethodPost, fmt.Sprintf("%s/api/recordings/%d/purge", srv.URL, id))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second purge status = %d, want 204", resp.StatusCode)
	}
	if got := purgeRequestCount(); got != 1 {
		t.Errorf("recording_purge_requests rows after second purge = %d, want 1", got)
	}
	var secondRequestedAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT requested_at FROM recording_purge_requests WHERE recording_id = $1`, id,
	).Scan(&secondRequestedAt); err != nil {
		t.Fatalf("query requested_at after second purge: %v", err)
	}
	if !secondRequestedAt.Equal(firstRequestedAt) {
		t.Errorf("requested_at after second purge = %v, want %v (再要求で上書きしない)",
			secondRequestedAt, firstRequestedAt)
	}

	// 存在しない
	resp = doRecordingMethod(t, http.MethodPost, fmt.Sprintf("%s/api/recordings/999999/purge", srv.URL))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing purge status = %d, want 404", resp.StatusCode)
	}

	// restore で要求の行も消える（取り消し = DELETE）
	resp = doRecordingMethod(t, http.MethodPost, fmt.Sprintf("%s/api/recordings/%d/restore", srv.URL, id))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("restore after purge status = %d", resp.StatusCode)
	}
	err = pool.QueryRow(ctx,
		`SELECT deleted_at FROM recordings WHERE id = $1`, id,
	).Scan(&deletedAt)
	if err != nil {
		t.Fatalf("query after restore: %v", err)
	}
	if deletedAt != nil {
		t.Errorf("after restore deleted_at=%v, want nil", deletedAt)
	}
	if got := purgeRequestCount(); got != 0 {
		t.Errorf("recording_purge_requests rows after restore = %d, want 0", got)
	}
}

// restore が ErrNotFound のとき、即時削除の要求は消えない。ごみ箱状態の解除に
// 失敗したのに要求だけ取り消すと、purge 済み tombstone で「消してと言った事実」
// が消える。
func TestRestoreRecording_NotInTrash_KeepsPurgeRequest(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()

	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "purge 済み", time.Now().Truncate(time.Second), "finished", 14
	})

	resp := doRecordingMethod(t, http.MethodPost, fmt.Sprintf("%s/api/recordings/%d/purge", srv.URL, id))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("purge status = %d, want 204", resp.StatusCode)
	}
	// 削除 reconcile が完全削除を終えた状態（purged_at）にする。restore は
	// この行を対象外にする（0 行 → 404）。
	if _, err := pool.Exec(ctx,
		`UPDATE recordings SET purged_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("marking purged: %v", err)
	}

	resp = doRecordingMethod(t, http.MethodPost, fmt.Sprintf("%s/api/recordings/%d/restore", srv.URL, id))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("restore of a purged recording status = %d, want 404", resp.StatusCode)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recording_purge_requests WHERE recording_id = $1`, id,
	).Scan(&n); err != nil {
		t.Fatalf("query recording_purge_requests: %v", err)
	}
	if n != 1 {
		t.Errorf("recording_purge_requests rows after a 404 restore = %d, want 1 (要求は取り消されない)", n)
	}
}

// restore は同一イベントに生きている録画があると 409。
func TestRestoreRecording_ConflictWhenActiveExists(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	// 同じ (site, network, service, event, program_start_at) で 1 本 soft-delete、もう 1 本 active。
	// seedRecording は eventID を変えるので、ここでは直接 INSERT する。
	trashed := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "旧", base.Add(-time.Hour), "finished", 20
	})
	resp := doRecordingMethod(t, http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d", srv.URL, trashed))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	// 同じ放送イベントで生きている録画を作る。
	_ = dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "新", base.Add(-time.Hour), "finished", 20
	})

	resp = doRecordingMethod(t, http.MethodPost, fmt.Sprintf("%s/api/recordings/%d/restore", srv.URL, trashed))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("restore conflict status = %d, want 409", resp.StatusCode)
	}
}

// purged_at が立った録画（完全削除が完了した tombstone、issue #135）は
// restore できない。ファイルが二度と戻らない録画をライブラリに戻すと
// 「再生できない録画」が並んでしまうため。RestoreRecording クエリの
// WHERE に purged_at IS NULL を足して 0 行にし、既存の 404 経路に落とす。
// また、GET /api/recordings?trash=true にも出ない（trash 一覧も
// purged_at IS NULL を要求する）。
func TestRestoreRecording_PurgedNotFound(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()

	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "完全削除済み", time.Now().Truncate(time.Second), "finished", 21
	})

	resp := doRecordingMethod(t, http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}

	// 完全削除が完了した状態を直接作る（実際の delete_reconcile の
	// MarkPurgedRecordings が押すのと同じ列。ワーカーの往復は
	// internal/worker 側のテストで検証済みなので、ここでは api 層の
	// 振る舞いだけを見る）。
	if _, err := pool.Exec(ctx,
		"UPDATE recordings SET purged_at = now() WHERE id = $1", id); err != nil {
		t.Fatalf("marking purged: %v", err)
	}

	// ごみ箱一覧にはもう出ない。
	var trash []Recording
	getJSON(t, srv.URL+"/api/recordings?trash=true", &trash)
	if len(trash) != 0 {
		t.Fatalf("trash after purge = %+v, want empty", trash)
	}

	// restore は 404（既存の「ごみ箱に無い」経路と同じレスポンス）。
	resp = doRecordingMethod(t, http.MethodPost, fmt.Sprintf("%s/api/recordings/%d/restore", srv.URL, id))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("restore purged status = %d, want 404", resp.StatusCode)
	}

	// deleted_at 自体は消えていない（tombstone として残り続ける。
	// docs/storage.md §7「物理削除後も tombstone は残る」）。
	var deletedAt, purgedAtVal *time.Time
	if err := pool.QueryRow(ctx,
		"SELECT deleted_at, purged_at FROM recordings WHERE id = $1", id,
	).Scan(&deletedAt, &purgedAtVal); err != nil {
		t.Fatalf("query: %v", err)
	}
	if deletedAt == nil {
		t.Error("deleted_at should remain set on a purged tombstone")
	}
	if purgedAtVal == nil {
		t.Error("purged_at should remain set (restore must not have cleared it)")
	}
}

// GetRecording は一覧要素と同形の 1 件を返す（issue #232 M6-4）。
func TestGetRecording_Found(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "単体取得", base, "finished", 30
	})
	seedIngested(t, pool, id, 1234, map[int32][4]int64{
		0x100: {500, 2, 1, 0},
	})

	var got Recording
	resp := getJSON(t, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Id != id {
		t.Errorf("id = %d, want %d", got.Id, id)
	}
	if got.Title != "単体取得" {
		t.Errorf("title = %q", got.Title)
	}
	if got.DropSummary == nil {
		t.Fatal("dropSummary is nil")
	}
	want := DropSummary{Packets: 500, Drops: 2, Errors: 1, Scrambled: 0}
	if *got.DropSummary != want {
		t.Errorf("dropSummary = %+v, want %+v", *got.DropSummary, want)
	}
	if got.SizeBytes == nil || *got.SizeBytes != 1234 {
		t.Errorf("sizeBytes = %v, want 1234", got.SizeBytes)
	}
	if got.KeepOriginal != RecordingKeepOriginalAlways {
		t.Errorf("keepOriginal = %q, want always", got.KeepOriginal)
	}
	if got.DeletedAt != nil {
		t.Errorf("deletedAt = %v, want nil (生きている行)", got.DeletedAt)
	}
}

// 存在しない id は 404。
func TestGetRecording_NotFound(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	resp := doRecordingMethod(t, http.MethodGet, fmt.Sprintf("%s/api/recordings/999999", srv.URL))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// ごみ箱の録画も 200 で返す（メディア配信の 404 契約とは別の判断。
// openapi.yaml の getRecording description）。ただし encodedAssets は
// 一覧の trash=true と同じく省略する（プレイヤーを出さないので揃える必要が
// 無い。docs/frontend/recordings.md）。
func TestGetRecording_Trash(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "ごみ箱の単体取得", base, "finished", 31
	})
	seedIngested(t, pool, id, 1000, nil)
	h264 := "h264"
	dbtest.OriginalAsset(t, pool, id, func(fixtureParams *sqlcgen.CreateMediaAssetParams) {
		fixtureParams.Kind = store.AssetKindEncoded
		fixtureParams.Profile = &h264
		fixtureParams.RelPath = "trash_h264.mp4"
		fixtureParams.SizeBytes = 50
	})

	resp := doRecordingMethod(t, http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}

	var got Recording
	resp = getJSON(t, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (trash はメタデータを返す)", resp.StatusCode)
	}
	if got.DeletedAt == nil {
		t.Error("trash の録画は deletedAt を含むべき")
	}
	if got.EncodedAssets != nil {
		t.Errorf("trash の録画は encodedAssets を省略するべき、got %v", got.EncodedAssets)
	}
}

// 完全削除済み（purge_at が立った tombstone、issue #135）は 404。
// ファイルが既に無く、通常一覧・ごみ箱一覧のどちらにも現れない行なので、
// 単体 GET だけ見える形にしない。
func TestGetRecording_Purged(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()

	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "完全削除済みの単体取得", time.Now().Truncate(time.Second), "finished", 32
	})

	resp := doRecordingMethod(t, http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE recordings SET purged_at = now() WHERE id = $1", id); err != nil {
		t.Fatalf("marking purged: %v", err)
	}

	resp = doRecordingMethod(t, http.MethodGet, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestGetRecording_MatchesListElement は issue #232 レビューの nit 4: 単体取得
// （RecordingQuery.Get）と一覧（RecordingQuery.List）が同じ SELECT リスト定数
// （recordingsSelectColumns 等）を共有していることは構造的な保証だが、
// それ自体は「Scan の並びが定数と対応しているか」までは見ない。ここでは
// 同じ録画を GET /api/recordings（一覧から該当行を探す）と
// GET /api/recordings/{id} の両方から独立に取得し、返ってきた 2 つの
// Recording を直接比較する --- 実装の定数と比較する類（CLAUDE.md「テスト規律」
// が禁じる「期待値が定数」）ではなく、2 本の実際の HTTP レスポンスを比べる
// 本物のオラクル。原本・エンコード派生物・ドロップ統計・品質イベントを
// 一通り持つ録画で、両エンドポイントがまったく同じ形を返すことを見る。
func TestGetRecording_MatchesListElement(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	desc := "一覧要素と単体取得が一致するかの確認用"
	id := dbtest.Recording(t, pool, func(fixtureParams *sqlcgen.CreateRecordingParams) {
		fixtureParams.EventID = 40
		fixtureParams.Title = "一覧と単体の一致確認"
		fixtureParams.Description = &desc
		fixtureParams.ProgramStartAt = base
	})

	seedIngested(t, pool, id, 5000, map[int32][4]int64{
		0x100: {900, 3, 2, 1},
	})
	h264 := "h264"
	dbtest.OriginalAsset(t, pool, id, func(fixtureParams *sqlcgen.CreateMediaAssetParams) {
		fixtureParams.Kind = store.AssetKindEncoded
		fixtureParams.Profile = &h264
		fixtureParams.RelPath = "match_h264.mp4"
		fixtureParams.SizeBytes = 50
	})
	events, err := json.Marshal([]db.QualityEvent{{
		At:     base,
		Event:  "recording.record-broken",
		Reason: json.RawMessage(`{"reason":"io-error"}`),
	}})
	if err != nil {
		t.Fatalf("marshalling events: %v", err)
	}
	// `at` を固定した fixture を作るため、store の書き込みを通さず直接書く。
	if _, err := pool.Exec(context.Background(),
		`UPDATE recordings SET quality_events = $2::jsonb WHERE id = $1`, id, events); err != nil {
		t.Fatalf("appending quality events: %v", err)
	}

	var list []Recording
	getJSON(t, srv.URL+"/api/recordings", &list)
	var fromList *Recording
	for i := range list {
		if list[i].Id == id {
			fromList = &list[i]
			break
		}
	}
	if fromList == nil {
		t.Fatalf("recording %d not found in list %+v", id, list)
	}

	var fromDetail Recording
	resp := getJSON(t, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id), &fromDetail)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if !reflect.DeepEqual(*fromList, fromDetail) {
		t.Errorf("GET /api/recordings element and GET /api/recordings/{id} differ:\nlist   = %+v\ndetail = %+v", *fromList, fromDetail)
	}
}
