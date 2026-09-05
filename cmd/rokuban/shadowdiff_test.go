package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/epgstation"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestRunShadowDiff_EndToEnd は DB（reservations / program_intents）と
// EPGStation（httptest サーバー）の両方から実際にデータを集めて Compare するところまでを
// 通しで確認する。CLI 本体（cobra RunE）はごく薄い配線なので、ここでは
// runShadowDiff / printShadowDiffReport という切り出した関数を直接叩く。
func TestRunShadowDiff_EndToEnd(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	start := time.Date(2026, 8, 1, 21, 0, 0, 0, time.UTC)

	// #27 で番組の事実のスナップショットが program_snapshots に抽出され、
	// reservations / program_intents への FK が張られたため、各 programId の
	// 予約行・意図より先に program_snapshots を作る。
	upsertSnapshot(t, ctx, q, 1, "番組1", start)
	upsertSnapshot(t, ctx, q, 2, "番組2", start.Add(time.Hour))
	upsertSnapshot(t, ctx, q, 3, "番組3", start.Add(2*time.Hour))
	upsertSnapshot(t, ctx, q, 5, "重複排除された番組", start.Add(5*time.Hour))

	// Rokuban 側: programId=1 は両方に存在させる（Both）
	if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: db.DefaultSite, ProgramID: 1,
	}); err != nil {
		t.Fatalf("creating reservation 1: %v", err)
	}
	// programId=2 は Rokuban だけ（RokubanOnly）
	if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: db.DefaultSite, ProgramID: 2,
	}); err != nil {
		t.Fatalf("creating reservation 2: %v", err)
	}
	// programId=3 は Rokuban 側で skip 意図（EPGStation 側にはあるが Expected）
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site: db.DefaultSite, ProgramID: 3,
	}); err != nil {
		t.Fatalf("skipping program 3: %v", err)
	}
	// programId=5 は base.skip = true（M2-6 の重複排除が立てる想定）を持つ予約。
	// ruler は base だけを書き reservations 行自体は削除しない設計なので、この
	// 行は ListReservationsForSyncEvaluation（never-scheduled 除外。issue #98 で
	// orphaned_at IS NULL から置き換わった）に残り続ける。
	// reconciler.listDesired が effective.skip として除外して mirakc に同期しない
	// （＝ Rokuban は実際には録らない）のと同じ判定を runShadowDiff もしないと、
	// EPGStation 側に対応する予約があるとき Both（一致）に誤分類されてしまう
	// （見逃しを「一致」だと言い張る、shadow-diff にとって最悪の壊れ方。issue #54
	// の回帰テスト本体）。ruler パッケージには触れず、reservations.base を
	// 直接書き換えて模す（生 SQL で状態を固定するパターン）。
	res5, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: db.DefaultSite, ProgramID: 5,
	})
	if err != nil {
		t.Fatalf("creating reservation 5: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE reservations SET base = $1 WHERE site = $2 AND program_id = $3`,
		[]byte(`{"skip": true}`), res5.Site, res5.ProgramID); err != nil {
		t.Fatalf("setting base.skip for reservation 5: %v", err)
	}

	// EPGStation 側のフィクスチャ
	reserves := []epgstation.Reserve{
		{ID: 100, ProgramID: int64Ptr(1), Name: "番組1", StartAt: start.UnixMilli(), EndAt: start.Add(30 * time.Minute).UnixMilli()},
		{ID: 101, ProgramID: int64Ptr(3), Name: "番組3", StartAt: start.Add(2 * time.Hour).UnixMilli(), EndAt: start.Add(2*time.Hour + 30*time.Minute).UnixMilli()},
		{ID: 102, ProgramID: int64Ptr(4), Name: "番組4", StartAt: start.Add(3 * time.Hour).UnixMilli(), EndAt: start.Add(3*time.Hour + 30*time.Minute).UnixMilli()},
		{ID: 103, ProgramID: nil, IsTimeSpecified: true, Name: "時刻指定予約", StartAt: start.Add(4 * time.Hour).UnixMilli(), EndAt: start.Add(4*time.Hour + 30*time.Minute).UnixMilli()},
		{ID: 104, ProgramID: int64Ptr(5), Name: "重複排除された番組", StartAt: start.Add(5 * time.Hour).UnixMilli(), EndAt: start.Add(5*time.Hour + 30*time.Minute).UnixMilli()},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"reserves": %s, "total": %d}`, mustMarshalReserves(t, reserves), len(reserves))
	}))
	defer srv.Close()

	epgClient := epgstation.NewClient(srv.URL, srv.Client())

	report, err := runShadowDiff(ctx, q, epgClient, db.DefaultSite)
	if err != nil {
		t.Fatalf("runShadowDiff: %v", err)
	}

	// Both は programId=1 だけ。programId=5 は EPGStation 側にも対応する予約が
	// あるが base.skip = true なので Both に落ちてはならない（回帰テスト本体）。
	if len(report.Both) != 1 || report.Both[0].ProgramID != 1 {
		t.Errorf("Both = %+v, want [programId=1] (programId=5 must not appear here despite "+
			"having a matching EPGStation reserve — it has base.skip = true)", report.Both)
	}
	if len(report.RokubanOnly) != 1 || report.RokubanOnly[0].ProgramID != 2 {
		t.Errorf("RokubanOnly = %+v, want [programId=2]", report.RokubanOnly)
	}
	if len(report.EPGStationOnly) != 1 || report.EPGStationOnly[0].ProgramID != 4 {
		t.Errorf("EPGStationOnly = %+v, want [programId=4]", report.EPGStationOnly)
	}
	// skip 意図（3）・時刻指定（103）・base.skip（5）の 3 件。
	if len(report.Expected) != 3 {
		t.Fatalf("len(Expected) = %d, want 3 (skip intent + time-specified + base.skip): %+v", len(report.Expected), report.Expected)
	}
	var sawProgram5InExpected bool
	for _, item := range report.Expected {
		if item.ProgramID == 5 {
			sawProgram5InExpected = true
		}
	}
	if !sawProgram5InExpected {
		t.Errorf("Expected = %+v, want programId=5 (base.skip) among them", report.Expected)
	}
	if !report.HasUnexplained() {
		t.Error("HasUnexplained() = false, want true (RokubanOnly/EPGStationOnly present)")
	}

	var out bytes.Buffer
	if err := printShadowDiffReport(&out, report); err != nil {
		t.Fatalf("printShadowDiffReport: %v", err)
	}
	rendered := out.String()

	for _, want := range []string{
		"RokubanOnly:     1",
		"EPGStationOnly:  1",
		"Expected:        3",
		"番組2",               // RokubanOnly の題名
		"番組4",               // EPGStationOnly の題名
		"時刻指定予約は programId", // allowlist の理由
		"JST",               // 時刻表示
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("output does not contain %q:\n%s", want, rendered)
		}
	}

	// JST 表示になっていること。RokubanOnly の programId=2 は start+1h UTC
	// = 22:00 UTC = 翌日 07:00 JST。
	if !strings.Contains(rendered, "2026-08-02 07:00:00") {
		t.Errorf("output should show start time converted to JST:\n%s", rendered)
	}
}

// TestRunShadowDiff_NoUnexplainedDiff は完全一致のケースで HasUnexplained が false になることを確認する。
func TestRunShadowDiff_NoUnexplainedDiff(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	start := time.Date(2026, 8, 1, 21, 0, 0, 0, time.UTC)
	upsertSnapshot(t, ctx, q, 1, "番組1", start)
	if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: db.DefaultSite, ProgramID: 1,
	}); err != nil {
		t.Fatalf("creating reservation: %v", err)
	}

	reserves := []epgstation.Reserve{
		{ID: 100, ProgramID: int64Ptr(1), Name: "番組1", StartAt: start.UnixMilli(), EndAt: start.Add(30 * time.Minute).UnixMilli()},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"reserves": %s, "total": %d}`, mustMarshalReserves(t, reserves), len(reserves))
	}))
	defer srv.Close()

	report, err := runShadowDiff(ctx, q, epgstation.NewClient(srv.URL, srv.Client()), db.DefaultSite)
	if err != nil {
		t.Fatalf("runShadowDiff: %v", err)
	}
	if report.HasUnexplained() {
		t.Errorf("HasUnexplained() = true, want false: %+v", report)
	}
}

