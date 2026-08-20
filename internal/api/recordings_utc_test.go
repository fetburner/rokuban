package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestRecordingFromListFields_NormalizesTimestampsToUTC は issue #366 の
// 修正（utcTimePtr / .UTC()）そのものを、DB や scan mode に依存しない形で
// 固定する。
//
// PR #408 のレビューが指摘した通り、`TestGetRecording_MatchesListElement`
// （list/detail 2 経路の struct 比較）は CI（プロセス TZ・Postgres セッション
// TimeZone のどちらも UTC）では修正の有無に関わらず緑になる --- pgx が
// Location を取り違えるのは「プロセス TZ とセッション TimeZone が食い違う」
// ときだけなので、両方 UTC の CI はそもそも症状の外側にいる。
//
// ここでは pgx の scan を経由せず、recordingFromListFields /
// ingestProgressFromFields に非 UTC の Location（JST, +9h）を積んだ
// *time.Time を直接渡す。実行環境（プロセス TZ・セッション TimeZone・
// pgx の protocol）に関係なく常に同じ入力になるので、CI で決定的に落ちる。
//
// 対象は utcTimePtr を通した 4 フィールド（startedAt / endedAt / deletedAt /
// ingest.observedAt）と、直接 .UTC() を呼んでいる 2 フィールド（startAt /
// createdAt）の合計 6 フィールド全部 --- ブロッカー 2 が指摘した「4
// フィールドのどれを外しても既存テストは気付かない」を個別に塞ぐため、
// assertUTCSuffix で 1 フィールドずつ検証する。
func TestRecordingFromListFields_NormalizesTimestampsToUTC(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	// UTC の壁時計と異なる instant にしておく（Location だけでなく壁時計の
	// 値も JST 由来であることをはっきりさせる）。
	nonUTC := time.Date(2026, 8, 20, 21, 0, 0, 0, jst)

	written := int64(1234)
	fields := recordingListFields{
		ID:                  1,
		Site:                db.DefaultSite,
		Source:              "manual",
		ServiceName:         "テスト局",
		ChannelType:         "GR",
		Channel:             "27",
		NetworkID:           1,
		ServiceID:           1,
		EventID:             1,
		Title:               "UTC 正規化の確認用",
		ProgramStartAt:      nonUTC,
		ProgramDurationMs:   1800000,
		Status:              "finished",
		StartedAt:           &nonUTC,
		EndedAt:             &nonUTC,
		DeletedAt:           &nonUTC,
		CreatedAt:           nonUTC,
		IngestWrittenBytes:  &written,
		IngestObservedAt:    &nonUTC,
		HasIngestableRecord: true,
	}

	rec, err := recordingFromListFields(fields, true, nil)
	if err != nil {
		t.Fatalf("recordingFromListFields: %v", err)
	}

	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshalling recording: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshalling recording into map: %v", err)
	}

	assertUTCSuffix(t, decoded, "startAt")
	assertUTCSuffix(t, decoded, "startedAt")
	assertUTCSuffix(t, decoded, "endedAt")
	assertUTCSuffix(t, decoded, "deletedAt")
	assertUTCSuffix(t, decoded, "createdAt")

	rawIngest, ok := decoded["ingest"]
	if !ok {
		t.Fatalf("recording JSON has no \"ingest\" field: %s", body)
	}
	var ingest map[string]json.RawMessage
	if err := json.Unmarshal(rawIngest, &ingest); err != nil {
		t.Fatalf("unmarshalling ingest into map: %v", err)
	}
	assertUTCSuffix(t, ingest, "observedAt")
}

// assertUTCSuffix は decoded[field] が JSON 文字列で、末尾が "Z"（UTC の
// RFC3339 表現）であることを確認する。フィールドが無ければ Fatal（存在すべき
// フィールドの欠落は別の壊れ方なので、このテストの主張とは分けて落とす）。
func assertUTCSuffix(t *testing.T, decoded map[string]json.RawMessage, field string) {
	t.Helper()
	raw, ok := decoded[field]
	if !ok {
		t.Fatalf("field %q is missing from JSON", field)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("field %q is not a JSON string (%s): %v", field, raw, err)
	}
	if !bytes.HasSuffix([]byte(s), []byte("Z")) {
		t.Errorf("field %q = %q, want RFC3339 with UTC (\"Z\") suffix, got non-UTC offset", field, s)
	}
}

