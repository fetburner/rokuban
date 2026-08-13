package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/testutil"
)

// seedRecordSync は mirakc record の観測を 1 行入れる。ingest 状態の導出で
// 「取り込み待ち（pending）」と「取り込む対象の観測すら無い（unknown）」を
// 分ける材料。
func seedRecordSync(t *testing.T, pool *pgxpool.Pool, recordingID int64, recordID string, contentLength *int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO record_sync (site, record_id, recording_id, program_id, status, content_length)
		 VALUES ('default', $1, $2, 1, 'finished', $3)`,
		recordID, recordingID, contentLength); err != nil {
		t.Fatalf("seeding record_sync: %v", err)
	}
}

// seedIngestProgress は転送中の進捗行を 1 行入れる（IngestWorker が書くもの）。
func seedIngestProgress(t *testing.T, pool *pgxpool.Pool, recordingID, written int64, expected *int64, observedAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO recording_ingest_progress (recording_id, written_bytes, expected_bytes, observed_at)
		 VALUES ($1, $2, $3, $4)`,
		recordingID, written, expected, observedAt); err != nil {
		t.Fatalf("seeding recording_ingest_progress: %v", err)
	}
}

// TestListRecordingsIngestState は GET /api/recordings が原本の取り込み状態を
// 4 つに分けて返すことを固定する（issue #212）。
//
// 特に **committed かつ sizeBytes 無し**（取り込んだ後に原本を削除した）と
// **pending / transferring**（まだ取り込めていない）が別物として出ることが
// 主題 --- 両者は以前どちらも「sizeBytes の省略」でしか表せず、UI が
// 未 ingest の録画に「削除済み」と読める表示を出していた（issue #211）。
func TestListRecordingsIngestState(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()

	base := time.Now().Truncate(time.Second)

	// (1) 原本が active = 取り込み済みで原本もある
	committed := seedRecording(t, pool, "取り込み済み", base.Add(-time.Hour), "finished", 1)
	seedIngested(t, pool, committed, 4096, nil)

	// (2) 原本行はあるが state='deleted' = 取り込み後に消した
	deletedOriginal := seedRecording(t, pool, "原本削除済み", base.Add(-2*time.Hour), "finished", 2)
	assetID := seedIngested(t, pool, deletedOriginal, 8192, nil)
	if _, err := pool.Exec(ctx,
		`UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE id = $1`, assetID); err != nil {
		t.Fatalf("marking original deleted: %v", err)
	}

	// (3) 転送中（進捗行あり）
	expected := int64(1000)
	observed := base.Add(-30 * time.Second)
	transferring := seedRecording(t, pool, "転送中", base.Add(-3*time.Hour), "finished", 3)
	seedRecordSync(t, pool, transferring, "rec-transferring", &expected)
	seedIngestProgress(t, pool, transferring, 250, &expected, observed)

	// (4) 取り込み待ち（record の観測だけがある）
	pending := seedRecording(t, pool, "取り込み待ち", base.Add(-4*time.Hour), "finished", 4)
	seedRecordSync(t, pool, pending, "rec-pending", &expected)

	// (5) record の観測すら無い
	unknown := seedRecording(t, pool, "観測なし", base.Add(-5*time.Hour), "finished", 5)

	var got []Recording
	resp := getJSON(t, srv.URL+"/api/recordings", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	byID := map[int64]Recording{}
	for _, r := range got {
		byID[r.Id] = r
	}
	if len(byID) != 5 {
		t.Fatalf("recordings = %d, want 5", len(byID))
	}

	for _, tc := range []struct {
		name      string
		id        int64
		wantState IngestProgressState
		wantSize  bool
	}{
		{"原本 active", committed, "committed", true},
		{"原本 deleted", deletedOriginal, "committed", false},
		{"転送中", transferring, "transferring", false},
		{"取り込み待ち", pending, "pending", false},
		{"観測なし", unknown, "unknown", false},
	} {
		rec := byID[tc.id]
		if rec.Ingest == nil {
			t.Fatalf("%s: ingest is nil (want state %q)", tc.name, tc.wantState)
		}
		if rec.Ingest.State != tc.wantState {
			t.Errorf("%s: ingest.state = %q, want %q", tc.name, rec.Ingest.State, tc.wantState)
		}
		if (rec.SizeBytes != nil) != tc.wantSize {
			t.Errorf("%s: sizeBytes present = %v, want %v", tc.name, rec.SizeBytes != nil, tc.wantSize)
		}
	}

	// 転送中の録画は進捗のバイト数を持つ。**sizeBytes には混ぜない**
	// （コミット = DB 行。不変条件 3）--- 上のループで sizeBytes が nil で
	// あることを既に固定しているので、ここは writtenBytes 側を見る。
	tr := byID[transferring]
	if tr.Ingest.WrittenBytes == nil || *tr.Ingest.WrittenBytes != 250 {
		t.Errorf("transferring writtenBytes = %v, want 250", tr.Ingest.WrittenBytes)
	}
	if tr.Ingest.ExpectedBytes == nil || *tr.Ingest.ExpectedBytes != 1000 {
		t.Errorf("transferring expectedBytes = %v, want 1000", tr.Ingest.ExpectedBytes)
	}
	if tr.Ingest.ObservedAt == nil || !tr.Ingest.ObservedAt.Equal(observed) {
		t.Errorf("transferring observedAt = %v, want %v", tr.Ingest.ObservedAt, observed)
	}

	// pending / committed には進捗のバイト数を付けない（無い数をでっち上げない）。
	if byID[pending].Ingest.WrittenBytes != nil {
		t.Errorf("pending writtenBytes = %v, want nil", byID[pending].Ingest.WrittenBytes)
	}
	if byID[committed].Ingest.WrittenBytes != nil {
		t.Errorf("committed writtenBytes = %v, want nil", byID[committed].Ingest.WrittenBytes)
	}

	// 単体 GET は一覧要素と同形（openapi.yaml の getRecording）。射影が
	// 一覧側にしか足されていない drift をここで捕まえる。
	var single Recording
	resp = getJSON(t, srv.URL+"/api/recordings/"+itoa(transferring), &single)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("single status = %d", resp.StatusCode)
	}
	if single.Ingest == nil || single.Ingest.State != "transferring" {
		t.Fatalf("single ingest = %+v, want state transferring", single.Ingest)
	}
	if single.Ingest.WrittenBytes == nil || *single.Ingest.WrittenBytes != 250 {
		t.Errorf("single writtenBytes = %v, want 250", single.Ingest.WrittenBytes)
	}
}

