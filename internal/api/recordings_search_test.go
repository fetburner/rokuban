package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// seedRecordingOpts は seedRecordingFull の入力（issue #136 のテスト専用。
// seedRecording は既存の最小フィクスチャなので変えず、こちらは絞り込み軸を
// 網羅するために別に用意する）。
type seedRecordingOpts struct {
	title       string
	description string
	start       time.Time
	status      string
	eventID     int32
	serviceID   int32
	channelType string
	genres      string // jsonb リテラル（空なら NULL）
	source      string // 既定 "manual"
	ruleID      *int64
}

func seedRecordingFull(t *testing.T, pool *pgxpool.Pool, o seedRecordingOpts) int64 {
	t.Helper()
	source := o.source
	if source == "" {
		source = "manual"
	}
	channelType := o.channelType
	if channelType == "" {
		channelType = "GR"
	}
	serviceID := o.serviceID
	if serviceID == 0 {
		serviceID = 5168
	}
	var description *string
	if o.description != "" {
		description = &o.description
	}
	var genres json.RawMessage
	if o.genres != "" {
		genres = json.RawMessage(o.genres)
	}
	id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		RuleID:            o.ruleID,
		Source:            source,
		Site:              db.DefaultSite,
		NetworkID:         32678,
		ServiceID:         serviceID,
		EventID:           o.eventID,
		ServiceName:       "テスト局",
		ChannelType:       channelType,
		Channel:           "27",
		Title:             o.title,
		Description:       description,
		Genres:            genres,
		ProgramStartAt:    o.start,
		ProgramDurationMs: (30 * time.Minute).Milliseconds(),
		Status:            o.status,
	})
	if err != nil {
		t.Fatalf("seeding recording (full): %v", err)
	}
	return id
}