// upsertSnapshot は program_snapshots 行を用意する。#27 で番組の事実の
// スナップショット（title / 開始時刻 / 尺）が program_snapshots に抽出され、
// reservations / program_intents への FK が張られたため、このテストの
// フィクスチャはすべてこれを先に呼ぶ。
func upsertSnapshot(t *testing.T, ctx context.Context, q *sqlcgen.Queries, programID int64, title string, startAt time.Time) {
	t.Helper()
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site:        db.DefaultSite,
		ProgramID:   programID,
		Title:       title,
		StartAt:     startAt,
		DurationMs:  1800000,
		NetworkID:   32736,
		ServiceID:   1024,
		ChannelType: "GR",
		Channel:     "27",
		EventID:     int32(programID % 100000),
		ServiceName: "テスト局",
	}); err != nil {
		t.Fatalf("upserting program snapshot %d: %v", programID, err)
	}
}

func int64Ptr(v int64) *int64 { return &v }

func mustMarshalReserves(t *testing.T, reserves []epgstation.Reserve) []byte {
	t.Helper()
	data, err := json.Marshal(reserves)
	if err != nil {
		t.Fatalf("marshalling reserves fixture: %v", err)
	}
	return data
}

// 到達不能な DB（127.0.0.1:1）+ 2 サイトのレジストリ。server_test.go の
// serverCmdTestConfig と同じ手法（RunE を丸ごと走らせ、site 解決を通れば DB 接続
// エラーで落ち、通らなければそれより手前のエラーで落ちる。CLAUDE.md「壊す場所を、
// 実際に壊れる経路の上に置く」）。
const shadowDiffCmdTestConfigTwoSites = `
db:
  host: 127.0.0.1
  port: 1
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: tokyo
    url: http://mirakc-tokyo:40772
  - site: takamatsu
    url: http://mirakc-takamatsu:40772
storage:
  media_dir: /mnt/media
`