// TestListRecordingsIngestExpectedBytesOmitted は mirakc が record の length を
// 返さない構成（record_sync.content_length が NULL）で、分母をでっち上げずに
// 省略することを固定する。
func TestListRecordingsIngestExpectedBytesOmitted(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	id := seedRecording(t, pool, "分母なし転送中", time.Now().Truncate(time.Second), "finished", 1)
	seedRecordSync(t, pool, id, "rec-1", nil)
	seedIngestProgress(t, pool, id, 512, nil, time.Now())

	var got []Recording
	if resp := getJSON(t, srv.URL+"/api/recordings", &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 1 {
		t.Fatalf("recordings = %d, want 1", len(got))
	}
	if got[0].Ingest == nil || got[0].Ingest.State != "transferring" {
		t.Fatalf("ingest = %+v, want state transferring", got[0].Ingest)
	}
	if got[0].Ingest.WrittenBytes == nil || *got[0].Ingest.WrittenBytes != 512 {
		t.Errorf("writtenBytes = %v, want 512", got[0].Ingest.WrittenBytes)
	}
	if got[0].Ingest.ExpectedBytes != nil {
		t.Errorf("expectedBytes = %v, want nil", got[0].Ingest.ExpectedBytes)
	}
}

// TestIngestProgressFromFields は導出の優先順位を DB なしで固定する。
//
// 特に「原本行があるなら進捗行より原本を優先する」--- 取り残された進捗行が
// コミット済みの録画に「取り込み中」を名乗らせないこと（真実は media_assets
// 側。不変条件 5）。
func TestIngestProgressFromFields(t *testing.T) {
	written := int64(42)
	observed := time.Unix(1700000000, 0).UTC()

	for _, tc := range []struct {
		name      string
		fields    recordingListFields
		wantState IngestProgressState
	}{
		{
			name:      "原本行がある",
			fields:    recordingListFields{HasOriginalAsset: true, HasRecordSync: true},
			wantState: "committed",
		},
		{
			name: "原本行と進捗行が同時にある（取り残された進捗行）",
			fields: recordingListFields{
				HasOriginalAsset:   true,
				IngestWrittenBytes: &written,
				IngestObservedAt:   &observed,
			},
			wantState: "committed",
		},
		{
			name: "進捗行だけ",
			fields: recordingListFields{
				HasRecordSync:      true,
				IngestWrittenBytes: &written,
				IngestObservedAt:   &observed,
			},
			wantState: "transferring",
		},
		{
			name:      "record の観測だけ",
			fields:    recordingListFields{HasRecordSync: true},
			wantState: "pending",
		},
		{
			name:      "何も無い",
			fields:    recordingListFields{},
			wantState: "unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ingestProgressFromFields(tc.fields)
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q", got.State, tc.wantState)
			}
			if tc.wantState != "transferring" && got.WrittenBytes != nil {
				t.Errorf("writtenBytes = %v, want nil for state %q", got.WrittenBytes, got.State)
			}
		})
	}
}
