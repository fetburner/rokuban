package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/testutil"
)

// testIngestWorker は jobs.IngestJobArgs 用の no-op ワーカー。このパッケージの
// テストはジョブを実際に実行しない（river_job テーブルの行を SQL で確認するだけ）が、
// InsertTx は挿入時点で Kind が Workers バンドルに登録済みであることを要求するため、
// 何もしないワーカーだけ登録しておく。
type testIngestWorker struct {
	river.WorkerDefaults[jobs.IngestJobArgs]
}

func (testIngestWorker) Work(context.Context, *river.Job[jobs.IngestJobArgs]) error { return nil }

// newTestRiverClient はテスト用の River クライアントを作る。
//
// internal/worker.NewClient は使わない（同じ理由で internal/worker を import
// できないため）。
func newTestRiverClient(t *testing.T, pool *pgxpool.Pool) *river.Client[pgx5.Tx] {
	t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, &testIngestWorker{})
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers: workers,
	})
	if err != nil {
		t.Fatalf("creating test river client: %v", err)
	}
	return rc
}

func setupTest(t *testing.T) (*Watcher, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	rc := newTestRiverClient(t, pool)

	mc := mirakc.NewClient("http://unused:40772", nil)
	w := New(DefaultSite, mc, pool, rc, nil)
	return w, pool
}

// insertTestProgramSnapshot は program_snapshots 行を作る。#27 で番組の事実の
// スナップショット（title / 開始時刻 / 尺）が program_snapshots に抽出され、
// reservations / program_intents / program_overrides への FK が張られたため、
// このパッケージのフィクスチャはすべてこれを先に呼ぶ。
//
// チャンネル・イベント識別 6 列は issue #101 で NOT NULL 化された。
// このパッケージのテストは recordings.source 等の導出を見るだけでチャンネル
// 識別自体は検証しないので、固定のダミー値で足りる。
func insertTestProgramSnapshot(t *testing.T, pool *pgxpool.Pool, programID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO program_snapshots (
			site, program_id, title, start_at, duration_ms,
			network_id, service_id, channel_type, channel, event_id, service_name
		)
		VALUES ('default', $1, 'Test Program', now(), 3600000, 32736, 1024, 'GR', '27', 1, 'テスト局')
		ON CONFLICT (site, program_id) DO NOTHING`, programID,
	); err != nil {
		t.Fatalf("creating program_snapshot fixture: %v", err)
	}
}

// createTestReservation は program_snapshots + reservations 行を作る
// （program_intents には触れない）。このパッケージの大半のテストは
// recordings.source の値を検証しないので、intent の有無を気にする必要が
// ない場合に使う。source の導出（issue #26）を検証するテストは
// createTestReservationWithIntent / createTestReservationWithRule を使う。
func createTestReservation(t *testing.T, pool *pgxpool.Pool, programID int64) {
	t.Helper()
	insertTestProgramSnapshot(t, pool, programID)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO reservations (site, program_id)
		VALUES ('default', $1)`, programID,
	); err != nil {
		t.Fatalf("creating reservation: %v", err)
	}
}

// createTestRule は最小構成のルールを 1 件作る（reservations.rule_id の FK 先。
// issue #26 の受け入れ基準検証で「ルールが今マッチしている」ことを模すために使う）。
func createTestRule(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO rules (name) VALUES ('テストルール') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("creating rule fixture: %v", err)
	}
	return id
}

// createTestReservationWithIntent は reservations 行と、対応する
// program_intents{action=record} 行を両方作る（手動予約を模す）。ruleID が
// 非 nil なら rule_id も埋め、「手動予約にルールがマッチ中」の状態を作れる
// （issue #26 の受け入れ基準 1）。
func createTestReservationWithIntent(t *testing.T, pool *pgxpool.Pool, programID int64, ruleID *int64) {
	t.Helper()
	ctx := context.Background()
	insertTestProgramSnapshot(t, pool, programID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO reservations (site, program_id, rule_id)
		VALUES ('default', $1, $2)`, programID, ruleID,
	); err != nil {
		t.Fatalf("creating reservation: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO program_intents (site, program_id, action)
		VALUES ('default', $1, 'record')`, programID,
	); err != nil {
		t.Fatalf("creating program_intents fixture: %v", err)
	}
}

// createTestReservationWithRule は rule_id を持つが program_intents 行を持たない
// reservations を作る（ルール由来予約を模す。issue #26 の受け入れ基準 2）。
func createTestReservationWithRule(t *testing.T, pool *pgxpool.Pool, programID int64, ruleID int64) {
	t.Helper()
	insertTestProgramSnapshot(t, pool, programID)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO reservations (site, program_id, rule_id)
		VALUES ('default', $1, $2)`, programID, ruleID,
	); err != nil {
		t.Fatalf("creating reservation: %v", err)
	}
}

// insertProgramOverride は program_overrides に priority の上書きだけを持つ行を作る
// （program_intents には触れない）。「ルール由来の予約に priority を上書きしただけ」
// （intent 無し）を模すために使う（issue #26 の受け入れ基準 3。上書きは
// 「手動予約した」ではない — docs/recording.md §4.4）。
func insertProgramOverride(t *testing.T, pool *pgxpool.Pool, programID int64, priority int) {
	t.Helper()
	insertTestProgramSnapshot(t, pool, programID)
	_, err := pool.Exec(context.Background(), `
		INSERT INTO program_overrides (site, program_id, overrides)
		VALUES ('default', $1, $2)`,
		programID, fmt.Sprintf(`{"priority": %d}`, priority),
	)
	if err != nil {
		t.Fatalf("creating program_overrides fixture: %v", err)
	}
}

// insertTestFailedRecording は指定した active-event スロット（site, network_id,
// service_id, event_id）に直接 status='failed' の recordings 行を作る
// （issue #129 症状 2: この行が recordings_unique_active_event の枠を占有した
// まま残っている状態を再現するのが目的で、handleRecordingFailed 経由の作られ方
// 自体は TestHandleRecordingFailed_Idempotent 等で別に検証済み）。
func insertTestFailedRecording(t *testing.T, pool *pgxpool.Pool, site string, networkID, serviceID, eventID int32) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO recordings (
			source, site,
			network_id, service_id, event_id, service_name,
			channel_type, channel, title,
			is_free, program_start_at, program_duration_ms,
			status
		) VALUES (
			'manual', $1,
			$2, $3, $4, 'NHK総合',
			'GR', '27', 'Failed Attempt',
			true, now() - interval '1 hour', 180000,
			'failed'
		) RETURNING id`,
		site, networkID, serviceID, eventID,
	).Scan(&id)
	if err != nil {
		t.Fatalf("creating failed recording fixture: %v", err)
	}
	return id
}