// seedRule は rules に最小行を作り id を返す（recordings.rule_id の FK 先）。
func seedRule(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO rules (name) VALUES ($1) RETURNING id`, name,
	).Scan(&id); err != nil {
		t.Fatalf("seeding rule: %v", err)
	}
	return id
}

func recordingsURL(base string, params url.Values) string {
	if len(params) == 0 {
		return base + "/api/recordings"
	}
	return base + "/api/recordings?" + params.Encode()
}

// getRecordings は 200 を期待して GET /api/recordings?... を呼び、タイトルの
// 集合を返す（順序も見たいテストは呼び出し側で直接 got を使う）。
func getRecordingsTitles(t *testing.T, srvURL string, params url.Values) []string {
	t.Helper()
	var got []Recording
	resp := getJSON(t, recordingsURL(srvURL, params), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/recordings?%s status = %d, want 200", params.Encode(), resp.StatusCode)
	}
	titles := make([]string, len(got))
	for i, r := range got {
		titles[i] = r.Title
	}
	return titles
}

// 全角キーワードが normalize_search_text 経由で半角番組名に当たる
// （EPG 検索 /search と同じ揺れ吸収。docs/data.md §5）。
func TestListRecordings_KeywordSearch(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	base := time.Now().Truncate(time.Second)

	seedRecordingFull(t, pool, seedRecordingOpts{title: "NHKニュース", start: base, status: "finished", eventID: 1})
	seedRecordingFull(t, pool, seedRecordingOpts{title: "無関係の番組", description: "NHK特集について", start: base.Add(time.Minute), status: "finished", eventID: 2})
	seedRecordingFull(t, pool, seedRecordingOpts{title: "全く関係ない", start: base.Add(2 * time.Minute), status: "finished", eventID: 3})

	// 全角「ＮＨＫ」で半角「NHK」を含む録画に当たる（title のみ検索）
	params := url.Values{"q": {"ＮＨＫ"}, "qTarget": {"title"}}
	titles := getRecordingsTitles(t, srv.URL, params)
	if len(titles) != 1 || titles[0] != "NHKニュース" {
		t.Fatalf("qTarget=title got %v, want [NHKニュース]", titles)
	}

	// qTarget=title だと description しか一致しない録画は出ない
	for _, tt := range titles {
		if tt == "無関係の番組" {
			t.Errorf("qTarget=title で description 一致の録画が出た: %v", titles)
		}
	}

	// 既定（titleDescription）は両方に当たる
	titles = getRecordingsTitles(t, srv.URL, url.Values{"q": {"ＮＨＫ"}})
	if len(titles) != 2 {
		t.Fatalf("qTarget=titleDescription(既定) got %v, want 2 件", titles)
	}
}

// genre（genre_lv1 の重なり）で絞り込める。00034 で追加した生成列。
func TestListRecordings_GenreFilter(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	base := time.Now().Truncate(time.Second)

	seedRecordingFull(t, pool, seedRecordingOpts{
		title: "ニュース番組", start: base, status: "finished", eventID: 1,
		genres: `[{"lv1":1,"lv2":0,"un1":0,"un2":0}]`,
	})
	seedRecordingFull(t, pool, seedRecordingOpts{
		title: "スポーツ番組", start: base.Add(time.Minute), status: "finished", eventID: 2,
		genres: `[{"lv1":3,"lv2":0,"un1":0,"un2":0}]`,
	})
	seedRecordingFull(t, pool, seedRecordingOpts{
		title: "ジャンル無し番組", start: base.Add(2 * time.Minute), status: "finished", eventID: 3,
	})

	titles := getRecordingsTitles(t, srv.URL, url.Values{"genre": {"1"}})
	if len(titles) != 1 || titles[0] != "ニュース番組" {
		t.Fatalf("genre=1 got %v, want [ニュース番組]", titles)
	}

	// 複数指定は OR（重なりがあればマッチ）
	titles = getRecordingsTitles(t, srv.URL, url.Values{"genre": {"1", "3"}})
	if len(titles) != 2 {
		t.Fatalf("genre=1,3 got %v, want 2 件", titles)
	}

	// ジャンル無し・該当なしは出ない
	for _, tt := range titles {
		if tt == "ジャンル無し番組" {
			t.Errorf("ジャンル無し番組が genre フィルタに出た: %v", titles)
		}
	}
}

// channelType / serviceId は複数指定可。
func TestListRecordings_ChannelTypeAndServiceID(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	base := time.Now().Truncate(time.Second)

	seedRecordingFull(t, pool, seedRecordingOpts{title: "GR番組", start: base, status: "finished", eventID: 1, channelType: "GR", serviceID: 100})
	seedRecordingFull(t, pool, seedRecordingOpts{title: "BS番組", start: base.Add(time.Minute), status: "finished", eventID: 2, channelType: "BS", serviceID: 200})
	seedRecordingFull(t, pool, seedRecordingOpts{title: "CS番組", start: base.Add(2 * time.Minute), status: "finished", eventID: 3, channelType: "CS", serviceID: 300})

	titles := getRecordingsTitles(t, srv.URL, url.Values{"channelType": {"BS"}})
	if len(titles) != 1 || titles[0] != "BS番組" {
		t.Fatalf("channelType=BS got %v, want [BS番組]", titles)
	}

	titles = getRecordingsTitles(t, srv.URL, url.Values{"channelType": {"BS", "CS"}})
	if len(titles) != 2 {
		t.Fatalf("channelType=BS,CS got %v, want 2 件", titles)
	}

	titles = getRecordingsTitles(t, srv.URL, url.Values{"serviceId": {"100"}})
	if len(titles) != 1 || titles[0] != "GR番組" {
		t.Fatalf("serviceId=100 got %v, want [GR番組]", titles)
	}

	titles = getRecordingsTitles(t, srv.URL, url.Values{"serviceId": {"100", "200"}})
	if len(titles) != 2 {
		t.Fatalf("serviceId=100,200 got %v, want 2 件", titles)
	}
}

// status / source / ruleId は録画自身の観測・出自での絞り込み。
func TestListRecordings_StatusSourceRuleID(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	base := time.Now().Truncate(time.Second)
	ruleID := seedRule(t, pool, "テストルール")

	seedRecordingFull(t, pool, seedRecordingOpts{title: "録画中", start: base, status: "recording", eventID: 1})
	seedRecordingFull(t, pool, seedRecordingOpts{title: "失敗", start: base.Add(time.Minute), status: "failed", eventID: 2})
	seedRecordingFull(t, pool, seedRecordingOpts{title: "手動完了", start: base.Add(2 * time.Minute), status: "finished", eventID: 3, source: "manual"})
	seedRecordingFull(t, pool, seedRecordingOpts{title: "ルール完了", start: base.Add(3 * time.Minute), status: "finished", eventID: 4, source: "rule", ruleID: &ruleID})

	titles := getRecordingsTitles(t, srv.URL, url.Values{"status": {"failed"}})
	if len(titles) != 1 || titles[0] != "失敗" {
		t.Fatalf("status=failed got %v, want [失敗]", titles)
	}

	titles = getRecordingsTitles(t, srv.URL, url.Values{"source": {"rule"}})
	if len(titles) != 1 || titles[0] != "ルール完了" {
		t.Fatalf("source=rule got %v, want [ルール完了]", titles)
	}

	titles = getRecordingsTitles(t, srv.URL, url.Values{"ruleId": {fmt.Sprint(ruleID)}})
	if len(titles) != 1 || titles[0] != "ルール完了" {
		t.Fatalf("ruleId=%d got %v, want [ルール完了]", ruleID, titles)
	}
}

// from / to は program_start_at の範囲（from 以上 to 未満）。
func TestListRecordings_FromTo(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	base := time.Now().Truncate(time.Second)

	seedRecordingFull(t, pool, seedRecordingOpts{title: "1時間前", start: base.Add(-time.Hour), status: "finished", eventID: 1})
	seedRecordingFull(t, pool, seedRecordingOpts{title: "30分前", start: base.Add(-30 * time.Minute), status: "finished", eventID: 2})
	seedRecordingFull(t, pool, seedRecordingOpts{title: "現在", start: base, status: "finished", eventID: 3})
	seedRecordingFull(t, pool, seedRecordingOpts{title: "30分後", start: base.Add(30 * time.Minute), status: "finished", eventID: 4})

	params := url.Values{
		"from": {base.Add(-45 * time.Minute).Format(rfc3339)},
		"to":   {base.Add(time.Minute).Format(rfc3339)},
	}
	titles := getRecordingsTitles(t, srv.URL, params)
	if len(titles) != 2 {
		t.Fatalf("from/to got %v, want [現在 30分前]（順不定 2 件）", titles)
	}
	found := map[string]bool{}
	for _, tt := range titles {
		found[tt] = true
	}
	if !found["30分前"] || !found["現在"] {
		t.Errorf("from/to got %v, want 30分前・現在 を含む", titles)
	}
	if found["1時間前"] || found["30分後"] {
		t.Errorf("from/to got %v, want 範囲外を含まない", titles)
	}

	// to は境界未満（含まない）: to をちょうど "現在" にすると現在は含まれない
	titles = getRecordingsTitles(t, srv.URL, url.Values{
		"from": {base.Add(-45 * time.Minute).Format(rfc3339)},
		"to":   {base.Format(rfc3339)},
	})
	for _, tt := range titles {
		if tt == "現在" {
			t.Errorf("to は境界を含まないはずだが 現在 が含まれた: %v", titles)
		}
	}
}

// trash と絞り込み条件は直交する。deleted_at IS NOT NULL の行は既定の一覧に
// 出ず、ごみ箱一覧では条件が同じように効く。
func TestListRecordings_TrashOrthogonal(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	base := time.Now().Truncate(time.Second)

	live := seedRecordingFull(t, pool, seedRecordingOpts{title: "生きているNHK番組", start: base, status: "finished", eventID: 1})
	trashed := seedRecordingFull(t, pool, seedRecordingOpts{title: "捨てたNHK番組", start: base.Add(time.Minute), status: "finished", eventID: 2})
	_ = live

	resp := doRecordingMethod(t, http.MethodDelete, fmt.Sprintf("%s/api/recordings/%d", srv.URL, trashed))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}

	// 通常一覧: 削除済みは出ない（q が当たっても）
	titles := getRecordingsTitles(t, srv.URL, url.Values{"q": {"NHK"}})
	if len(titles) != 1 || titles[0] != "生きているNHK番組" {
		t.Fatalf("trash=false q=NHK got %v, want [生きているNHK番組]", titles)
	}

	// ごみ箱一覧: 条件はごみ箱側でも効く
	titles = getRecordingsTitles(t, srv.URL, url.Values{"trash": {"true"}, "q": {"NHK"}})
	if len(titles) != 1 || titles[0] != "捨てたNHK番組" {
		t.Fatalf("trash=true q=NHK got %v, want [捨てたNHK番組]", titles)
	}
}

// キーセットページングは (program_start_at, id) の複合で割る。同一
// program_start_at の行（同時刻開始の別チャンネル）を含めても、ページを
// 辿った結果は重複も欠落も出ない。desc / asc 両方向で確認する。
//
// レビュー（PR #187 M2）: タイ群を 1 組だけ・limit=2 固定で置くと、そのタイ群が
// 「ページ境界の内側」に来るかどうかは並び順（desc/asc）とタイ群の位置の
// 組み合わせで決まってしまい、**片方向だけ壊れて他方向は通る**ケースが
// 再現した（タイ群を後ろにずらすと逆に asc が通って desc が落ちる）。
// つまり「desc と asc を両方走らせる」だけでは、たまたま境界がタイ群の
// 外側に来た方向を「通った」と誤読しうる。ここでは (a) タイ群を先頭寄り・
// 末尾寄りの 2 組置き、(b) limit を 1（境界が全行間に来る）・2・3 の
// 複数で回し、(c) 単一ページ大 limit で取った正解の並び順（asc は挿入順、
// desc はその逆順 --- 挿入を program_start_at 昇順・id 昇順で行っているため
// 一致する）と**位置まで含めて**一致することを確認する。これにより
// タイ群の位置に依存しない検証になる。
func TestListRecordings_KeysetPagination_DuplicateStartAt(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	base := time.Now().Truncate(time.Second)

	// asc の正解順（program_start_at 昇順・タイは id 昇順）をそのまま記録する。
	// 挿入順を program_start_at 昇順・id 昇順に揺れなく揃えているので、
	// wantAsc はここでの挿入順そのものになる。
	var wantAsc []int64
	seed := func(title string, start time.Time, eventID int32) {
		id := seedRecordingFull(t, pool, seedRecordingOpts{title: title, start: start, status: "finished", eventID: eventID})
		wantAsc = append(wantAsc, id)
	}
	seed("a", base, 1)                   // 単独
	seed("b1", base.Add(time.Minute), 2) // 先頭寄りのタイ群
	seed("b2", base.Add(time.Minute), 3)
	seed("c", base.Add(2*time.Minute), 4)  // 単独
	seed("d1", base.Add(3*time.Minute), 5) // 末尾寄りのタイ群
	seed("d2", base.Add(3*time.Minute), 6)
	seed("e", base.Add(4*time.Minute), 7) // 単独

	wantDesc := make([]int64, len(wantAsc))
	for i, id := range wantAsc {
		wantDesc[len(wantAsc)-1-i] = id
	}

	for _, order := range []string{"desc", "asc"} {
		want := wantDesc
		if order == "asc" {
			want = wantAsc
		}
		for _, limit := range []int{1, 2, 3} {
			t.Run(fmt.Sprintf("%s_limit%d", order, limit), func(t *testing.T) {
				var ids []int64
				before, beforeID := "", ""
				for page := 0; page < 20; page++ {
					params := url.Values{"limit": {fmt.Sprint(limit)}, "order": {order}}
					if before != "" {
						params.Set("before", before)
						params.Set("beforeId", beforeID)
					}
					var got []Recording
					resp := getJSON(t, recordingsURL(srv.URL, params), &got)
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("page %d status = %d", page, resp.StatusCode)
					}
					if len(got) == 0 {
						break
					}
					for _, r := range got {
						ids = append(ids, r.Id)
					}
					last := got[len(got)-1]
					before = last.StartAt.Format(rfc3339)
					beforeID = fmt.Sprint(last.Id)
				}
				if !equalInt64Slices(ids, want) {
					t.Fatalf("order=%s limit=%d got %v, want %v (exact order, 重複・欠落・順序入れ替わりのいずれも不可)", order, limit, ids, want)
				}
			})
		}
	}
}

func equalInt64Slices(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 入力検証: before / beforeId は両方揃って初めて有効。limit は 1..200。
func TestListRecordings_ValidationErrors(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	tests := []struct {
		name   string
		params url.Values
	}{
		{"before without beforeId", url.Values{"before": {time.Now().Format(rfc3339)}}},
		{"beforeId without before", url.Values{"beforeId": {"1"}}},
		{"limit zero", url.Values{"limit": {"0"}}},
		{"limit too large", url.Values{"limit": {"201"}}},
		// 罠（issue #136）「黙って 0 件にしない」: enum に一致しない値や
		// ドメイン外の genre は無視・切り詰めせず 400 にする（PR #187 レビュー O4）。
		{"invalid status", url.Values{"status": {"bogus"}}},
		{"invalid source", url.Values{"source": {"bogus"}}},
		{"invalid qTarget", url.Values{"qTarget": {"bogus"}}},
		{"invalid channelType", url.Values{"channelType": {"XX"}}},
		{"invalid order (wrong case)", url.Values{"order": {"ASC"}}},
		{"genre above domain max", url.Values{"genre": {"16"}}},
		{"genre negative", url.Values{"genre": {"-1"}}},
		{"genre wraps int16 if not validated", url.Values{"genre": {"32768"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := getJSON(t, recordingsURL(srv.URL, tt.params), nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var body ErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decoding error body: %v", err)
			}
			if body.Error == "" {
				t.Error("error message is empty")
			}
		})
	}

	// 境界（limit=1, limit=200）は通る
	for _, limit := range []string{"1", "200"} {
		resp := getJSON(t, recordingsURL(srv.URL, url.Values{"limit": {limit}}), nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("limit=%s status = %d, want 200", limit, resp.StatusCode)
		}
	}

	// genre の境界（0, 15）と正しい enum 値は通る
	for _, genre := range []string{"0", "15"} {
		resp := getJSON(t, recordingsURL(srv.URL, url.Values{"genre": {genre}}), nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("genre=%s status = %d, want 200", genre, resp.StatusCode)
		}
	}
	for _, tt := range []struct {
		name   string
		params url.Values
	}{
		{"status", url.Values{"status": {"finished"}}},
		{"source", url.Values{"source": {"manual"}}},
		{"qTarget", url.Values{"qTarget": {"title"}}},
		{"channelType", url.Values{"channelType": {"GR"}}},
		{"order", url.Values{"order": {"asc"}}},
	} {
		resp := getJSON(t, recordingsURL(srv.URL, tt.params), nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s valid value status = %d, want 200", tt.name, resp.StatusCode)
		}
	}
}
