package capacity

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

const testSite = "default"

// seedTuner は tuner_sync に 1 行入れる。
func seedTuner(t *testing.T, pool *pgxpool.Pool, index int, name string, types []string, available, fault bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO tuner_sync (site, tuner_index, name, types, is_available, is_fault)
VALUES ($1, $2, $3, $4, $5, $6)`, testSite, index, name, types, available, fault); err != nil {
		t.Fatalf("inserting tuner_sync row: %v", err)
	}
}

// seedReservation は program_snapshots と reservations に 1 行ずつ入れる。
// base は ruler が載せる導出オプション。#27 で番組の事実のスナップショット
// （title / 開始時刻 / 尺 / チャンネル識別）が program_snapshots に抽出された。
//
// event_id は他のテスト（TestLoad_ExcludesReservationsThatProduceNoSchedule の
// 「番組が終了済み」ケース）が seedNeverScheduledRecording で recordings 行を
// 作れるように、programID の下 5 桁から機械的に割り当てる（本番コードでの
// programId 分解は禁止だが、テストフィクスチャの一意な ID 割り当てとしてのみ
// 使う。internal/reconciler の createReservation ヘルパと同じ流儀）。
func seedReservation(
	t *testing.T, pool *pgxpool.Pool,
	programID int64, channelType, channel string,
	startAt time.Time, duration time.Duration, base string,
) {
	t.Helper()
	if base == "" {
		base = "{}"
	}
	ctx := context.Background()
	eventID := int32(programID % 100000)
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms, network_id, service_id, channel_type, channel,
  event_id, service_name
) VALUES ($1, $2, 'テスト番組', $3, $4, 32678, 5168, $5, $6, $7, 'テスト局')`,
		testSite, programID, startAt, duration.Milliseconds(), channelType, channel, eventID); err != nil {
		t.Fatalf("inserting program_snapshot row: %v", err)
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO reservations (site, program_id, base)
VALUES ($1, $2, $3)`,
		testSite, programID, base); err != nil {
		t.Fatalf("inserting reservation row: %v", err)
	}
}

// seedNeverScheduledRecording は reconciler.recordNeverScheduled（issue #98）が
// 実際に書く recordings 行を模す --- status='failed' + quality_events に
// recording.never-scheduled のマーカー。ListCapacityDemand がこの行の存在を
// 「schedule を作らない予約」として除外することを確認するための直接 INSERT。
func seedNeverScheduledRecording(t *testing.T, pool *pgxpool.Pool, networkID, serviceID, eventID int32) {
	t.Helper()
	ctx := context.Background()
	qe := `[{"at":"2026-01-01T00:00:00Z","event":"recording.never-scheduled","reason":{}}]`
	if _, err := pool.Exec(ctx, `
INSERT INTO recordings (
    source, site, network_id, service_id, event_id, service_name,
    channel_type, channel, title, program_start_at, program_duration_ms, status, quality_events
) VALUES ('manual', $1, $2, $3, $4, 'テスト局', 'GR', '25', 'テスト番組', now(), 1800000, 'failed', $5::jsonb)`,
		testSite, networkID, serviceID, eventID, qe); err != nil {
		t.Fatalf("seeding never-scheduled recording: %v", err)
	}
}

// seedMidRecordingFailure は handleRecordingFailed（internal/watcher）が作る形の
// failed 行を模す --- never-scheduled マーカーは無い。ListCapacityDemand が
// この行の存在で予約を需要から除外してはならないことを確認するための直接
// INSERT（issue #157: never_scheduled_events view の述語が status='failed'
// 全般に緩んでいないかの回帰確認）。
func seedMidRecordingFailure(t *testing.T, pool *pgxpool.Pool, networkID, serviceID, eventID int32) {
	t.Helper()
	ctx := context.Background()
	qe := `[{"event":"recording.failed","reason":"need-rescheduling"}]`
	if _, err := pool.Exec(ctx, `
INSERT INTO recordings (
    source, site, network_id, service_id, event_id, service_name,
    channel_type, channel, title, program_start_at, program_duration_ms, status, quality_events
) VALUES ('manual', $1, $2, $3, $4, 'テスト局', 'GR', '25', 'テスト番組', now(), 1800000, 'failed', $5::jsonb)`,
		testSite, networkID, serviceID, eventID, qe); err != nil {
		t.Fatalf("seeding mid-recording failure: %v", err)
	}
}

// 放送中の mirakc 由来の失敗（never-scheduled マーカー無し）は需要から除外
// しない（issue #157 の回帰確認。TestLoad_ExcludesReservationsThatProduceNoSchedule
// の「never-scheduled の recordings 行がある」ケースと対になる反転テスト）。
func TestLoad_MidRecordingFailureNotExcluded(t *testing.T) {
	pool := testutil.SetupDB(t)
	start := time.Now().Truncate(time.Hour).Add(24 * time.Hour)
	duration := time.Hour

	seedTuner(t, pool, 0, "PX-S1UD_T1", []string{"GR"}, true, false)
	seedReservation(t, pool, 100, "GR", "27", start, duration, "")
	seedReservation(t, pool, 101, "GR", "25", start, duration, "")
	seedMidRecordingFailure(t, pool, 32678, 5168, int32(101%100000))

	overages := loadOverages(t, pool)
	if len(overages) != 1 {
		t.Fatalf("overages = %+v, want 1 (never-scheduled マーカー無しの failed 行は需要から除外してはいけない)", overages)
	}
}

// seedReservationWithoutChannel（00009 以前の残骸＝チャンネル未設定を模す
// ヘルパー）は issue #101（00026）で program_snapshots のチャンネル・イベント
// 識別 6 列が NOT NULL 化されたことで削除した。この状態自体が DB レベルで
// 表現不可能になったため（INSERT が 23502 で落ちる）。NOT NULL が実際に
// 効いていることの回帰テストは internal/db/models_test.go の
// TestSchemaV1_ProgramSnapshotChannelIdentityNotNull にある。

func loadOverages(t *testing.T, pool *pgxpool.Pool) []Overage {
	t.Helper()
	overages, err := Load(context.Background(), sqlcgen.New(pool), testSite)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return overages
}

// GR 1 本しかない構成で、別チャンネルの 2 予約が超過として出ること。
// 予約行 → 需要 → Hall 条件までの配線が通っていることの確認。
func TestLoad_ReportsOverage(t *testing.T) {
	pool := testutil.SetupDB(t)
	start := time.Now().Truncate(time.Hour).Add(24 * time.Hour)

	seedTuner(t, pool, 0, "PX-S1UD_T1", []string{"GR"}, true, false)
	seedReservation(t, pool, 100, "GR", "27", start, time.Hour, "")
	seedReservation(t, pool, 101, "GR", "25", start, time.Hour, "")

	overages := loadOverages(t, pool)
	if len(overages) != 1 {
		t.Fatalf("overages = %+v, want 1", overages)
	}
	o := overages[0]
	if o.Site != testSite {
		t.Errorf("site = %q, want %q", o.Site, testSite)
	}
	if !o.StartAt.Equal(start) || !o.EndAt.Equal(start.Add(time.Hour)) {
		t.Errorf("interval = %v..%v, want %v..%v", o.StartAt, o.EndAt, start, start.Add(time.Hour))
	}
	if o.Shortfall != 1 {
		t.Errorf("shortfall = %d, want 1", o.Shortfall)
	}
	if len(o.JammedTypes) != 1 || o.JammedTypes[0] != "GR" {
		t.Errorf("jammedTypes = %v, want [GR]", o.JammedTypes)
	}

	// 反対方向: チューナーを 1 本足せば超過しない。
	seedTuner(t, pool, 1, "PX-S1UD_T2", []string{"GR"}, true, false)
	if overages := loadOverages(t, pool); len(overages) != 0 {
		t.Errorf("overages = %+v, want none after adding a second tuner", overages)
	}
}

// 数えるのは reconciler が実際に schedule を作る予約だけ。
func TestLoad_ExcludesReservationsThatProduceNoSchedule(t *testing.T) {
	ctx := context.Background()
	duration := time.Hour

	tests := []struct {
		name string
		// seed は 2 件目の予約（GR/25）を「schedule が作られない」形で入れる。
		seed func(t *testing.T, pool *pgxpool.Pool, start time.Time)
	}{
		{
			// 旧「state = orphaned」ケース。issue #98 で orphaned_at 列が
			// 廃止されたため、reconciler.recordNeverScheduled が実際に書く形
			// （recordings に never-scheduled 行がある）で模す。
			name: "never-scheduled の recordings 行がある",
			seed: func(t *testing.T, pool *pgxpool.Pool, start time.Time) {
				seedReservation(t, pool, 101, "GR", "25", start, duration, "")
				seedNeverScheduledRecording(t, pool, 32678, 5168, int32(101%100000))
			},
		},
		{
			name: "base.skip = true",
			seed: func(t *testing.T, pool *pgxpool.Pool, start time.Time) {
				seedReservation(t, pool, 101, "GR", "25", start, duration, `{"skip":true}`)
			},
		},
		{
			name: "overrides.skip = true",
			seed: func(t *testing.T, pool *pgxpool.Pool, start time.Time) {
				seedReservation(t, pool, 101, "GR", "25", start, duration, "")
				if _, err := sqlcgen.New(pool).UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
					Site: testSite, ProgramID: 101, Overrides: json.RawMessage(`{"skip":true}`),
				}); err != nil {
					t.Fatalf("upserting overrides: %v", err)
				}
			},
		},
		{
			name: "intent action = skip",
			seed: func(t *testing.T, pool *pgxpool.Pool, start time.Time) {
				seedReservation(t, pool, 101, "GR", "25", start, duration, "")
				if _, err := sqlcgen.New(pool).UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
					Site: testSite, ProgramID: 101, Action: "skip",
				}); err != nil {
					t.Fatalf("upserting intent: %v", err)
				}
			},
		},
		// 「チャンネルスナップショットが無い（00009 以前の残骸）」というケースは
		// issue #101（00026）で program_snapshots のチャンネル識別列が NOT NULL
		// 化されたことで削除した。この状態自体が表現不可能になったため
		// （INSERT が 23502 で落ちる。NOT NULL の回帰テストは
		// internal/db/models_test.go の
		// TestSchemaV1_ProgramSnapshotChannelIdentityNotNull にある）。
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			start := time.Now().Truncate(time.Hour).Add(24 * time.Hour)

			seedTuner(t, pool, 0, "PX-S1UD_T1", []string{"GR"}, true, false)
			seedReservation(t, pool, 100, "GR", "27", start, duration, "")
			tc.seed(t, pool, start)

			if overages := loadOverages(t, pool); len(overages) != 0 {
				t.Errorf("overages = %+v, want none (この予約は schedule を生まないので需要にならない)", overages)
			}
		})
	}
}

// intent action = 'record' は base.skip に勝つ（docs/recording.md §4.2）。
// 「録れ」意図がある予約は schedule が作られるので需要になる。
func TestLoad_IntentRecordBeatsBaseSkip(t *testing.T) {
	pool := testutil.SetupDB(t)
	start := time.Now().Truncate(time.Hour).Add(24 * time.Hour)

	seedTuner(t, pool, 0, "PX-S1UD_T1", []string{"GR"}, true, false)
	seedReservation(t, pool, 100, "GR", "27", start, time.Hour, "")
	seedReservation(t, pool, 101, "GR", "25", start, time.Hour, `{"skip":true}`)
	if _, err := sqlcgen.New(pool).UpsertProgramIntent(context.Background(), sqlcgen.UpsertProgramIntentParams{
		Site: testSite, ProgramID: 101, Action: "record",
	}); err != nil {
		t.Fatalf("upserting intent: %v", err)
	}

	if overages := loadOverages(t, pool); len(overages) != 1 {
		t.Errorf("overages = %+v, want 1 (action='record' は base.skip に勝つので需要になる)", overages)
	}
}

// tuner_sync の is_available / is_fault が cap に効くこと（DB からの読み出し経路）。
func TestLoad_TunerHealthFromProjection(t *testing.T) {
	tests := []struct {
		name             string
		available, fault bool
		wantOverages     int
	}{
		{name: "健全な 2 本目は数える", available: true, fault: false, wantOverages: 0},
		{name: "無効化された 2 本目は数えない", available: false, fault: false, wantOverages: 1},
		{name: "故障報告のある 2 本目は数えない", available: true, fault: true, wantOverages: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			start := time.Now().Truncate(time.Hour).Add(24 * time.Hour)

			seedTuner(t, pool, 0, "PX-S1UD_T1", []string{"GR"}, true, false)
			seedTuner(t, pool, 1, "PX-S1UD_T2", []string{"GR"}, tc.available, tc.fault)
			seedReservation(t, pool, 100, "GR", "27", start, time.Hour, "")
			seedReservation(t, pool, 101, "GR", "25", start, time.Hour, "")

			if got := loadOverages(t, pool); len(got) != tc.wantOverages {
				t.Errorf("overages = %+v, want %d", got, tc.wantOverages)
			}
		})
	}
}

// 射影が空なら何も主張しない（自分の無知を警告に変えない）。
func TestLoad_NoClaimWithoutTunerProjection(t *testing.T) {
	pool := testutil.SetupDB(t)
	start := time.Now().Truncate(time.Hour).Add(24 * time.Hour)

	seedReservation(t, pool, 100, "GR", "27", start, time.Hour, "")
	seedReservation(t, pool, 101, "GR", "25", start, time.Hour, "")

	if overages := loadOverages(t, pool); len(overages) != 0 {
		t.Errorf("overages = %+v, want none (tuner_sync が空なら判定しない)", overages)
	}

	// 反対方向: 射影が入れば判定する。
	seedTuner(t, pool, 0, "PX-S1UD_T1", []string{"GR"}, true, false)
	if overages := loadOverages(t, pool); len(overages) != 1 {
		t.Errorf("overages = %+v, want 1 after projecting a tuner", overages)
	}
}

// 種別部分集合への縮約が DB 経路でも効くこと。GR 専用 1 本 + GR/BS 両対応 1 本に
// 対する「GR 1 + BS 2」は総本数（2）が足りているのに {BS}: 2 ≤ 1 が破れる。
// tuner_sync.types（text[]）が正しく読めていないとこの判定は出ない。
func TestLoad_ReductionThroughProjection(t *testing.T) {
	pool := testutil.SetupDB(t)
	start := time.Now().Truncate(time.Hour).Add(24 * time.Hour)

	seedTuner(t, pool, 0, "PX-S1UD_T1", []string{"GR"}, true, false)
	seedTuner(t, pool, 1, "PX-W3U4_T1", []string{"GR", "BS"}, true, false)
	seedReservation(t, pool, 100, "GR", "27", start, time.Hour, "")
	seedReservation(t, pool, 101, "BS", "BS15_0", start, time.Hour, "")

	// GR 1 + BS 1 は収まる（T2→BS, T1→GR）。
	if overages := loadOverages(t, pool); len(overages) != 0 {
		t.Fatalf("overages = %+v, want none for GR 1 + BS 1", overages)
	}

	// BS を 1 つ足すと {BS} が破れる。
	seedReservation(t, pool, 102, "BS", "BS03_1", start, time.Hour, "")
	overages := loadOverages(t, pool)
	if len(overages) != 1 {
		t.Fatalf("overages = %+v, want 1", overages)
	}
	if overages[0].Shortfall != 1 {
		t.Errorf("shortfall = %d, want 1", overages[0].Shortfall)
	}
	if len(overages[0].JammedTypes) != 1 || overages[0].JammedTypes[0] != "BS" {
		t.Errorf("jammedTypes = %v, want [BS]", overages[0].JammedTypes)
	}
}

// 予約の終了時刻は program_start_at + program_duration_ms。
// SQL の式（::timestamptz キャスト）が正しく効いていることの確認。
func TestLoad_DemandWindowFromDuration(t *testing.T) {
	pool := testutil.SetupDB(t)
	start := time.Now().Truncate(time.Hour).Add(24 * time.Hour)

	seedTuner(t, pool, 0, "PX-S1UD_T1", []string{"GR"}, true, false)
	// 27ch は 60 分、25ch は 30 分遅れて 60 分。重なりは 30..60 分。
	seedReservation(t, pool, 100, "GR", "27", start, time.Hour, "")
	seedReservation(t, pool, 101, "GR", "25", start.Add(30*time.Minute), time.Hour, "")

	overages := loadOverages(t, pool)
	if len(overages) != 1 {
		t.Fatalf("overages = %+v, want 1", overages)
	}
	wantStart := start.Add(30 * time.Minute)
	wantEnd := start.Add(time.Hour)
	if !overages[0].StartAt.Equal(wantStart) || !overages[0].EndAt.Equal(wantEnd) {
		t.Errorf("interval = %v..%v, want %v..%v",
			overages[0].StartAt, overages[0].EndAt, wantStart, wantEnd)
	}
}
