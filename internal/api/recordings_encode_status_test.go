package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/testutil"
)

// seedEncodeAttempt は recording_encode_attempts に 1 行入れる（EncodeWorker が
// 書くもの。issue #316）。error は state="running" のときは無視される想定
// （呼び出し側が nil を渡す）。
func seedEncodeAttempt(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile, state string, encodeErr *string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO recording_encode_attempts (recording_id, profile, state, error)
		 VALUES ($1, $2, $3, $4)`,
		recordingID, profile, state, encodeErr); err != nil {
		t.Fatalf("seeding recording_encode_attempts: %v", err)
	}
}

// setEncodeProfiles は recording_encode_policy.encode_profiles（desired）を
// 上書きする。seedIngested が既定 '{}' で凍結した行を、テストが望む desired
// 一覧に書き換える。
func setEncodeProfiles(t *testing.T, pool *pgxpool.Pool, recordingID int64, profiles []string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE recording_encode_policy SET encode_profiles = $2 WHERE recording_id = $1`,
		recordingID, profiles); err != nil {
		t.Fatalf("updating encode_profiles: %v", err)
	}
}

// seedEncodedAsset は active な encoded media_asset を 1 件作る（observed。
// UpsertEncodedMediaAsset を直接呼ばず INSERT するのは、テストが recording_id
// 側の状態だけを組み立てたいため）。
func seedEncodedAsset(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile string, size int64) {
	t.Helper()
	relPath := fmt.Sprintf("test/%d_%s.mp4", recordingID, profile)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, state)
		 VALUES ($1, 'encoded', $2, $3, $4, 'active')`,
		recordingID, profile, relPath, size); err != nil {
		t.Fatalf("seeding encoded media_asset: %v", err)
	}
}

// TestListRecordingsEncodeStatus は完了していないエンコードプロファイルの
// 試行状態が queued / running / failed のどれで出るかを固定する（issue #316）。
//
// 主題は次の 3 点:
//   - 完了済みプロファイル（active な encoded がある）は encodeStatus に
//     出ない（encodedAssets の存在で「完了」を示すので、同じ情報を 2 つの
//     配列で主張しない）
//   - 試行行が無いプロファイルは queued（まだ来ていない、来る根拠は desired
//     にある）
//   - 試行行があるプロファイルはその state（running/failed）をそのまま出す
func TestListRecordingsEncodeStatus(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	base := time.Now().Truncate(time.Second)

	// h264: 完了済み（active な encoded） / h265: running / aac: 試行行なし（queued）
	mixed := seedRecording(t, pool, "混在", base.Add(-time.Hour), "finished", 1)
	seedIngested(t, pool, mixed, 1000, nil)
	setEncodeProfiles(t, pool, mixed, []string{"h264", "h265", "aac"})
	seedEncodedAsset(t, pool, mixed, "h264", 500)
	seedEncodeAttempt(t, pool, mixed, "h265", "running", nil)

	// vp9: 失敗（試行行 state=failed）
	failMsg := "unknown encode profile \"vp9\""
	failing := seedRecording(t, pool, "失敗", base.Add(-2*time.Hour), "finished", 2)
	seedIngested(t, pool, failing, 2000, nil)
	setEncodeProfiles(t, pool, failing, []string{"vp9"})
	seedEncodeAttempt(t, pool, failing, "vp9", "failed", &failMsg)

	// 全プロファイル完了済み: encodeStatus は省略（空配列を返さない）
	done := seedRecording(t, pool, "全完了", base.Add(-3*time.Hour), "finished", 3)
	seedIngested(t, pool, done, 3000, nil)
	setEncodeProfiles(t, pool, done, []string{"h264"})
	seedEncodedAsset(t, pool, done, "h264", 700)

	// プロファイル未設定: encodeStatus は省略
	none := seedRecording(t, pool, "未設定", base.Add(-4*time.Hour), "finished", 4)
	seedIngested(t, pool, none, 4000, nil)

	var got []Recording
	if resp := getJSON(t, srv.URL+"/api/recordings", &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	byID := map[int64]Recording{}
	for _, r := range got {
		byID[r.Id] = r
	}
	if len(byID) != 4 {
		t.Fatalf("recordings = %d, want 4", len(byID))
	}

	mixedRec, ok := byID[mixed]
	if !ok {
		t.Fatalf("mixed recording missing from list")
	}
	if mixedRec.EncodeStatus == nil {
		t.Fatalf("mixed: encodeStatus is nil, want h265=running, aac=queued")
	}
	statusByProfile := map[string]EncodeJobStatusState{}
	for _, s := range *mixedRec.EncodeStatus {
		statusByProfile[s.Profile] = s.State
	}
	if len(statusByProfile) != 2 {
		t.Fatalf("mixed: encodeStatus = %+v, want exactly 2 entries (h264 must be excluded)", *mixedRec.EncodeStatus)
	}
	if _, present := statusByProfile["h264"]; present {
		t.Errorf("mixed: h264 (already encoded) must not appear in encodeStatus")
	}
	if statusByProfile["h265"] != EncodeJobStatusStateRunning {
		t.Errorf("mixed: h265 state = %q, want running", statusByProfile["h265"])
	}
	if statusByProfile["aac"] != EncodeJobStatusStateQueued {
		t.Errorf("mixed: aac state = %q, want queued (no attempt row yet)", statusByProfile["aac"])
	}

	failingRec, ok := byID[failing]
	if !ok {
		t.Fatalf("failing recording missing from list")
	}
	if failingRec.EncodeStatus == nil || len(*failingRec.EncodeStatus) != 1 {
		t.Fatalf("failing: encodeStatus = %+v, want exactly 1 entry", failingRec.EncodeStatus)
	}
	if (*failingRec.EncodeStatus)[0].State != EncodeJobStatusStateFailed {
		t.Errorf("failing: state = %q, want failed", (*failingRec.EncodeStatus)[0].State)
	}

	if doneRec := byID[done]; doneRec.EncodeStatus != nil {
		t.Errorf("done: encodeStatus = %+v, want omitted (nil)", doneRec.EncodeStatus)
	}
	if noneRec := byID[none]; noneRec.EncodeStatus != nil {
		t.Errorf("none: encodeStatus = %+v, want omitted (nil)", noneRec.EncodeStatus)
	}

	// 単体 GET は一覧要素と同形（openapi.yaml の getRecording）。射影が
	// 一覧側にしか足されていない drift をここで捕まえる。
	var single Recording
	if resp := getJSON(t, srv.URL+"/api/recordings/"+itoa(mixed), &single); resp.StatusCode != http.StatusOK {
		t.Fatalf("single status = %d", resp.StatusCode)
	}
	if single.EncodeStatus == nil || len(*single.EncodeStatus) != 2 {
		t.Fatalf("single: encodeStatus = %+v, want exactly 2 entries", single.EncodeStatus)
	}
}

// TestEncodeJobStatusesFromFields は導出を DB なしで固定する
// （encodeJobStatusesFromFields、issue #316）。
func TestEncodeJobStatusesFromFields(t *testing.T) {
	for _, tc := range []struct {
		name    string
		desired []string
		done    []string
		attempt map[string]string // profile -> state ("running"/"failed")
		want    map[string]EncodeJobStatusState
	}{
		{
			name:    "プロファイル未設定",
			desired: nil,
			want:    map[string]EncodeJobStatusState{},
		},
		{
			name:    "全プロファイル完了済み",
			desired: []string{"h264"},
			done:    []string{"h264"},
			want:    map[string]EncodeJobStatusState{},
		},
		{
			name:    "試行行が無ければ queued",
			desired: []string{"h264"},
			want:    map[string]EncodeJobStatusState{"h264": EncodeJobStatusStateQueued},
		},
		{
			name:    "試行行が running",
			desired: []string{"h264"},
			attempt: map[string]string{"h264": "running"},
			want:    map[string]EncodeJobStatusState{"h264": EncodeJobStatusStateRunning},
		},
		{
			name:    "試行行が failed",
			desired: []string{"h264"},
			attempt: map[string]string{"h264": "failed"},
			want:    map[string]EncodeJobStatusState{"h264": EncodeJobStatusStateFailed},
		},
		{
			name:    "完了・running・queued が混在",
			desired: []string{"h264", "h265", "aac"},
			done:    []string{"h264"},
			attempt: map[string]string{"h265": "running"},
			want: map[string]EncodeJobStatusState{
				"h265": EncodeJobStatusStateRunning,
				"aac":  EncodeJobStatusStateQueued,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := recordingListFields{ID: 1, EncodeProfiles: tc.desired}
			if len(tc.attempt) > 0 {
				rows := make([]encodeAttemptRow, 0, len(tc.attempt))
				for profile, state := range tc.attempt {
					rows = append(rows, encodeAttemptRow{Profile: profile, State: state})
				}
				b, err := json.Marshal(rows)
				if err != nil {
					t.Fatalf("marshaling attempts: %v", err)
				}
				fields.EncodeAttempts = b
			}

			got, err := encodeJobStatusesFromFields(fields, tc.done)
			if err != nil {
				t.Fatalf("encodeJobStatusesFromFields: %v", err)
			}
			gotMap := map[string]EncodeJobStatusState{}
			for _, s := range got {
				gotMap[s.Profile] = s.State
			}
			if len(gotMap) != len(tc.want) {
				t.Fatalf("got = %+v, want %+v", gotMap, tc.want)
			}
			for profile, wantState := range tc.want {
				if gotMap[profile] != wantState {
					t.Errorf("%s: state = %q, want %q", profile, gotMap[profile], wantState)
				}
			}
		})
	}
}