// insertTestMediaAsset は recordingID に紐づく original media_asset 行を 1 つ作る
// （途中まで録れて failed になった行が実ファイルを持つケースを模す）。
func insertTestMediaAsset(t *testing.T, pool *pgxpool.Pool, recordingID int64) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
		VALUES ($1, 'original', $2, 12345)
		RETURNING id`,
		recordingID, fmt.Sprintf("test/failed-%d.m2ts", recordingID),
	).Scan(&id)
	if err != nil {
		t.Fatalf("creating media_asset fixture: %v", err)
	}
	return id
}

func testRecord(recordID string, programID int64, status string) mirakc.Record {
	startAt := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	recStart := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	duration := int64(3600000)
	name := "Test Program"

	r := mirakc.Record{
		ID: recordID,
		Program: mirakc.Program{
			ID:        programID,
			EventID:   100,
			ServiceID: 1024,
			NetworkID: 32736,
			StartAt:   &startAt,
			Duration:  &duration,
			IsFree:    true,
			Name:      &name,
		},
		Service: mirakc.Service{
			Name:    "NHK総合",
			Channel: mirakc.ServiceChannel{Type: "GR", Channel: "27"},
		},
		Tags: []string{mirakc.ProgramTag(programID)},
		Recording: mirakc.RecordInfo{
			Status:    status,
			StartTime: recStart,
		},
		Content: mirakc.ContentInfo{
			Path: "videos/test.m2ts",
			Type: "video/mp2t",
		},
	}

	if status == "finished" {
		endTime := mirakc.Milliseconds(time.Now())
		r.Recording.EndTime = &endTime
	}
	return r
}

func TestProcessRecord_CreateRecordingAndSync(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	createTestReservation(t, pool, 327360102415397)
	record := testRecord("abc123def456", 327360102415397, "finished")

	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord: %v", err)
	}

	// Verify record_sync
	var syncStatus string
	var syncRecordingID *int64
	err := pool.QueryRow(ctx,
		"SELECT status, recording_id FROM record_sync WHERE site = $1 AND record_id = $2",
		DefaultSite, record.ID).Scan(&syncStatus, &syncRecordingID)
	if err != nil {
		t.Fatalf("querying record_sync: %v", err)
	}
	if syncStatus != "finished" {
		t.Errorf("record_sync.status = %q, want %q", syncStatus, "finished")
	}
	if syncRecordingID == nil {
		t.Fatal("record_sync.recording_id is nil")
	}

	// Verify recordings
	var recStatus string
	err = pool.QueryRow(ctx,
		"SELECT status FROM recordings WHERE id = $1", *syncRecordingID,
	).Scan(&recStatus)
	if err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recStatus != "finished" {
		t.Errorf("recordings.status = %q, want %q", recStatus, "finished")
	}

	// Verify ingest job
	var jobCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount)
	if err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 1 {
		t.Errorf("ingest job count = %d, want 1", jobCount)
	}
}

func TestProcessRecord_Idempotent(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	createTestReservation(t, pool, 100001)
	record := testRecord("record-idem-001", 100001, "finished")

	for i := 0; i < 3; i++ {
		if err := w.processRecord(ctx, record); err != nil {
			t.Fatalf("processRecord (iteration %d): %v", i, err)
		}
	}

	var recCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 1 {
		t.Errorf("recording count = %d, want 1", recCount)
	}

	var jobCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 1 {
		t.Errorf("ingest job count = %d, want 1", jobCount)
	}
}

func TestProcessRecord_StatusProgression(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	createTestReservation(t, pool, 100002)
	record := testRecord("record-prog-001", 100002, "recording")

	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord (recording): %v", err)
	}

	// No ingest job while recording
	var jobCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 0 {
		t.Errorf("ingest job count during recording = %d, want 0", jobCount)
	}

	var recStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM recordings").Scan(&recStatus); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recStatus != "recording" {
		t.Errorf("recordings.status = %q, want %q", recStatus, "recording")
	}

	// Now finish
	record.Recording.Status = "finished"
	endTime := mirakc.Milliseconds(time.Now())
	record.Recording.EndTime = &endTime
	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord (finished): %v", err)
	}

	if err := pool.QueryRow(ctx, "SELECT status FROM recordings").Scan(&recStatus); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recStatus != "finished" {
		t.Errorf("recordings.status = %q, want %q", recStatus, "finished")
	}

	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 1 {
		t.Errorf("ingest job count after finished = %d, want 1", jobCount)
	}

	// Still only one recording
	var recCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 1 {
		t.Errorf("recording count = %d, want 1", recCount)
	}
}

func TestProcessRecord_StatusNoDowngrade(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	createTestReservation(t, pool, 100003)
	record := testRecord("record-nodg-001", 100003, "finished")

	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord (finished): %v", err)
	}

	// Out-of-order: recording event arrives after finished
	record.Recording.Status = "recording"
	record.Recording.EndTime = nil
	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord (recording): %v", err)
	}

	var recStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM recordings").Scan(&recStatus); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recStatus != "finished" {
		t.Errorf("recordings.status = %q, want %q (should not downgrade)", recStatus, "finished")
	}
}

// TestProcessRecord_SupersedesFailedRecording は issue #129 症状 2 の本体を固定する:
// 同一 active-event (site, network_id, service_id, event_id) に status='failed' の
// 行が既に recordings_unique_active_event の枠を占有していても、後から来た成功
// record が一意制約違反で弾かれず recordings 行として作られ、ingest ジョブが
// 起動すること。
//
// このテストは CreateRecording の supersede CTE（internal/db/queries/recordings.sql）
// を素の INSERT に戻すと、processRecord が「creating recording: ... duplicate key
// value violates unique constraint "recordings_unique_active_event"」で失敗して
// 落ちることを確認済み（報告参照）。
func TestProcessRecord_SupersedesFailedRecording(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	programID := int64(750001)
	createTestReservation(t, pool, programID)

	record := testRecord("record-supersede-001", programID, "finished")
	failedID := insertTestFailedRecording(t, pool, DefaultSite,
		int32(record.Program.NetworkID), int32(record.Program.ServiceID), int32(record.Program.EventID))

	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord: %v (成功 record が failed 行の一意制約に阻まれてはいけない。issue #129 症状 2)", err)
	}

	// 新しい recordings 行が作られ、ingest が起動していること。
	var syncRecordingID *int64
	if err := pool.QueryRow(ctx,
		"SELECT recording_id FROM record_sync WHERE site = $1 AND record_id = $2",
		DefaultSite, record.ID,
	).Scan(&syncRecordingID); err != nil {
		t.Fatalf("querying record_sync: %v", err)
	}
	if syncRecordingID == nil {
		t.Fatal("record_sync.recording_id is nil (新しい recordings 行が作られていない)")
	}
	if *syncRecordingID == failedID {
		t.Fatalf("new recording id (%d) == failed recording id (%d), want distinct rows (supersede しつつ新規行を作るはず)",
			*syncRecordingID, failedID)
	}

	var newStatus string
	var newSupersededAt *time.Time
	if err := pool.QueryRow(ctx,
		"SELECT status, superseded_at FROM recordings WHERE id = $1", *syncRecordingID,
	).Scan(&newStatus, &newSupersededAt); err != nil {
		t.Fatalf("querying new recording: %v", err)
	}
	if newStatus != "finished" {
		t.Errorf("new recording status = %q, want %q", newStatus, "finished")
	}
	if newSupersededAt != nil {
		t.Errorf("new recording superseded_at = %v, want nil", newSupersededAt)
	}

	var jobCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 1 {
		t.Errorf("ingest job count = %d, want 1 (成功 record を取り込む ingest が起動するはず)", jobCount)
	}

	// 失敗の履歴が失われていないこと（CLAUDE.md 不変条件 3 / docs/schema §5
	// 「録画されなかった試行も履歴に残る」）: 行は消えず、superseded_at だけが
	// 立ち、deleted_at はユーザー操作を表す列なので触れない。
	var failedDeletedAt, failedSupersededAt *time.Time
	var failedStatus string
	if err := pool.QueryRow(ctx,
		"SELECT status, deleted_at, superseded_at FROM recordings WHERE id = $1", failedID,
	).Scan(&failedStatus, &failedDeletedAt, &failedSupersededAt); err != nil {
		t.Fatalf("querying failed recording after supersede: %v", err)
	}
	if failedStatus != "failed" {
		t.Errorf("failed recording status = %q, want %q (履歴は書き換えないはず)", failedStatus, "failed")
	}
	if failedDeletedAt != nil {
		t.Errorf("failed recording deleted_at = %v, want nil (ユーザーが消したわけではない)", failedDeletedAt)
	}
	if failedSupersededAt == nil {
		t.Error("failed recording superseded_at is nil, want non-nil (active-event の枠を明け渡したはず)")
	}

	// 両方とも deleted_at IS NULL のまま残るので、履歴としては 2 行見える。
	var totalCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM recordings WHERE site = $1 AND network_id = $2 AND service_id = $3 AND event_id = $4",
		DefaultSite, record.Program.NetworkID, record.Program.ServiceID, record.Program.EventID,
	).Scan(&totalCount); err != nil {
		t.Fatalf("querying recordings by event: %v", err)
	}
	if totalCount != 2 {
		t.Errorf("recordings count for event = %d, want 2 (failed 行 + 新しい成功行が両方履歴に残るはず)", totalCount)
	}
}

// TestProcessRecord_SupersedesFailedRecordingWithMediaAsset は判断基準 2
// （media_assets を持つ failed 行を巻き込まないこと）を固定する。途中まで録れて
// failed になった行（media_assets 行を持つ）が supersede されても、そのアセットの
// recording_id は書き換わらない —— ファイルの所有者は superseded になった旧
// recordings 行のまま。superseded にするだけでは media_assets 側の状態
// （state / rel_path）も一切変更しない。物理削除するかどうかは削除 reconcile が
// 別途 recordings.deleted_at を見て判断するので、この PR の範囲では対象外。
func TestProcessRecord_SupersedesFailedRecordingWithMediaAsset(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	programID := int64(750002)
	createTestReservation(t, pool, programID)

	record := testRecord("record-supersede-media-001", programID, "finished")
	failedID := insertTestFailedRecording(t, pool, DefaultSite,
		int32(record.Program.NetworkID), int32(record.Program.ServiceID), int32(record.Program.EventID))
	assetID := insertTestMediaAsset(t, pool, failedID)

	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord: %v (media_assets を持つ failed 行があっても成功 record は取り込めるはず)", err)
	}

	var syncRecordingID *int64
	if err := pool.QueryRow(ctx,
		"SELECT recording_id FROM record_sync WHERE site = $1 AND record_id = $2",
		DefaultSite, record.ID,
	).Scan(&syncRecordingID); err != nil {
		t.Fatalf("querying record_sync: %v", err)
	}
	if syncRecordingID == nil {
		t.Fatal("record_sync.recording_id is nil")
	}

	// media_asset は動かない: recording_id は failed 行のまま、行の状態も変わらない。
	var assetRecordingID int64
	var assetState string
	var assetDeletedAt *time.Time
	if err := pool.QueryRow(ctx,
		"SELECT recording_id, state, deleted_at FROM media_assets WHERE id = $1", assetID,
	).Scan(&assetRecordingID, &assetState, &assetDeletedAt); err != nil {
		t.Fatalf("querying media_asset after supersede: %v", err)
	}
	if assetRecordingID != failedID {
		t.Errorf("media_asset.recording_id = %d, want %d (superseded 後もファイルの所有者は旧 failed 行のまま)",
			assetRecordingID, failedID)
	}
	if assetState != "active" {
		t.Errorf("media_asset.state = %q, want %q (supersede は media_assets の状態を変えないはず)", assetState, "active")
	}
	if assetDeletedAt != nil {
		t.Errorf("media_asset.deleted_at = %v, want nil (supersede はファイルを物理削除しないはず)", assetDeletedAt)
	}

	// 新しい recording 行に media_asset が誤って付け替えられていないこと。
	var assetCountForNew int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM media_assets WHERE recording_id = $1", *syncRecordingID,
	).Scan(&assetCountForNew); err != nil {
		t.Fatalf("querying media_assets for new recording: %v", err)
	}
	if assetCountForNew != 0 {
		t.Errorf("media_assets count for new recording = %d, want 0 (ingest がまだ何も作っていないはず)", assetCountForNew)
	}
}

// TestProcessRecord_SupersedeIsIdempotentAcrossReprocessing は判断基準 3
// （べき等性）を固定する: record_sweep が同じ record を再処理しても、supersede
// および recordings 行の作成が繰り返されない。processRecord は record_sync の
// (site, record_id) 行ロックで 2 回目以降 createRecording 自体を呼ばないので
// （AcquireRecordSync が既存 recording_id を返す）、CreateRecording の supersede
// CTE も 2 回目は実行されないはず。
func TestProcessRecord_SupersedeIsIdempotentAcrossReprocessing(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	programID := int64(750003)
	createTestReservation(t, pool, programID)

	record := testRecord("record-supersede-idem-001", programID, "finished")
	failedID := insertTestFailedRecording(t, pool, DefaultSite,
		int32(record.Program.NetworkID), int32(record.Program.ServiceID), int32(record.Program.EventID))

	for i := 0; i < 3; i++ {
		if err := w.processRecord(ctx, record); err != nil {
			t.Fatalf("processRecord (iteration %d): %v", i, err)
		}
	}

	var totalCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM recordings WHERE site = $1 AND network_id = $2 AND service_id = $3 AND event_id = $4",
		DefaultSite, record.Program.NetworkID, record.Program.ServiceID, record.Program.EventID,
	).Scan(&totalCount); err != nil {
		t.Fatalf("querying recordings by event: %v", err)
	}
	if totalCount != 2 {
		t.Errorf("recordings count for event after 3x processRecord = %d, want 2 "+
			"(failed 行 1 + 成功行 1 のまま増減しないはず)", totalCount)
	}

	var jobCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 1 {
		t.Errorf("ingest job count = %d, want 1 (3 回処理しても ingest は 1 回だけ)", jobCount)
	}

	var failedSupersededAt *time.Time
	if err := pool.QueryRow(ctx,
		"SELECT superseded_at FROM recordings WHERE id = $1", failedID,
	).Scan(&failedSupersededAt); err != nil {
		t.Fatalf("querying failed recording: %v", err)
	}
	if failedSupersededAt == nil {
		t.Error("failed recording superseded_at is nil, want non-nil")
	}
}

// TestProcessRecord_DoesNotSupersedeLivingNonFailedRecording は supersede の
// 境界を固定する: 同一 active-event に「生きている」（deleted_at IS NULL かつ
// superseded_at IS NULL）行が既にあっても、それが status='failed' でなければ
// supersede しない。'finished'/'recording' 等の本物の重複を黙って追い出すのは
// このクエリの責務ではなく、素の一意制約違反として従来どおりエラーになるはず
// （internal/db/queries/recordings.sql の CreateRecording の doc コメント参照）。
func TestProcessRecord_DoesNotSupersedeLivingNonFailedRecording(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	programID := int64(750004)
	createTestReservation(t, pool, programID)

	record := testRecord("record-no-supersede-001", programID, "finished")
	// 直接 'finished' な生きている行を同じスロットに作る（すでに録れている本物の
	// 重複を模す。failed ではないので supersede 対象にならないはず）。
	var livingID int64
	err := pool.QueryRow(ctx, `
		INSERT INTO recordings (
			source, site,
			network_id, service_id, event_id, service_name,
			channel_type, channel, title,
			is_free, program_start_at, program_duration_ms,
			status
		) VALUES (
			'manual', $1,
			$2, $3, $4, 'NHK総合',
			'GR', '27', 'Already Finished',
			true, now() - interval '1 hour', 180000,
			'finished'
		) RETURNING id`,
		DefaultSite, record.Program.NetworkID, record.Program.ServiceID, record.Program.EventID,
	).Scan(&livingID)
	if err != nil {
		t.Fatalf("creating living finished recording fixture: %v", err)
	}

	if err := w.processRecord(ctx, record); err == nil {
		t.Fatal("processRecord: want error (finished な生きている行と衝突するはずで、supersede されてはいけない)")
	}

	var totalCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM recordings WHERE site = $1 AND network_id = $2 AND service_id = $3 AND event_id = $4",
		DefaultSite, record.Program.NetworkID, record.Program.ServiceID, record.Program.EventID,
	).Scan(&totalCount); err != nil {
		t.Fatalf("querying recordings by event: %v", err)
	}
	if totalCount != 1 {
		t.Errorf("recordings count for event = %d, want 1 (衝突した INSERT はロールバックされ、新しい行は残らないはず)", totalCount)
	}

	var livingSupersededAt *time.Time
	if err := pool.QueryRow(ctx,
		"SELECT superseded_at FROM recordings WHERE id = $1", livingID,
	).Scan(&livingSupersededAt); err != nil {
		t.Fatalf("querying living recording: %v", err)
	}
	if livingSupersededAt != nil {
		t.Errorf("living recording superseded_at = %v, want nil (status='finished' は supersede 対象外)", livingSupersededAt)
	}

	// record_sync も tx ロールバックで残らないはず（AcquireRecordSync が同じ tx 内）。
	var syncCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM record_sync WHERE site = $1 AND record_id = $2",
		DefaultSite, record.ID,
	).Scan(&syncCount); err != nil {
		t.Fatalf("querying record_sync: %v", err)
	}
	if syncCount != 0 {
		t.Errorf("record_sync count = %d, want 0 (tx がロールバックされたはず)", syncCount)
	}
}

func TestProcessRecord_UntaggedRecord(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	record := testRecord("record-notag-001", 100004, "finished")
	record.Tags = nil

	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord: %v", err)
	}

	// record_sync created with nil recording_id
	var syncRecordingID *int64
	err := pool.QueryRow(ctx,
		"SELECT recording_id FROM record_sync WHERE site = $1 AND record_id = $2",
		DefaultSite, record.ID).Scan(&syncRecordingID)
	if err != nil {
		t.Fatalf("querying record_sync: %v", err)
	}
	if syncRecordingID != nil {
		t.Errorf("expected recording_id nil for untagged record, got %d", *syncRecordingID)
	}

	// No recordings row
	var recCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 0 {
		t.Errorf("recording count = %d, want 0 for untagged record", recCount)
	}

	// No ingest job
	var jobCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 0 {
		t.Errorf("ingest job count = %d, want 0 for untagged record", jobCount)
	}
}

// runConcurrentProcessRecord は同一 record を n 本の goroutine から同時に
// processRecord へ渡し、全 goroutine の完了を待ってエラーがないことを確認する。
// M2-16（processRecord の冪等化）の受け入れ基準である「並行実行しても
// recordings が 1 行しかできない」ことを検証するための土台。
func runConcurrentProcessRecord(t *testing.T, w *Watcher, record mirakc.Record, n int) {
	t.Helper()
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = w.processRecord(ctx, record)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("processRecord (goroutine %d): %v", i, err)
		}
	}
}

// TestProcessRecord_ConcurrentIdempotent は M2-16 の受け入れ基準の核心を検証する。
// 同一 record を多数の goroutine から同時に processRecord して、record_sync の
// (site, record_id) 行ロック（AcquireRecordSync）による直列化によって recordings
// が 1 行しか作られないことを確認する。複数ラウンド（record_id を変えて繰り返す）
// 実行し、たまたま競合が起きなかっただけという flaky な成功を排除する。
func TestProcessRecord_ConcurrentIdempotent(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	const rounds = 30
	const goroutinesPerRound = 8

	for round := 0; round < rounds; round++ {
		programID := int64(400000 + round)
		recordID := fmt.Sprintf("record-concurrent-%03d", round)

		createTestReservation(t, pool, programID)
		record := testRecord(recordID, programID, "finished")
		// recordings には (site, network_id, service_id, event_id) の一意制約
		// （部分一意索引 recordings_unique_active_event、述語
		// deleted_at IS NULL AND superseded_at IS NULL）があるため、
		// ラウンドごとに event_id を変えて他ラウンドの録画と衝突しないようにする。
		// 同じキーをこのアサーションの絞り込みにも使う（recordings.reservation_id
		// は #158 で列自体を落とした）。
		record.Program.EventID = 500 + round

		runConcurrentProcessRecord(t, w, record, goroutinesPerRound)

		var recCount int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM recordings WHERE network_id = $1 AND service_id = $2 AND event_id = $3",
			record.Program.NetworkID, record.Program.ServiceID, record.Program.EventID,
		).Scan(&recCount); err != nil {
			t.Fatalf("round %d: querying recordings: %v", round, err)
		}
		if recCount != 1 {
			t.Fatalf("round %d: recording count = %d, want 1 (concurrent processRecord must be idempotent)", round, recCount)
		}

		var jobCount int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM river_job WHERE kind = 'ingest' AND args->>'record_id' = $1", recordID,
		).Scan(&jobCount); err != nil {
			t.Fatalf("round %d: querying river_job: %v", round, err)
		}
		if jobCount != 1 {
			t.Errorf("round %d: ingest job count = %d, want 1", round, jobCount)
		}
	}
}

// TestProcessRecord_ConcurrentUntaggedRecord は Rokuban 以外が mirakc に入れた
// tag のない record（record_sync.recording_id が NULL のまま正しい）を並行処理
// しても、recordings が作られたり record_sync が壊れたりしないことを検証する。
func TestProcessRecord_ConcurrentUntaggedRecord(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	record := testRecord("record-concurrent-notag-001", 400900, "finished")
	record.Tags = nil

	runConcurrentProcessRecord(t, w, record, 20)

	var recCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 0 {
		t.Errorf("recording count = %d, want 0 for untagged record", recCount)
	}

	var syncRecordingID *int64
	if err := pool.QueryRow(ctx,
		"SELECT recording_id FROM record_sync WHERE site = $1 AND record_id = $2",
		DefaultSite, record.ID).Scan(&syncRecordingID); err != nil {
		t.Fatalf("querying record_sync: %v", err)
	}
	if syncRecordingID != nil {
		t.Errorf("expected recording_id nil for untagged record, got %d", *syncRecordingID)
	}

	var jobCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 0 {
		t.Errorf("ingest job count = %d, want 0 for untagged record", jobCount)
	}
}

func TestHandleRecordBroken(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	createTestReservation(t, pool, 100005)
	record := testRecord("record-broken-001", 100005, "recording")
	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord: %v", err)
	}

	if err := w.handleRecordBroken(ctx, mirakc.RecordBrokenData{
		RecordID: record.ID,
		Reason:   "io-error",
	}); err != nil {
		t.Fatalf("handleRecordBroken: %v", err)
	}

	var qeJSON json.RawMessage
	err := pool.QueryRow(ctx,
		"SELECT quality_events FROM recordings",
	).Scan(&qeJSON)
	if err != nil {
		t.Fatalf("querying quality_events: %v", err)
	}

	var events []qualityEvent
	if err := json.Unmarshal(qeJSON, &events); err != nil {
		t.Fatalf("unmarshalling quality_events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 quality event, got %d", len(events))
	}
	if events[0].Event != "recording.record-broken" {
		t.Errorf("quality_events[0].event = %q, want %q", events[0].Event, "recording.record-broken")
	}
}

func TestHandleRecordingFailed_Idempotent(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	rc := newTestRiverClient(t, pool)

	programID := int64(327361024100)
	createTestReservation(t, pool, programID)

	startAt := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	duration := int64(3600000)
	name := "Failed Program"

	schedule := mirakc.Schedule{
		State: "scheduled",
		Program: mirakc.Program{
			ID:        programID,
			EventID:   100,
			ServiceID: 1024,
			NetworkID: 32736,
			StartAt:   &startAt,
			Duration:  &duration,
			IsFree:    true,
			Name:      &name,
		},
	}

	services := []mirakc.Service{
		{
			ServiceID: 1024,
			NetworkID: 32736,
			Name:      "NHK総合",
			Channel:   mirakc.ServiceChannel{Type: "GR", Channel: "27"},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/recording/schedules/", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(schedule)
	})
	mux.HandleFunc("/api/services", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(services)
	})
	mockServer := httptest.NewServer(mux)
	defer mockServer.Close()

	mc := mirakc.NewClient(mockServer.URL, nil)
	w := New(DefaultSite, mc, pool, rc, nil)

	failedData := mirakc.RecordingFailedData{
		ProgramID: programID,
		Reason:    mirakc.FailedReason{Type: "tuner-unavailable"},
	}

	if err := w.handleRecordingFailed(ctx, failedData); err != nil {
		t.Fatalf("handleRecordingFailed (1st): %v", err)
	}

	var recCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 1 {
		t.Errorf("recording count after 1st call = %d, want 1", recCount)
	}

	// Call again with same program — should NOT create a duplicate
	if err := w.handleRecordingFailed(ctx, failedData); err != nil {
		t.Fatalf("handleRecordingFailed (2nd): %v", err)
	}

	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 1 {
		t.Errorf("recording count after 2nd call = %d, want 1 (idempotent)", recCount)
	}

	// Verify quality_events were merged (2 events appended)
	var qeJSON json.RawMessage
	if err := pool.QueryRow(ctx,
		"SELECT quality_events FROM recordings",
	).Scan(&qeJSON); err != nil {
		t.Fatalf("querying quality_events: %v", err)
	}

	var events []qualityEvent
	if err := json.Unmarshal(qeJSON, &events); err != nil {
		t.Fatalf("unmarshalling quality_events: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("quality_events count = %d, want 2 (merged from 2 calls)", len(events))
	}
}

func TestSweep_CatchesMissedRecords(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning: %v", err)
	}

	rc := newTestRiverClient(t, pool)

	createTestReservation(t, pool, 200001)
	createTestReservation(t, pool, 200002)

	startAt := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	recStart := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	duration := int64(3600000)
	name1 := "Program 1"
	name2 := "Program 2"

	records := []mirakc.Record{
		{
			ID:        "reconcile-rec-001",
			Program:   mirakc.Program{ID: 200001, EventID: 1, ServiceID: 1, NetworkID: 1, StartAt: &startAt, Duration: &duration, IsFree: true, Name: &name1},
			Service:   mirakc.Service{Name: "NHK", Channel: mirakc.ServiceChannel{Type: "GR", Channel: "27"}},
			Tags:      []string{mirakc.ProgramTag(200001)},
			Recording: mirakc.RecordInfo{Status: "finished", StartTime: recStart},
			Content:   mirakc.ContentInfo{Path: "test1.m2ts"},
		},
		{
			ID:        "reconcile-rec-002",
			Program:   mirakc.Program{ID: 200002, EventID: 2, ServiceID: 1, NetworkID: 1, StartAt: &startAt, Duration: &duration, IsFree: true, Name: &name2},
			Service:   mirakc.Service{Name: "NHK", Channel: mirakc.ServiceChannel{Type: "GR", Channel: "27"}},
			Tags:      []string{mirakc.ProgramTag(200002)},
			Recording: mirakc.RecordInfo{Status: "finished", StartTime: recStart},
			Content:   mirakc.ContentInfo{Path: "test2.m2ts"},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/recording/records", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(records)
	})
	mockServer := httptest.NewServer(mux)
	defer mockServer.Close()

	mc := mirakc.NewClient(mockServer.URL, nil)
	w := New(DefaultSite, mc, pool, rc, nil)

	if err := w.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var syncCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM record_sync").Scan(&syncCount); err != nil {
		t.Fatalf("querying record_sync: %v", err)
	}
	if syncCount != 2 {
		t.Errorf("record_sync count = %d, want 2", syncCount)
	}

	var recCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 2 {
		t.Errorf("recordings count = %d, want 2", recCount)
	}

	var jobCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 2 {
		t.Errorf("ingest job count = %d, want 2", jobCount)
	}

	// Run sweep again — verify idempotency
	if err := w.Sweep(ctx); err != nil {
		t.Fatalf("sweep (2nd): %v", err)
	}

	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recordings").Scan(&recCount); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if recCount != 2 {
		t.Errorf("recordings count after 2nd sweep = %d, want 2", recCount)
	}

	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'ingest'").Scan(&jobCount); err != nil {
		t.Fatalf("querying river_job: %v", err)
	}
	if jobCount != 2 {
		t.Errorf("ingest job count after 2nd sweep = %d, want 2", jobCount)
	}
}

// TestSweepAndHandleEvent_ConcurrentIdempotent は本タスク（M2-18）の核心を検証する。
// 3 段構え（docs/recording.md §3.3）のうち (a) SSE 由来の handleEvent と (c) 定期の
// Sweep（record_sweep ジョブから呼ばれる）が同一 record を同時に処理しても、
// M2-16 で processRecord に入れた record_sync の行ロックにより recordings が
// 重複しないことを確認する。
//
// (a) は record-saved イベントを模して handleEvent 経由で、(c) は Sweep（mirakc の
// ListRecords 経由）で、それぞれ独立した goroutine から同じ record を同時に叩く。
// 複数ラウンド実行して、たまたま競合が起きなかっただけの flaky な成功を排除する
// （TestProcessRecord_ConcurrentIdempotent と同じ考え方）。
func TestSweepAndHandleEvent_ConcurrentIdempotent(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	rc := newTestRiverClient(t, pool)

	const rounds = 20
	for round := 0; round < rounds; round++ {
		programID := int64(600000 + round)
		recordID := fmt.Sprintf("record-sweep-vs-event-%03d", round)

		createTestReservation(t, pool, programID)
		record := testRecord(recordID, programID, "finished")
		// recordings の (site, network_id, service_id, event_id) 一意制約
		// （deleted_at IS NULL）に他ラウンドと衝突しないよう event_id をずらす。
		// 同じキーをこのアサーションの絞り込みにも使う（recordings.reservation_id
		// は #158 で列自体を落とした）。
		record.Program.EventID = 900 + round

		mux := http.NewServeMux()
		// (c) Sweep が使う全量取得エンドポイント。
		mux.HandleFunc("/api/recording/records", func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(rw).Encode([]mirakc.Record{record})
		})
		// (a) handleEvent が record-saved を受けて GetRecord で個別取得するエンドポイント。
		mux.HandleFunc("/api/recording/records/"+recordID, func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(rw).Encode(record)
		})
		// Sweep が呼ぶ ListServices 用のスタブ（未登録だと 404 がログに出るだけで
		// テスト結果には影響しないが、ノイズを消しておく）。
		mux.HandleFunc("/api/services", func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(rw).Encode([]mirakc.Service{})
		})
		mockServer := httptest.NewServer(mux)

		w := New(DefaultSite, mirakc.NewClient(mockServer.URL, nil), pool, rc, nil)

		savedData, err := json.Marshal(mirakc.RecordSavedData{
			RecordID:        recordID,
			RecordingStatus: "finished",
		})
		if err != nil {
			t.Fatalf("marshalling record-saved data: %v", err)
		}
		ev := mirakc.Event{Type: "recording.record-saved", Data: savedData}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			// (a) SSE 由来: record-saved イベントを handleEvent 経由で処理する経路。
			w.handleEvent(ctx, ev)
		}()
		go func() {
			defer wg.Done()
			// (c) 定期突き合わせ: Sweep が ListRecords 経由で同じ record を処理する経路。
			if err := w.Sweep(ctx); err != nil {
				t.Errorf("round %d: Sweep: %v", round, err)
			}
		}()
		wg.Wait()
		mockServer.Close()

		var recCount int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM recordings WHERE network_id = $1 AND service_id = $2 AND event_id = $3",
			record.Program.NetworkID, record.Program.ServiceID, record.Program.EventID,
		).Scan(&recCount); err != nil {
			t.Fatalf("round %d: querying recordings: %v", round, err)
		}
		if recCount != 1 {
			t.Fatalf("round %d: recording count = %d, want 1 "+
				"((a) handleEvent と (c) Sweep の並行実行は冪等でなければならない)", round, recCount)
		}

		var jobCount int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM river_job WHERE kind = 'ingest' AND args->>'record_id' = $1", recordID,
		).Scan(&jobCount); err != nil {
			t.Fatalf("round %d: querying river_job: %v", round, err)
		}
		if jobCount != 1 {
			t.Errorf("round %d: ingest job count = %d, want 1", round, jobCount)
		}
	}
}

// TestRun_NoAutomaticSweep は watcher が SSE 購読と handleEvent だけの常駐になった
// こと（M2-18）を確認する。(c) を record_sweep ジョブへ切り出したことで
// Watcher 自身は `GET /api/recording/records` を一切呼ばなくなったはず。
func TestRun_NoAutomaticSweep(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	rc := newTestRiverClient(t, pool)

	var recordsCalls, eventsCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/recording/records", func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&recordsCalls, 1)
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode([]mirakc.Record{})
	})
	mux.HandleFunc("/events", func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&eventsCalls, 1)
		rw.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := rw.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter is not Flusher")
		}
		flusher.Flush()
		<-r.Context().Done()
	})
	mockServer := httptest.NewServer(mux)
	defer mockServer.Close()

	mc := mirakc.NewClient(mockServer.URL, nil)
	w := New(DefaultSite, mc, pool, rc, nil)

	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()

	// SSE 接続が確立するまで少し待ってから、Run が自発的に全量突き合わせを
	// 呼んでいないことを確認する。
	deadline := time.After(500 * time.Millisecond)
	for atomic.LoadInt32(&eventsCalls) == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for SSE connection")
		case <-time.After(10 * time.Millisecond):
		}
	}

	runCancel()
	<-done

	if got := atomic.LoadInt32(&recordsCalls); got != 0 {
		t.Errorf("GET /api/recording/records call count = %d, want 0 "+
			"(Watcher.Run は SSE 購読だけの常駐になり、全量突き合わせは record_sweep ジョブの仕事のはず)", got)
	}
}

// getRecordingSource は recordings.source を引く（issue #26 の回帰テスト用）。
// 呼び出し元はいずれも 1 テストにつき recordings 行が 1 つしかできない構成
// なので絞り込みは不要（recordings.reservation_id は #158 で列自体を落とした）。
func getRecordingSource(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var source string
	if err := pool.QueryRow(context.Background(),
		"SELECT source FROM recordings",
	).Scan(&source); err != nil {
		t.Fatalf("querying recordings.source: %v", err)
	}
	return source
}

// TestProcessRecord_ManualReservationWithRuleMatch_SourceManual は issue #26 の
// 受け入れ基準 1（元のバグの回帰テスト）: 手動予約（program_intents{record} あり）に
// ルールがマッチして rule_id が埋まっていても、録画の recordings.source は
// 'manual' のままでなければならない。
//
// 修正前の internal/ruler/sql.go は reservations.source を
// `CASE WHEN d.rule_id IS NOT NULL THEN 'rule' ELSE ... END` で不可逆に
// 'rule' へ書き換えており、それを watcher がそのまま recordings.source に
// コピーしていた。修正後は reservations に source 列自体が無く、watcher は
// 録画時点の program_intents の有無だけを見るため、rule_id が埋まっていても
// 手動予約の履歴が保たれる。
func TestProcessRecord_ManualReservationWithRuleMatch_SourceManual(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	ruleID := createTestRule(t, pool)
	programID := int64(700001)
	createTestReservationWithIntent(t, pool, programID, &ruleID)

	record := testRecord("record-manual-rule-match-001", programID, "finished")
	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord: %v", err)
	}

	if got := getRecordingSource(t, pool); got != reservation.SourceManual {
		t.Errorf("recordings.source = %q, want %q "+
			"(手動予約にルールがマッチしても由来は manual のまま変わらないはず。issue #26)", got, reservation.SourceManual)
	}
}

// TestProcessRecord_RuleReservation_SourceRule は issue #26 の受け入れ基準 2:
// ルール由来の予約（program_intents 行なし、rule_id あり）を録画すると
// recordings.source は 'rule' になる。
func TestProcessRecord_RuleReservation_SourceRule(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	ruleID := createTestRule(t, pool)
	programID := int64(700002)
	createTestReservationWithRule(t, pool, programID, ruleID)

	record := testRecord("record-rule-001", programID, "finished")
	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord: %v", err)
	}

	if got := getRecordingSource(t, pool); got != reservation.SourceRule {
		t.Errorf("recordings.source = %q, want %q", got, reservation.SourceRule)
	}
}

// TestProcessRecord_RuleReservationWithOverrideOnly_SourceRule は issue #26 の
// 受け入れ基準 3: ルール由来の予約に priority だけを上書きした（program_overrides
// はあるが program_intents は無い）場合も recordings.source は 'rule' のまま。
//
// M2-4（program_overrides 分離）後は program_intents の有無だけを見るので、
// program_overrides（上書き）の有無に判定が影響されない。
func TestProcessRecord_RuleReservationWithOverrideOnly_SourceRule(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	ruleID := createTestRule(t, pool)
	programID := int64(700003)
	createTestReservationWithRule(t, pool, programID, ruleID)
	insertProgramOverride(t, pool, programID, 5)

	record := testRecord("record-rule-override-001", programID, "finished")
	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord: %v", err)
	}

	if got := getRecordingSource(t, pool); got != reservation.SourceRule {
		t.Errorf("recordings.source = %q, want %q "+
			"(priority の上書きだけでは「手動予約した」にならないはず。issue #26 / M2-4)", got, reservation.SourceRule)
	}
}

// TestHandleRecordingFailed_SourceDerivedFromIntent は handleRecordingFailed
// （recording.failed イベント経由の失敗記録）でも createRecording と同じ
// deriveRecordingSource を通ることを確認する。手動予約にルールがマッチしていても
// 失敗記録の source が manual のままであることを見る。
func TestHandleRecordingFailed_SourceDerivedFromIntent(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}
	rc := newTestRiverClient(t, pool)

	ruleID := createTestRule(t, pool)
	programID := int64(700004)
	createTestReservationWithIntent(t, pool, programID, &ruleID)

	startAt := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	duration := int64(3600000)
	name := "Failed Program"

	schedule := mirakc.Schedule{
		State: "scheduled",
		Program: mirakc.Program{
			ID:        programID,
			EventID:   200,
			ServiceID: 1024,
			NetworkID: 32736,
			StartAt:   &startAt,
			Duration:  &duration,
			IsFree:    true,
			Name:      &name,
		},
	}
	services := []mirakc.Service{
		{ServiceID: 1024, NetworkID: 32736, Name: "NHK総合", Channel: mirakc.ServiceChannel{Type: "GR", Channel: "27"}},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/recording/schedules/", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(schedule)
	})
	mux.HandleFunc("/api/services", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(services)
	})
	mockServer := httptest.NewServer(mux)
	defer mockServer.Close()

	mc := mirakc.NewClient(mockServer.URL, nil)
	w := New(DefaultSite, mc, pool, rc, nil)

	failedData := mirakc.RecordingFailedData{
		ProgramID: programID,
		Reason:    mirakc.FailedReason{Type: "tuner-unavailable"},
	}
	if err := w.handleRecordingFailed(ctx, failedData); err != nil {
		t.Fatalf("handleRecordingFailed: %v", err)
	}

	if got := getRecordingSource(t, pool); got != reservation.SourceManual {
		t.Errorf("recordings.source = %q, want %q "+
			"(失敗記録でも手動予約の由来は manual のままのはず)", got, reservation.SourceManual)
	}
}

// TestProcessRecord_MissingReservation_SourceManual は、tag は付いているが予約行が
// 存在しない record（予約が削除された後に mirakc 側の録画が残っていた等）の
// source が 'manual' になることを確認する。
//
// issue #26 の修正で source を program_intents から導出するようにしたが、
// 「意図が無ければ rule」と素朴に倒すと**この経路が rule になってしまう**。
// rule_id は NULL なので `source = 'rule'` かつ `rule_id IS NULL` という矛盾した
// 組が生まれる。帰属できるルールが無いなら manual に倒すのが正しい
// （issue #26 以前の実装も source の既定を "manual" にしていた）。
func TestProcessRecord_MissingReservation_SourceManual(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	// 存在しない予約を指す番組 tag を付ける。intent も作らない。
	programID := int64(700005)

	record := testRecord("record-missing-res-001", programID, "finished")
	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord: %v", err)
	}

	var source string
	var ruleID *int64
	if err := pool.QueryRow(ctx,
		// recordings は programId を分解して持つ（不変条件 7: mirakc 固有の ID を
		// 永続テーブルに構造として持ち込まない）ので event_id で引く。
		"SELECT source, rule_id FROM recordings WHERE site = $1 AND event_id = $2",
		w.site, 100,
	).Scan(&source, &ruleID); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if source != reservation.SourceManual {
		t.Errorf("recordings.source = %q, want %q "+
			"（帰属できるルールが無いのに rule と記録すると rule_id IS NULL と矛盾する。issue #26）",
			source, reservation.SourceManual)
	}
	if ruleID != nil {
		t.Errorf("recordings.rule_id = %v, want nil", ruleID)
	}
}

// TestProcessRecord_ReservationGCedBeyondGrace_SourceManual は issue #214 の
// 交点の**前半**を固定する: エッジに record が滞留して復帰が
// epg.retention_grace（既定 24h）より後になると、復帰時に recordings 行を作る
// createRecording は既に GC された予約を引くことになり、ルール由来の録画でも
// source が 'manual' に・rule_id が NULL になる。
//
// TestProcessRecord_MissingReservation_SourceManual が「予約行が最初から無い」を
// 模すのに対し、こちらは**ルール予約が確かに存在したうえで、実際の GC クエリ
// （DeleteEndedProgramSnapshots。ruler の runGC が呼ぶのと同じもの）に刈られた**
// 経路を通す。
//
// 後半（source='manual' の録画で ingest の凍結解決が失敗すると slog.Warn では
// なく slog.Info になる）は internal/worker の
// TestIngestWorker_LogsInfoWhenManualSourceReservationUnresolvable が持つ。
// docs/storage.md §6 が書く回線断の経路はこの 2 本の合成であり、1 本で通す
// テストは無い（パッケージ境界。internal/watcher は internal/worker に依存
// できない。Watcher が依存するジョブ契約は internal/jobs に置かれている）。
func TestProcessRecord_ReservationGCedBeyondGrace_SourceManual(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()

	ruleID := createTestRule(t, pool)
	programID := int64(700006)
	createTestReservationWithRule(t, pool, programID, ruleID)

	// 放送は 48 時間前に終わったことにする（insertTestProgramSnapshot は
	// start_at = now() / duration 1h で作る）。
	if _, err := pool.Exec(ctx,
		"UPDATE program_snapshots SET start_at = now() - interval '48 hours' WHERE site = $1 AND program_id = $2",
		DefaultSite, programID,
	); err != nil {
		t.Fatalf("aging program snapshot: %v", err)
	}

	var deleted int64
	if err := pool.QueryRow(ctx,
		// ruler の runGC が呼ぶ DeleteEndedProgramSnapshots と同じ述語。
		// internal/db/sqlcgen をこのテストから引かずに書き下すのは、
		// 本パッケージのフィクスチャが一貫して生 SQL である流儀に合わせるため。
		`WITH d AS (
		     DELETE FROM program_snapshots
		     WHERE start_at + (duration_ms * interval '1 millisecond') < now() - interval '24 hours'
		     RETURNING 1
		 ) SELECT count(*) FROM d`,
	).Scan(&deleted); err != nil {
		t.Fatalf("running GC: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("GC が刈った行数 = %d, want 1（前提が崩れている: 刈られていないなら以降は何も主張しない）", deleted)
	}
	var reservations int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM reservations WHERE site = $1 AND program_id = $2", DefaultSite, programID,
	).Scan(&reservations); err != nil {
		t.Fatalf("counting reservations: %v", err)
	}
	if reservations != 0 {
		t.Fatalf("GC 後の reservations = %d, want 0（FK CASCADE で一緒に落ちるはず）", reservations)
	}

	record := testRecord("record-gced-001", programID, "finished")
	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord: %v", err)
	}

	var source string
	var gotRuleID *int64
	if err := pool.QueryRow(ctx,
		"SELECT source, rule_id FROM recordings WHERE site = $1 AND event_id = $2", w.site, 100,
	).Scan(&source, &gotRuleID); err != nil {
		t.Fatalf("querying recordings: %v", err)
	}
	if source != reservation.SourceManual {
		t.Errorf("recordings.source = %q, want %q "+
			"（GC 済みの予約は引けないので manual に倒れる。issue #214 の交点）", source, reservation.SourceManual)
	}
	if gotRuleID != nil {
		t.Errorf("recordings.rule_id = %v, want nil （ルール予約は GC で失われている。issue #214）", *gotRuleID)
	}
}

// TestProcessRecord_StatusValues は issue #130 の受け入れ基準の核心:
// mirakc が返しうる recording.status の 4 値（recording/finished/canceled/failed）
// すべてで processRecord がエラーを返さず、決定的な結果になることを固定する。
//
// canceled はこのテストが書かれる前は recordings_status_check（旧 3 値）
// 違反でトランザクション全体がロールバックし、record_sync にも観測が残らない
// まま毎パス再試行され続けていた（#130 本体のバグ）。このテストは
// recordings_status_check を issue #130 の 4 値から 3 値に戻すと canceled のケースで
// 落ちることを確認済み（報告参照。意図的に実装を壊して確認する、CLAUDE.md
// テスト規律）。
//
// 5 つ目のケース（未知の値）は「無限リトライにならない」ことを固定する
// （#130「含むもの 4」）。mirakc が将来値を追加した場合を模した架空の値
// "rescheduling" を使う（これは実際には mirakc の別概念
// RecordingScheduleState の値であり、RecordInfo.Status には現れない。
// 「未知の値の例」として issue 本文が挙げているのと同じ値）。
func TestProcessRecord_StatusValues(t *testing.T) {
	tests := []struct {
		name          string
		mirakcStatus  string
		wantRecStatus string // 正規化後、recordings.status に入るはずの値
		wantIngestJob bool
	}{
		{name: "recording", mirakcStatus: "recording", wantRecStatus: "recording", wantIngestJob: false},
		{name: "finished", mirakcStatus: "finished", wantRecStatus: "finished", wantIngestJob: true},
		{name: "canceled", mirakcStatus: "canceled", wantRecStatus: "canceled", wantIngestJob: false},
		{name: "failed", mirakcStatus: "failed", wantRecStatus: "failed", wantIngestJob: false},
		{
			name:          "unknown status does not infinite-retry",
			mirakcStatus:  "rescheduling",
			wantRecStatus: "failed", // normalizeRecordingStatus の丸め先。理由は同関数の doc コメント参照
			wantIngestJob: false,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, pool := setupTest(t)
			ctx := context.Background()

			programID := int64(800000 + i)
			recordID := fmt.Sprintf("record-status-%02d", i)
			createTestReservation(t, pool, programID)
			record := testRecord(recordID, programID, tt.mirakcStatus)

			if err := w.processRecord(ctx, record); err != nil {
				t.Fatalf("processRecord: %v (status %q が CHECK 違反等でエラーになってはいけない)",
					err, tt.mirakcStatus)
			}

			// record_sync は mirakc の生の値をそのまま保持しなければならない
			// （CHECK が無い列。docs/schema/record-sync.md「mirakc の
			// recordingStatus そのまま」）。未知の値でもここで観測が残ることが
			// 「無限リトライにならない」の土台になる。
			var syncStatus string
			var syncRecordingID *int64
			if err := pool.QueryRow(ctx,
				"SELECT status, recording_id FROM record_sync WHERE site = $1 AND record_id = $2",
				DefaultSite, recordID,
			).Scan(&syncStatus, &syncRecordingID); err != nil {
				t.Fatalf("querying record_sync: %v", err)
			}
			if syncStatus != tt.mirakcStatus {
				t.Errorf("record_sync.status = %q, want %q (mirakc の生の値がそのまま残るはず)",
					syncStatus, tt.mirakcStatus)
			}
			if syncRecordingID == nil {
				t.Fatal("record_sync.recording_id is nil (recordings 行が作られていない)")
			}

			var recStatus string
			if err := pool.QueryRow(ctx,
				"SELECT status FROM recordings WHERE id = $1", *syncRecordingID,
			).Scan(&recStatus); err != nil {
				t.Fatalf("querying recordings: %v", err)
			}
			if recStatus != tt.wantRecStatus {
				t.Errorf("recordings.status = %q, want %q", recStatus, tt.wantRecStatus)
			}

			// 2 回目の processRecord も同じ結果を返し続けること（再試行しても
			// 状態が安定していることの確認。#130 のバグは「毎回失敗し続ける」
			// 形だったので、2 回目もエラーにならないことを見ておく）。
			if err := w.processRecord(ctx, record); err != nil {
				t.Fatalf("processRecord (2nd call): %v", err)
			}

			var jobCount int
			if err := pool.QueryRow(ctx,
				"SELECT count(*) FROM river_job WHERE kind = 'ingest' AND args->>'record_id' = $1", recordID,
			).Scan(&jobCount); err != nil {
				t.Fatalf("querying river_job: %v", err)
			}
			wantJobCount := 0
			if tt.wantIngestJob {
				wantJobCount = 1
			}
			if jobCount != wantJobCount {
				t.Errorf("ingest job count = %d, want %d", jobCount, wantJobCount)
			}
		})
	}
}