// nonLocalPostgresTimeZone は time.Local の現在のオフセットと異なることが
// 保証された Postgres の TimeZone 名を返す。
//
// TestGetRecording_MatchesListElement_AcrossSessionTimeZone は「pgx の
// binary path が time.Local を使う」ことを使って一覧/単体の Location 不一致を
// 人工的に作る（issue #366 の実際の再現条件）。host の time.Local が
// たまたま "Asia/Tokyo" と同じオフセットだと再現が消えるので、その場合だけ
// "UTC" に切り替える（"UTC" のオフセットは常に 0 で "Asia/Tokyo" の +9h とは
// 異なる）。
func nonLocalPostgresTimeZone() string {
	_, offset := time.Now().In(time.Local).Zone()
	if offset == 9*3600 {
		return "UTC"
	}
	return "Asia/Tokyo"
}

// TestGetRecording_MatchesListElement_AcrossSessionTimeZone は issue #366
// レビューのブロッカー 1 が要求する「DB ありの経路テスト」。
//
// TestGetRecording_MatchesListElement（struct 比較）は CI のように
// プロセス TZ とセッション TimeZone が両方 UTC の環境では修正の有無に
// 関わらず緑になる（症状そのものが「両者が食い違う」ときにしか出ないため）。
// ここでは pool の全接続に `SET TIME ZONE` で非 UTC のセッション TimeZone を
// 人工的に与え、CI・developer 環境のどちらでも決定的に食い違いを再現する。
//
// queryRecordings は pgx.QueryExecModeExec を明示していて text protocol
// （セッション TimeZone で decode）を使い、queryRecordingByID は既定の
// binary protocol（プロセスの time.Local で decode）を使う（recordings.go の
// utcTimePtr の doc コメント）。同じ pool でもクエリ単位でこの差が出るので、
// セッション TimeZone を 1 箇所変えるだけで両経路の Location が食い違う
// 状況を作れる。
//
// list 側の要素と detail の**生レスポンス本文**（デコード後の struct では
// なく wire representation そのもの）を比較する --- 生 JSON 比較は
// レビューが指摘した「struct 比較は Location 差を time.Time の Equal で
// 吸収してしまう」を避けるため（*time.Time の Location 差は
// reflect.DeepEqual では拾えるが、この比較はより issue の意図（wire
// representation の一致）に忠実な形にする）。
func TestGetRecording_MatchesListElement_AcrossSessionTimeZone(t *testing.T) {
	pool := testutil.SetupDB(t)

	zone := nonLocalPostgresTimeZone()
	cfg := pool.Config().Copy()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, fmt.Sprintf("SET TIME ZONE %s", pgx.Identifier{zone}.Sanitize()))
		return err
	}
	tzPool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("creating timezone-shifted pool: %v", err)
	}
	t.Cleanup(tzPool.Close)

	srv := newAPIServer(t, tzPool)

	base := time.Now().Truncate(time.Second)
	id := seedRecording(t, tzPool, "一覧要素と単体取得のセッション TimeZone 不一致確認用", base, "finished", 41)
	seedIngested(t, tzPool, id, 4000, map[int32][4]int64{
		0x100: {700, 1, 0, 0},
	})

	listResp, err := http.Get(srv.URL + "/api/recordings")
	if err != nil {
		t.Fatalf("GET /api/recordings: %v", err)
	}
	listBody, err := io.ReadAll(listResp.Body)
	_ = listResp.Body.Close()
	if err != nil {
		t.Fatalf("reading list body: %v", err)
	}
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200, body=%s", listResp.StatusCode, listBody)
	}

	var rawList []json.RawMessage
	if err := json.Unmarshal(listBody, &rawList); err != nil {
		t.Fatalf("unmarshalling list body into raw elements: %v", err)
	}
	var list []Recording
	if err := json.Unmarshal(listBody, &list); err != nil {
		t.Fatalf("unmarshalling list body: %v", err)
	}
	idx := -1
	for i := range list {
		if list[i].Id == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("recording %d not found in list %+v", id, list)
	}

	detailResp, err := http.Get(fmt.Sprintf("%s/api/recordings/%d", srv.URL, id))
	if err != nil {
		t.Fatalf("GET /api/recordings/%d: %v", id, err)
	}
	detailBody, err := io.ReadAll(detailResp.Body)
	_ = detailResp.Body.Close()
	if err != nil {
		t.Fatalf("reading detail body: %v", err)
	}
	if detailResp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d, want 200, body=%s", detailResp.StatusCode, detailBody)
	}

	if !bytes.Equal(bytes.TrimSpace(rawList[idx]), bytes.TrimSpace(detailBody)) {
		t.Errorf("session TimeZone=%s (time.Local offset differs by construction): "+
			"list element and detail response bodies differ:\nlist   = %s\ndetail = %s",
			zone, rawList[idx], detailBody)
	}
}