const shadowDiffCmdTestConfigOneSite = `
db:
  host: 127.0.0.1
  port: 1
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: tokyo
    url: http://mirakc-tokyo:40772
storage:
  media_dir: /mnt/media
`

// runShadowDiffCmdForTest は shadow-diff サブコマンドの RunE を実際に走らせる。
// 単体の resolveSiteFlag だけを直接呼ぶテストでは RunE の配線（--site フラグの
// 登録・requireSingleSite からの置き換え）が検証できない（server_test.go の
// runServerCmdForTest と同じ理由）。
func runShadowDiffCmdForTest(t *testing.T, configPath string, args ...string) error {
	t.Helper()
	cmd := newShadowDiffCmd()
	cmd.Flags().String("config", configPath, "")
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd.Execute()
}

// mirakcs: 2 要素 + --site tokyo は site 解決を通り、DB 接続まで進むこと
// （issue #533 の受け入れ基準 1）。
func TestShadowDiffCmd_SiteFlag_MultiSiteRegistryResolvesNamedSite(t *testing.T) {
	path := writeServerTestConfig(t, shadowDiffCmdTestConfigTwoSites)
	err := runShadowDiffCmdForTest(t, path, "--epgstation-url", "http://epgstation.invalid", "--site", "tokyo")
	if err == nil {
		t.Fatal("到達不能な DB を指しているので error を期待したが nil だった")
	}
	if !strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v, want to fail at the DB stage (= site 解決を通ったこと)", err)
	}
}

// mirakcs: 2 要素 + --site 省略は DB に触る前の起動エラーになること
// （issue #533 の受け入れ基準 1）。
func TestShadowDiffCmd_SiteFlag_MultiSiteRegistryRequiresSite(t *testing.T) {
	path := writeServerTestConfig(t, shadowDiffCmdTestConfigTwoSites)
	err := runShadowDiffCmdForTest(t, path, "--epgstation-url", "http://epgstation.invalid")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "--site is required") {
		t.Errorf("err = %v, want the --site required error (DB に触る前に落ちること)", err)
	}
	if strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v: DB まで進んでいる（--site の必須化が効いていない）", err)
	}
}

// レジストリに無い --site はタイポの早期検出のため起動エラーになり、
// どの site にも一致しないまま静かに成功してはならない（issue #533 の「罠」）。
func TestShadowDiffCmd_SiteFlag_UnknownSiteIsError(t *testing.T) {
	path := writeServerTestConfig(t, shadowDiffCmdTestConfigTwoSites)
	err := runShadowDiffCmdForTest(t, path, "--epgstation-url", "http://epgstation.invalid", "--site", "osaka")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown site") {
		t.Errorf("err = %v, want an unknown-site error", err)
	}
	if strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v: DB まで進んでいる（未知 site の照合が効いていない）", err)
	}
}

// mirakcs: 1 要素では --site 省略でも従来どおり動くこと（issue #533 の受け入れ
// 基準 2）。
func TestShadowDiffCmd_SiteFlag_SingleSiteRegistryOptional(t *testing.T) {
	path := writeServerTestConfig(t, shadowDiffCmdTestConfigOneSite)
	err := runShadowDiffCmdForTest(t, path, "--epgstation-url", "http://epgstation.invalid")
	if err == nil {
		t.Fatal("到達不能な DB を指しているので error を期待したが nil だった")
	}
	if !strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v, want to fail at the DB stage (単一サイトレジストリは --site 無しで解決する)", err)
	}
}
