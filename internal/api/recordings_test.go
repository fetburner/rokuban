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
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

func seedRecording(t *testing.T, pool *pgxpool.Pool, title string, start time.Time, status string, eventID int32) int64 {
	t.Helper()
	id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              db.DefaultSite,
		NetworkID:         32678,
		ServiceID:         5168,
		EventID:           eventID,
		ServiceName:       "ＯＨＫ",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             title,
		ProgramStartAt:    start,
		ProgramDurationMs: (30 * time.Minute).Milliseconds(),
		Status:            status,
	})
	if err != nil {
		t.Fatalf("seeding recording: %v", err)
	}
	return id
}

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
	assetID, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     fmt.Sprintf("test/%d.m2ts", recordingID),
		SizeBytes:   size,
	})
	if err != nil {
		t.Fatalf("seeding media_asset: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles)
		 VALUES ($1, 'always', '{}')`, recordingID); err != nil {
		t.Fatalf("seeding recording_encode_policy: %v", err)
	}
	for pid, s := range stats {
		if err := q.InsertDropStat(ctx, sqlcgen.InsertDropStatParams{
			MediaAssetID: assetID,
			Pid:          pid,
			Packets:      s[0],
			Drops:        s[1],
			Errors:       s[2],
			Scrambled:    s[3],
		}); err != nil {
			t.Fatalf("seeding drop_stat: %v", err)
		}
	}
	return assetID
}

func TestListRecordings(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	older := seedRecording(t, pool, "古い録画", base.Add(-2*time.Hour), "finished", 1)
	newer := seedRecording(t, pool, "新しい録画", base.Add(-time.Hour), "finished", 2)
	seedRecording(t, pool, "未 ingest", base.Add(-3*time.Hour), "recording", 3)

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

	// 正常な録画も dropSummary は付く（全 0）
	if got[0].DropSummary == nil || *got[0].DropSummary != (DropSummary{Packets: 800}) {
		t.Errorf("clean recording dropSummary = %+v", got[0].DropSummary)
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
	seedRecording(t, pool, "擬似失敗（置換済み）", base.Add(-time.Hour), "failed", eventID)
	n, err := sqlcgen.New(pool).SupersedeFailedRecording(context.Background(), sqlcgen.SupersedeFailedRecordingParams{
		Site:           db.DefaultSite,
		NetworkID:      32678,
		ServiceID:      5168,
		EventID:        eventID,
		ProgramStartAt: base.Add(-time.Hour),
	})
	if err != nil || n != 1 {
		t.Fatalf("SupersedeFailedRecording: rows=%d err=%v", n, err)
	}
	seedRecording(t, pool, "本物の成功", base, "finished", eventID)

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

// 一覧の射影（recordingsFromJoins、internal/api/recordings_query.go）は
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

	id := seedRecording(t, pool, "原本 unlink 待ち", time.Now().Truncate(time.Second), "finished", 401)
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

// ListRecordings は active な encoded 派生物（プロファイル名 + サイズ）を返すこと
// （ブラウザ再生用。issue #236 M7-3 で単なる名前の配列からサイズ付きに変わった）。
func TestListRecordings_EncodedProfiles(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	id := seedRecording(t, pool, "encoded あり", base, "finished", 10)
	seedIngested(t, pool, id, 500, nil)

	h264 := "h264"
	h265 := "h265"
	legacy := "legacy"
	q := sqlcgen.New(pool)
	if _, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id,
		Kind:        db.AssetKindEncoded,
		Profile:     &h264,
		RelPath:     "a_h264.mp4",
		SizeBytes:   100,
	}); err != nil {
		t.Fatalf("seed h264: %v", err)
	}
	if _, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id,
		Kind:        db.AssetKindEncoded,
		Profile:     &h265,
		RelPath:     "a_h265.mp4",
		SizeBytes:   80,
	}); err != nil {
		t.Fatalf("seed h265: %v", err)
	}
	// deleted は載せない
	if _, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id,
		Kind:        db.AssetKindEncoded,
		Profile:     &legacy,
		RelPath:     "a_legacy.mp4",
		SizeBytes:   10,
	}); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
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

	id := seedRecording(t, pool, "問題あり", time.Now().Truncate(time.Second), "failed", 1)

	events, err := json.Marshal([]db.QualityEvent{{At: time.Now(), Event: "bcas_anomaly"}})
	if err != nil {
		t.Fatalf("marshalling events: %v", err)
	}
	if err := sqlcgen.New(pool).AppendQualityEvents(context.Background(), sqlcgen.AppendQualityEventsParams{
		ID:     id,
		Events: events,
	}); err != nil {
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
	if (*got[0].QualityEvents)[0]["event"] != "bcas_anomaly" {
		t.Errorf("event = %v", (*got[0].QualityEvents)[0])
	}

	// イベントが無い録画では省略される
	seedRecording(t, pool, "正常", time.Now().Add(time.Hour).Truncate(time.Second), "finished", 2)
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

	id := seedRecording(t, pool, "録画", time.Now().Truncate(time.Second), "finished", 1)
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

	// 未 ingest の録画は空配列（null ではない）
	bare := seedRecording(t, pool, "未 ingest", time.Now().Add(time.Hour).Truncate(time.Second), "recording", 2)
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

	id := seedRecording(t, pool, "種別あり", time.Now().Truncate(time.Second), "finished", 1)
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
	for pid, pidType := range typed {
		if err := q.InsertDropStat(context.Background(), sqlcgen.InsertDropStatParams{
			MediaAssetID: assetID,
			Pid:          pid,
			Packets:      10,
			PidType:      &pidType,
		}); err != nil {
			t.Fatalf("seeding typed drop_stat: %v", err)
		}
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
	id := seedRecording(t, pool, "捨てる録画", base, "finished", 10)

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

	id := seedRecording(t, pool, "冪等", time.Now().Truncate(time.Second), "finished", 11)

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

// restore はごみ箱に無い行に対して 404。
func TestRestoreRecording_NotInTrash(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	id := seedRecording(t, pool, "生きている", time.Now().Truncate(time.Second), "finished", 12)

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

	id := seedRecording(t, pool, "即時削除", time.Now().Truncate(time.Second), "finished", 13)

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

// restore が 0 行（ごみ箱に無い / purge 済み）のとき、即時削除の要求は消えない。
// ハンドラは RestoreRecording（recordings の UPDATE）が 0 行なら 404 を返して
// return し、WithdrawRecordingPurgeRequest を呼ばない --- UPDATE 0 行でも
// DELETE まで進むように書き換えると、404 を返しながら要求だけ黙って取り消される
// （purge 済み tombstone で「消してと言った事実」が消える）。
func TestRestoreRecording_NotInTrash_KeepsPurgeRequest(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()

	id := seedRecording(t, pool, "purge 済み", time.Now().Truncate(time.Second), "finished", 14)

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

// waitForLockWaiter は current_database() 内で行ロックを待っているセッションが
// 現れるまでポーリングする（sleep での決め打ちを避けるため）。who は現れるはずの
// 待ち手の名前（失敗メッセージに出す）。
func waitForLockWaiter(t *testing.T, pool *pgxpool.Pool, who string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx, `
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
	t.Fatalf("行ロックを待つセッションが現れなかった（%s がロック待ちに入っていない）", who)
}

// 復元中に別トランザクションが即時削除を要求しても、復元が成功したなら要求行は
// 残らない。
//
// 2 表を 1 文のデータ変更 CTE で書いていた頃はここが壊れていた: CTE の全アームは
// 文全体で 1 つのスナップショットを共有するので、`recordings` の行ロックで UPDATE
// アームが待たされている間に commit された要求行は、DELETE アームから見えない。
// 「復元は 204 / deleted_at は NULL / なのに要求行が 1 行残る」になり、次に普通の
// DELETE /api/recordings/{id} をした時点で 30 日の猶予をバイパスして即時 purge の
// 対象になる（ユーザーは即時削除を要求していない）。
//
// ハンドラを 1 文の CTE に戻すと、下の leftover の assert が 1 で落ちる（確認済み）。
func TestRestoreRecording_ConcurrentPurgeRequest_Withdrawn(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()

	id := seedRecording(t, pool, "復元と並行 purge", time.Now().Truncate(time.Second), "finished", 40)
	resp := doRecordingMethod(t, http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}

	// 並行する purge の前半（recordings の UPDATE）だけを進め、行ロックを保持した
	// まま止まっているセッションを作る。
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker tx: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx,
		`UPDATE recordings SET updated_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("blocker UPDATE: %v", err)
	}

	type restoreResult struct {
		status int
		err    error
	}
	done := make(chan restoreResult, 1)
	go func() {
		r, err := http.Post(fmt.Sprintf("%s/api/recordings/%d/restore", srv.URL, id), "", nil)
		if err != nil {
			done <- restoreResult{err: err}
			return
		}
		_ = r.Body.Close()
		done <- restoreResult{status: r.StatusCode}
	}()

	// restore が行ロック待ちに入るのを待つ。
	waitForLockWaiter(t, pool, "restore")

	// 待たせている間に即時削除の要求を commit する。
	if _, err := blocker.Exec(ctx,
		`INSERT INTO recording_purge_requests (recording_id) VALUES ($1)`, id); err != nil {
		t.Fatalf("blocker INSERT purge request: %v", err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("blocker commit: %v", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("restore request: %v", res.err)
		}
		if res.status != http.StatusNoContent {
			t.Fatalf("restore status = %d, want 204", res.status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("restore が 10 秒で返らなかった")
	}

	var deletedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at FROM recordings WHERE id = $1`, id).Scan(&deletedAt); err != nil {
		t.Fatalf("query deleted_at: %v", err)
	}
	if deletedAt != nil {
		t.Errorf("after restore deleted_at = %v, want nil", deletedAt)
	}

	var leftover int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recording_purge_requests WHERE recording_id = $1`, id,
	).Scan(&leftover); err != nil {
		t.Fatalf("query recording_purge_requests: %v", err)
	}
	if leftover != 0 {
		t.Errorf("recording_purge_requests rows after a successful restore = %d, want 0"+
			"（復元が成功したのに即時要求だけ残ると、次の soft-delete で猶予をバイパスする）", leftover)
	}
}

// 逆向きのオラクル: 復元が recordings の行ロックを持っている間、purge は
// **ロック待ちで直列化される**。
//
// 上の TestRestoreRecording_ConcurrentPurgeRequest_Withdrawn は blocker 側が
// 自分で `UPDATE recordings` してロックを握る形なので、purge 側の経路が
// recordings をロックするかどうかを測っていない。窓を実際に閉じているのは
// 「要求行を入れる経路が対象の recordings 行を先にロックする」ことなので
// （復元の DELETE が 0 行のときロックは何も残らない ---
// internal/db/queries/recordings_trash.sql のコメント）、それをここで見る。
//
// 一括 purge を `INSERT INTO recording_purge_requests SELECT id FROM recordings
// WHERE ...` のように recordings をロックしない形で書き足すと、この
// waitForLockWaiter が 10 秒で落ちる（要求行の INSERT が待たずに通ってしまう）。
// MarkRecordingPurgeRequested の CTE から `trashed` アームを外して
// INSERT だけにしても同じ。
//
// purge が直列化された結果は「204 / 要求行が 1 行残る」——復元の後に来た
// 「今すぐ消して」を黙って捨てないこと（復元の DELETE に食われないこと）も
// ここで同時に見ている。
func TestPurgeRecording_SerializedBehindRestoreRowLock(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()

	id := seedRecording(t, pool, "復元中の purge", time.Now().Truncate(time.Second), "finished", 40)
	resp := doRecordingMethod(t, http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d", srv.URL, id))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}

	// 復元の 1 文目だけを進めた状態を作る（recordings の行ロックを保持したまま
	// 止まっているセッション）。
	restoreTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin restore tx: %v", err)
	}
	defer func() { _ = restoreTx.Rollback(ctx) }()
	if _, err := restoreTx.Exec(ctx,
		`UPDATE recordings SET deleted_at = NULL, updated_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("restore tx UPDATE: %v", err)
	}

	type purgeResult struct {
		status int
		err    error
	}
	done := make(chan purgeResult, 1)
	go func() {
		r, err := http.Post(fmt.Sprintf("%s/api/recordings/%d/purge", srv.URL, id), "", nil)
		if err != nil {
			done <- purgeResult{err: err}
			return
		}
		_ = r.Body.Close()
		done <- purgeResult{status: r.StatusCode}
	}()

	// ここが本体のアサーション: purge は recordings 行をロックしようとして待つ。
	waitForLockWaiter(t, pool, "purge")

	// 待たせている間に要求行が入っていないことを確認する（ロック待ちなのだから
	// まだ INSERT は済んでいない）。
	var duringLock int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recording_purge_requests WHERE recording_id = $1`, id,
	).Scan(&duringLock); err != nil {
		t.Fatalf("query recording_purge_requests while locked: %v", err)
	}
	if duringLock != 0 {
		t.Errorf("recording_purge_requests rows while restore holds the row lock = %d, want 0"+
			"（要求行を入れる経路が recordings 行をロックしていない）", duringLock)
	}

	// 復元の 2 文目（要求行の DELETE。この時点では 0 行）を流して commit。
	if _, err := restoreTx.Exec(ctx,
		`DELETE FROM recording_purge_requests WHERE recording_id = $1`, id); err != nil {
		t.Fatalf("restore tx DELETE: %v", err)
	}
	if err := restoreTx.Commit(ctx); err != nil {
		t.Fatalf("restore tx commit: %v", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("purge request: %v", res.err)
		}
		if res.status != http.StatusNoContent {
			t.Fatalf("purge status = %d, want 204", res.status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("purge が 10 秒で返らなかった")
	}

	var leftover int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recording_purge_requests WHERE recording_id = $1`, id,
	).Scan(&leftover); err != nil {
		t.Fatalf("query recording_purge_requests: %v", err)
	}
	if leftover != 1 {
		t.Errorf("recording_purge_requests rows after the serialized purge = %d, want 1"+
			"（復元の後に来た即時削除の要求を捨ててはいけない）", leftover)
	}

	// purge は soft-delete も兼ねるので、復元が消した deleted_at を立て直している。
	var deletedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at FROM recordings WHERE id = $1`, id).Scan(&deletedAt); err != nil {
		t.Fatalf("query deleted_at: %v", err)
	}
	if deletedAt == nil {
		t.Error("after the serialized purge deleted_at = nil, want non-nil（purge は soft-delete も兼ねる）")
	}
}

// restore は同一イベントに生きている録画があると 409。
func TestRestoreRecording_ConflictWhenActiveExists(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	// 同じ (site, network, service, event, program_start_at) で 1 本 soft-delete、もう 1 本 active。
	// seedRecording は eventID を変えるので、ここでは直接 INSERT する。
	trashed := seedRecording(t, pool, "旧", base.Add(-time.Hour), "finished", 20)
	resp := doRecordingMethod(t, http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d", srv.URL, trashed))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	// 同じ放送イベントで生きている録画を作る。
	_ = seedRecording(t, pool, "新", base.Add(-time.Hour), "finished", 20)

	resp = doRecordingMethod(t, http.MethodPost, fmt.Sprintf("%s/api/recordings/%d/restore", srv.URL, trashed))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("restore conflict status = %d, want 409", resp.StatusCode)
	}
}

// purged_at が立った録画（完全削除が完了した tombstone、issue #135）は
// restore できない。ファイルが二度と戻らない録画をライブラリに戻すと
// 「再生できない録画」が並んでしまうため。RestoreRecording クエリの
// WHERE に purged_at IS NULL を足して 0 行にし、既存の 404 経路に落とす。
// また、GET /api/recordings?trash=true にも出ない（ListTrashRecordings も
// purged_at IS NULL を要求する）。
func TestRestoreRecording_PurgedNotFound(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()

	id := seedRecording(t, pool, "完全削除済み", time.Now().Truncate(time.Second), "finished", 21)

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
	id := seedRecording(t, pool, "単体取得", base, "finished", 30)
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
	id := seedRecording(t, pool, "ごみ箱の単体取得", base, "finished", 31)
	seedIngested(t, pool, id, 1000, nil)
	h264 := "h264"
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id,
		Kind:        db.AssetKindEncoded,
		Profile:     &h264,
		RelPath:     "trash_h264.mp4",
		SizeBytes:   50,
	}); err != nil {
		t.Fatalf("seed encoded: %v", err)
	}

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

	id := seedRecording(t, pool, "完全削除済みの単体取得", time.Now().Truncate(time.Second), "finished", 32)

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
// （queryRecordingByID）と一覧（queryRecordings）が同じ SELECT リスト定数
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
	id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              db.DefaultSite,
		NetworkID:         32678,
		ServiceID:         5168,
		EventID:           40,
		ServiceName:       "ＯＨＫ",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "一覧と単体の一致確認",
		Description:       &desc,
		ProgramStartAt:    base,
		ProgramDurationMs: (30 * time.Minute).Milliseconds(),
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("seeding recording: %v", err)
	}
	seedIngested(t, pool, id, 5000, map[int32][4]int64{
		0x100: {900, 3, 2, 1},
	})
	h264 := "h264"
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id,
		Kind:        db.AssetKindEncoded,
		Profile:     &h264,
		RelPath:     "match_h264.mp4",
		SizeBytes:   50,
	}); err != nil {
		t.Fatalf("seed encoded: %v", err)
	}
	events, err := json.Marshal([]db.QualityEvent{{At: base, Event: "bcas_anomaly"}})
	if err != nil {
		t.Fatalf("marshalling events: %v", err)
	}
	if err := sqlcgen.New(pool).AppendQualityEvents(context.Background(), sqlcgen.AppendQualityEventsParams{
		ID:     id,
		Events: events,
	}); err != nil {
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
