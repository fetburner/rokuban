package dbcollector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	rokutest "github.com/fetburner/rokuban/internal/testutil"
)

const testSite = "default"

func newPresyncCollector(pool *pgxpool.Pool) *PresyncCollector {
	return NewPresyncCollector(pgstoretest.ScheduleSync(pool), testSite)
}

func TestLoopPassCollector_ReadsDBBackedMarkers(t *testing.T) {
	pool := rokutest.SetupDB(t)
	ctx := context.Background()
	markers := pgstoretest.Reservations(pool)
	if err := markers.MarkRulerPasses(ctx, []string{testSite}); err != nil {
		t.Fatalf("upserting ruler marker: %v", err)
	}
	if err := markers.MarkRecordSweep(ctx, testSite); err != nil {
		t.Fatalf("upserting record sweep marker: %v", err)
	}
	if err := markers.MarkReconciler(ctx, testSite); err != nil {
		t.Fatalf("upserting reconciler marker: %v", err)
	}

	// ruler と sweep と reconciler の値の取り違え（読む loop の入れ替え）を見るため、時刻を別々に固定する。
	rulerAt := time.Unix(1700001000, 0).UTC()
	sweepAt := time.Unix(1700002000, 0).UTC()
	reconcilerAt := time.Unix(1700003000, 0).UTC()
	for loop, at := range map[string]time.Time{"ruler": rulerAt, "record_sweep": sweepAt, "reconciler": reconcilerAt} {
		if _, err := pool.Exec(ctx, `UPDATE loop_success_markers SET last_success_at = $3 WHERE loop = $1 AND scope = $2`, loop, testSite, at); err != nil {
			t.Fatalf("setting %s marker time: %v", loop, err)
		}
	}

	c := NewLoopPassCollector(markers, testSite)
	if got := gaugeValue(t, c, "rokuban_ruler_last_success_timestamp_seconds"); got != float64(rulerAt.Unix()) {
		t.Errorf("ruler last success = %v, want %v", got, rulerAt.Unix())
	}
	if got := gaugeValue(t, c, "rokuban_sweep_last_success_timestamp_seconds"); got != float64(sweepAt.Unix()) {
		t.Errorf("sweep last success = %v, want %v", got, sweepAt.Unix())
	}
	if got := gaugeValue(t, c, "rokuban_reconcile_last_success_timestamp_seconds"); got != float64(reconcilerAt.Unix()) {
		t.Errorf("reconcile last success = %v, want %v", got, reconcilerAt.Unix())
	}
}

// 読み取りが失敗したら、reconciler の系列は出さず（0 にもせず）エラーカウンタだけを進める。
func TestLoopPassCollector_ReadFailureOmitsReconcileSeries(t *testing.T) {
	t.Parallel()
	fake := &fakeReconcileMarkers{err: map[string]error{store.LoopReconciler: errors.New("boom")}}
	c := NewLoopPassCollector(fake, testSite)
	got := collectReconcileSeries(t, c)

	if _, ok := got["rokuban_reconcile_last_success_timestamp_seconds{site=default}"]; ok {
		t.Error("reconcile last success series was emitted despite the read failure")
	}
	if v := got["rokuban_loop_pass_marker_scrape_errors_total{site=default}"]; v != 1 {
		t.Errorf("scrape errors = %v, want 1", v)
	}
}

// reconciler の系列は site ラベルで出る。値は loop_success_markers の scope = site の時刻。
func TestLoopPassCollector_ReconcileSeriesIsLabeledBySite(t *testing.T) {
	t.Parallel()
	at := time.Unix(1700004000, 0)
	fake := &fakeReconcileMarkers{loops: map[string]map[string]time.Time{
		store.LoopReconciler: {testSite: at, "other": time.Unix(1, 0)},
	}}
	c := NewLoopPassCollector(fake, testSite)
	got := collectReconcileSeries(t, c)

	if v := got["rokuban_reconcile_last_success_timestamp_seconds{site=default}"]; v != float64(at.Unix()) {
		t.Errorf("reconcile last success = %v, want %v", v, at.Unix())
	}
}

// marker がまだ無い site も「未確立 = 0」として観測できる。系列自体を消すと、
// 初回実行前と collector の登録漏れを区別できない。
func TestLoopPassCollector_ReportsZeroBeforeFirstPass(t *testing.T) {
	pool := rokutest.SetupDB(t)
	c := NewLoopPassCollector(pgstoretest.Reservations(pool), testSite)

	if got := gaugeValue(t, c, "rokuban_ruler_last_success_timestamp_seconds"); got != 0 {
		t.Errorf("ruler last success before first pass = %v, want 0", got)
	}
	if got := gaugeValue(t, c, "rokuban_sweep_last_success_timestamp_seconds"); got != 0 {
		t.Errorf("sweep last success before first pass = %v, want 0", got)
	}
	if got := gaugeValue(t, c, "rokuban_reconcile_last_success_timestamp_seconds"); got != 0 {
		t.Errorf("reconcile last success before first pass = %v, want 0", got)
	}
}

func seedFinishedRecord(t *testing.T, pool *pgxpool.Pool, recordID string, contentLength int64, ingested bool) {
	t.Helper()

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.EventID = int32(len(recordID)*1000 + int(recordID[len(recordID)-1]))
		fixtureParams.Title = "テスト番組 " + recordID
	})

	dbtest.RecordSync(t, pool, recordingID, recordID, func(p *dbtest.RecordSyncParams) {
		p.ProgramID = recordingID
		p.ContentLength = &contentLength
	})

	if ingested {
		dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
			fixtureParams.RelPath = "test/" + recordID + ".m2ts"
			fixtureParams.SizeBytes = contentLength
		})
	}
}

// 未 ingest の record だけが滞留として数えられること。
func TestBacklogCollector(t *testing.T) {
	pool := rokutest.SetupDB(t)

	// 未 ingest 2 件（合計 300）、ingest 済み 1 件
	seedFinishedRecord(t, pool, "rec-a", 100, false)
	seedFinishedRecord(t, pool, "rec-b", 200, false)
	seedFinishedRecord(t, pool, "rec-c", 999, true)

	c := NewBacklogCollector(pgstoretest.Recordings(pool), testSite)

	if got := gaugeValue(t, c, "rokuban_uningested_records"); got != 2 {
		t.Errorf("rokuban_uningested_records = %v, want 2", got)
	}
	if got := gaugeValue(t, c, "rokuban_uningested_record_bytes"); got != 300 {
		t.Errorf("rokuban_uningested_record_bytes = %v, want 300", got)
	}

	// ingest されると滞留から外れること
	seedIngestFor(t, pool, "rec-a")
	if got := gaugeValue(t, c, "rokuban_uningested_records"); got != 1 {
		t.Errorf("after ingest: rokuban_uningested_records = %v, want 1", got)
	}
	if got := gaugeValue(t, c, "rokuban_uningested_record_bytes"); got != 200 {
		t.Errorf("after ingest: rokuban_uningested_record_bytes = %v, want 200", got)
	}
}

// 滞留 0 のときも 0 として報告されること（メトリクスが消えない）。
func TestBacklogCollector_Empty(t *testing.T) {
	pool := rokutest.SetupDB(t)
	c := NewBacklogCollector(pgstoretest.Recordings(pool), testSite)

	if got := gaugeValue(t, c, "rokuban_uningested_records"); got != 0 {
		t.Errorf("rokuban_uningested_records = %v, want 0", got)
	}
}

// DB クエリが失敗したときは 0 を報告せず、専用のエラーカウンタを進めること。
// 0 を報告すると「滞留なし」と区別できず、滞留アラートを黙って無効化してしまう。
func TestBacklogCollector_QueryFailure(t *testing.T) {
	pool := rokutest.SetupDB(t)
	c := NewBacklogCollector(pgstoretest.Recordings(pool), testSite)
	pool.Close() // クエリを失敗させる

	text := gatherText(t, c)
	if strings.Contains(text, "rokuban_uningested_records ") {
		t.Error("クエリ失敗時に滞留メトリクスを報告してはいけない（0 と誤解される）")
	}
	if !strings.Contains(text, "rokuban_uningested_backlog_scrape_errors_total") {
		t.Error("エラーカウンタが報告されていない")
	}
}

// PresyncCollector は desired/observed の存在差分、実効 options の差分、
// 終了済み予約、skip、snapshot marker の未確立を区別する。特に marker が無い
// ときも pending の 0 と混同せず、PromQL 側で unobservable を最優先できる
// timestamp=0 を出す。
func TestPresyncCollector_ClassifiesPendingState(t *testing.T) {
	pool := rokutest.SetupDB(t)

	const programID int64 = 6800001
	startAt := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	seedPresyncReservation(t, pool, programID, startAt)
	c := newPresyncCollector(pool)

	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "missing"}); got != 1 {
		t.Errorf("without snapshot marker, missing = %v, want 1", got)
	}
	if got := labeledGaugeValue(t, c, "rokuban_schedule_snapshot_last_success_timestamp_seconds", nil); got != 0 {
		t.Errorf("without snapshot marker, snapshot timestamp = %v, want 0", got)
	}

	markScheduleSnapshot(t, pool)
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "missing"}); got != 1 {
		t.Errorf("missing = %v, want 1", got)
	}
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "options"}); got != 0 {
		t.Errorf("options = %v, want 0", got)
	}
	if got := labeledGaugeValue(t, c, "rokuban_schedule_snapshot_last_success_timestamp_seconds", nil); got <= 0 {
		t.Errorf("snapshot timestamp = %v, want positive", got)
	}

	seedObservedSchedule(t, pool, programID, mirakc.Options{Priority: 10}, []string{mirakc.ProgramTag(programID)})
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "missing"}); got != 0 {
		t.Errorf("synced missing = %v, want 0", got)
	}
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "options"}); got != 0 {
		t.Errorf("synced options = %v, want 0", got)
	}

	seedObservedSchedule(t, pool, programID, mirakc.Options{Priority: 11}, []string{mirakc.ProgramTag(programID)})
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "options"}); got != 1 {
		t.Errorf("priority mismatch options = %v, want 1", got)
	}

	// 番組時刻が過去へ変更されて終了済みになった予約は、schedule が無くても
	// presync 未同期として数えない。開始時刻変更の境界と ended filter を固定する。
	endedAt := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	dbtest.ProgramSnapshot(t, pool, programID, func(p *dbtest.ProgramSnapshotParams) {
		p.Title = "終了済みへ変更"
		p.StartAt = endedAt.Add(-time.Minute)
		p.DurationMs = 60_000
		p.NetworkID = 32678
		p.ServiceID = 5168
		p.ChannelType = "GR"
		p.Channel = "27"
		p.EventID = 68001
		p.ServiceName = "テストチャンネル"
	})
	if err := pgstoretest.Reservations(pool).DeleteOverrides(context.Background(), testSite, programID); err != nil {
		t.Fatalf("deleting overrides: %v", err)
	}
	markScheduleSnapshot(t, pool)
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "missing"}); got != 0 {
		t.Errorf("ended reservation missing = %v, want 0", got)
	}
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "options"}); got != 0 {
		t.Errorf("ended reservation options = %v, want 0", got)
	}

	const skippedProgramID int64 = 6800004
	seedPresyncReservation(t, pool, skippedProgramID, time.Now().Add(time.Hour))
	dbtest.ProgramIntent(t, pool, skippedProgramID, func(p *dbtest.ProgramIntentParams) {
		p.Action = "skip"
	})
	markScheduleSnapshot(t, pool)
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "missing"}); got != 0 {
		t.Errorf("skipped reservation missing = %v, want 0", got)
	}
}

func TestPresyncCollector_ContinuesAfterInvalidReservationOptions(t *testing.T) {
	pool := rokutest.SetupDB(t)

	const (
		scheduledProgramID int64 = 6800121
		missingProgramID   int64 = 6800122
	)
	scheduledStartAt := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	missingStartAt := time.Now().Add(2 * time.Hour).Truncate(time.Millisecond)
	seedPresyncReservation(t, pool, scheduledProgramID, scheduledStartAt)
	seedPresyncReservation(t, pool, missingProgramID, missingStartAt)
	seedInvalidPresyncOptions(t, pool, missingProgramID)
	seedObservedSchedule(t, pool, scheduledProgramID, mirakc.Options{Priority: 10}, []string{mirakc.ProgramTag(scheduledProgramID)})
	markScheduleSnapshot(t, pool)

	c := newPresyncCollector(pool)
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "missing"}); got != 1 {
		t.Errorf("missing = %v, want 1", got)
	}
	if got := labeledGaugeValue(t, c, "rokuban_schedule_snapshot_last_success_timestamp_seconds", nil); got <= 0 {
		t.Errorf("snapshot timestamp = %v, want a successful snapshot", got)
	}
	earliest := labeledGaugeValue(t, c, "rokuban_presync_pending_earliest_start_timestamp_seconds", map[string]string{"reason": "missing"})
	earliestAt := time.Unix(0, int64(earliest*float64(time.Second)))
	if delta := earliestAt.Sub(missingStartAt); delta < -time.Millisecond || delta > time.Millisecond {
		t.Errorf("missing earliest start = %v, want %v", earliestAt, missingStartAt)
	}
}

func TestPresyncCollector_InvalidOptionsWithObservedScheduleAreNotMissing(t *testing.T) {
	pool := rokutest.SetupDB(t)

	const programID int64 = 6800125
	seedPresyncReservation(t, pool, programID, time.Now().Add(time.Hour))
	seedInvalidPresyncOptions(t, pool, programID)
	seedObservedSchedule(t, pool, programID, mirakc.Options{Priority: 10}, []string{mirakc.ProgramTag(programID)})
	markScheduleSnapshot(t, pool)

	c := newPresyncCollector(pool)
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "missing"}); got != 0 {
		t.Errorf("observed invalid-options reservation missing = %v, want 0", got)
	}
	if _, ok := labeledGaugeValueOk(t, c, "rokuban_presync_pending_earliest_start_timestamp_seconds", map[string]string{"reason": "missing"}); ok {
		t.Error("missing earliest series must not be reported for an observed invalid-options reservation")
	}
}

func TestPresyncCollector_EndedInvalidOptionsWithoutScheduleAreNotMissing(t *testing.T) {
	pool := rokutest.SetupDB(t)

	const programID int64 = 6800126
	seedPresyncReservation(t, pool, programID, time.Now().Add(-2*time.Hour))
	seedInvalidPresyncOptions(t, pool, programID)
	markScheduleSnapshot(t, pool)

	c := newPresyncCollector(pool)
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "missing"}); got != 0 {
		t.Errorf("ended invalid-options reservation missing = %v, want 0", got)
	}
	if _, ok := labeledGaugeValueOk(t, c, "rokuban_presync_pending_earliest_start_timestamp_seconds", map[string]string{"reason": "missing"}); ok {
		t.Error("missing earliest series must not be reported for an ended invalid-options reservation")
	}
}

// presync_pending_earliest_start_timestamp_seconds は「開始が近い」を PromQL 側で
// 判定するための最小 start_at。件数 gauge だけでは 8 日先の 1 件と 2 分後開始の
// 1 件が同値になり、区別できない（issue #680）。
func TestPresyncCollector_EarliestStart(t *testing.T) {
	pool := rokutest.SetupDB(t)

	const farProgramID int64 = 6800101
	farStart := time.Now().Add(8 * 24 * time.Hour).Truncate(time.Millisecond)
	seedPresyncReservation(t, pool, farProgramID, farStart)
	markScheduleSnapshot(t, pool)
	c := newPresyncCollector(pool)

	// ① 十分先の予約 1 件だけ: missing=1 かつ earliest - now > 7 日。
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "missing"}); got != 1 {
		t.Fatalf("missing = %v, want 1", got)
	}
	earliest := labeledGaugeValue(t, c, "rokuban_presync_pending_earliest_start_timestamp_seconds", map[string]string{"reason": "missing"})
	if got := time.Until(time.Unix(0, int64(earliest*float64(time.Second)))); got <= 7*24*time.Hour {
		t.Errorf("earliest - now = %v, want > 7 days", got)
	}

	// ② 2 分後開始の予約を追加すると、その方が最小 start_at になる。
	const nearProgramID int64 = 6800102
	nearStart := time.Now().Add(2 * time.Minute).Truncate(time.Millisecond)
	seedPresyncReservation(t, pool, nearProgramID, nearStart)
	markScheduleSnapshot(t, pool)
	earliest = labeledGaugeValue(t, c, "rokuban_presync_pending_earliest_start_timestamp_seconds", map[string]string{"reason": "missing"})
	if got := time.Until(time.Unix(0, int64(earliest*float64(time.Second)))); got >= 5*time.Minute {
		t.Errorf("earliest - now = %v, want < 5 minutes", got)
	}

	// ③ pending が 0 の reason（options）には earliest 系列が出ない。
	if _, ok := labeledGaugeValueOk(t, c, "rokuban_presync_pending_earliest_start_timestamp_seconds", map[string]string{"reason": "options"}); ok {
		t.Error("earliest series must not be reported for a reason with zero pending")
	}
}

func TestPresyncCollector_StaleSnapshotRemainsObservable(t *testing.T) {
	pool := rokutest.SetupDB(t)
	ctx := context.Background()
	markScheduleSnapshot(t, pool)

	var staleAt time.Time
	if err := pool.QueryRow(ctx, `
		UPDATE schedule_sync_snapshots
		SET snapshot_at = now() - interval '1 hour'
		WHERE site = $1
		RETURNING snapshot_at`, testSite).Scan(&staleAt); err != nil {
		t.Fatalf("aging schedule snapshot: %v", err)
	}

	c := newPresyncCollector(pool)
	got := time.Unix(0, int64(labeledGaugeValue(t, c, "rokuban_schedule_snapshot_last_success_timestamp_seconds", nil)*float64(time.Second)))
	if !got.Before(time.Now().Add(-30 * time.Minute)) {
		t.Errorf("snapshot timestamp = %v, want a stale timestamp", got)
	}
	if !got.Equal(staleAt) {
		t.Logf("collector timestamp = %v, database timestamp = %v (precision conversion is expected)", got, staleAt)
	}
}

func TestPresyncCollector_ExplicitContentPathMismatch(t *testing.T) {
	pool := rokutest.SetupDB(t)

	const programID int64 = 6800002
	seedPresyncReservation(t, pool, programID, time.Now().Add(time.Hour))
	contentPath, err := json.Marshal(map[string]string{"contentPath": "custom/program.m2ts"})
	if err != nil {
		t.Fatalf("marshalling content path override: %v", err)
	}
	dbtest.ProgramOverrides(t, pool, programID, func(p *dbtest.ProgramOverridesParams) {
		p.Overrides = contentPath
	})
	seedObservedSchedule(t, pool, programID, mirakc.Options{
		Priority:    10,
		ContentPath: stringPtr("other/program.m2ts"),
	}, []string{mirakc.ProgramTag(programID)})
	markScheduleSnapshot(t, pool)

	c := newPresyncCollector(pool)
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "options"}); got != 1 {
		t.Errorf("content path mismatch options = %v, want 1", got)
	}
}

func TestPresyncCollector_ReMaterializationReevaluatesCurrentOptions(t *testing.T) {
	pool := rokutest.SetupDB(t)

	const programID int64 = 6800005
	seedPresyncReservation(t, pool, programID, time.Now().Add(time.Hour))
	seedObservedSchedule(t, pool, programID, mirakc.Options{Priority: 10}, []string{mirakc.ProgramTag(programID)})
	markScheduleSnapshot(t, pool)
	c := newPresyncCollector(pool)
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "options"}); got != 0 {
		t.Fatalf("initial options mismatch = %v, want 0", got)
	}

	if _, err := pool.Exec(context.Background(), `DELETE FROM reservations WHERE site = $1 AND program_id = $2`, testSite, programID); err != nil {
		t.Fatalf("deleting reservation: %v", err)
	}
	dbtest.ManualReservation(t, pool, programID)
	overrides, err := json.Marshal(map[string]int{"priority": 11})
	if err != nil {
		t.Fatalf("marshalling priority override: %v", err)
	}
	dbtest.ProgramOverrides(t, pool, programID, func(p *dbtest.ProgramOverridesParams) {
		p.Overrides = overrides
	})

	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "options"}); got != 1 {
		t.Errorf("re-materialized options mismatch = %v, want 1", got)
	}
}

// state=scheduled 以外の options 不一致は、reconciler が今すぐ再作成できない
// ため options_deferred に分離する。番組終了後は未同期の母集団から消える。
func TestPresyncCollector_DeferredOptionsDisappearAfterProgramEnds(t *testing.T) {
	pool := rokutest.SetupDB(t)

	const programID int64 = 6800006
	startAt := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	seedPresyncReservation(t, pool, programID, startAt)
	seedObservedScheduleWithState(t, pool, programID, mirakc.ScheduleStateRecording, mirakc.Options{
		Priority: 11,
	}, []string{mirakc.ProgramTag(programID)})
	markScheduleSnapshot(t, pool)

	c := newPresyncCollector(pool)
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": reasonOptions}); got != 0 {
		t.Errorf("scheduled options = %v, want 0", got)
	}
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": reasonOptionsDeferred}); got != 1 {
		t.Errorf("deferred options = %v, want 1", got)
	}

	endedAt := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	dbtest.ProgramSnapshot(t, pool, programID, func(p *dbtest.ProgramSnapshotParams) {
		p.Title = "終了済みへ変更"
		p.StartAt = endedAt
		p.DurationMs = 30 * time.Minute.Milliseconds()
		p.NetworkID = 32678
		p.ServiceID = 5168
		p.ChannelType = "GR"
		p.Channel = "27"
		p.EventID = 68006
		p.ServiceName = "テストチャンネル"
	})
	markScheduleSnapshot(t, pool)

	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": reasonOptions}); got != 0 {
		t.Errorf("ended options = %v, want 0", got)
	}
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": reasonOptionsDeferred}); got != 0 {
		t.Errorf("ended deferred options = %v, want 0", got)
	}
	if _, ok := labeledGaugeValueOk(t, c, "rokuban_presync_pending_earliest_start_timestamp_seconds", map[string]string{"reason": reasonOptionsDeferred}); ok {
		t.Error("ended deferred options must not report an earliest series")
	}
}

// DB query が失敗したときは pending / snapshot を 0 として出さず、専用の
// エラーカウンタだけを出す。既存 BacklogCollector と同じ沈黙しない契約。
func TestPresyncCollector_QueryFailure(t *testing.T) {
	pool := rokutest.SetupDB(t)
	c := newPresyncCollector(pool)
	pool.Close()

	text := gatherText(t, c)
	if strings.Contains(text, "rokuban_presync_pending") {
		t.Error("query failure must not report presync_pending as zero")
	}
	if strings.Contains(text, "rokuban_schedule_snapshot_last_success_timestamp_seconds") {
		t.Error("query failure must not report snapshot timestamp")
	}
	if !strings.Contains(text, "rokuban_presync_scrape_errors_total") {
		t.Error("presync scrape error counter was not reported")
	}
}

// seedInvalidPresyncOptions は options として解釈できない上書きを作る。番組の snapshot を先に作っておくこと。
func seedInvalidPresyncOptions(t *testing.T, pool *pgxpool.Pool, programID int64) {
	t.Helper()
	dbtest.ProgramOverrides(t, pool, programID, func(p *dbtest.ProgramOverridesParams) {
		p.Overrides = json.RawMessage(`{"priority":"high"}`)
	})
}

func seedPresyncReservation(t *testing.T, pool *pgxpool.Pool, programID int64, startAt time.Time) {
	t.Helper()
	dbtest.ProgramSnapshot(t, pool, programID, func(p *dbtest.ProgramSnapshotParams) {
		p.Title = "presync test"
		p.StartAt = startAt
		p.DurationMs = 30 * time.Minute.Milliseconds()
		p.NetworkID = 32678
		p.ServiceID = 5168
		p.ChannelType = "GR"
		p.Channel = "27"
		p.EventID = int32(programID % 100000)
		p.ServiceName = "テストチャンネル"
	})
	dbtest.ManualReservation(t, pool, programID)
}

func seedObservedSchedule(t *testing.T, pool *pgxpool.Pool, programID int64, options mirakc.Options, tags []string) {
	seedObservedScheduleWithState(t, pool, programID, mirakc.ScheduleStateScheduled, options, tags)
}

func seedObservedScheduleWithState(t *testing.T, pool *pgxpool.Pool, programID int64, state string, options mirakc.Options, tags []string) {
	t.Helper()
	optionsJSON, err := json.Marshal(options)
	if err != nil {
		t.Fatalf("marshalling observed options: %v", err)
	}
	dbtest.ObservedSchedule(t, pool, programID, func(p *dbtest.ObservedScheduleParams) {
		p.State = state
		p.Options = optionsJSON
		p.Tags = tags
	})
}

// markScheduleSnapshot は snapshot marker を現在時刻へ進める。marker は ReplaceObserved が
// 観測行と同じ tx で書くので、今ある観測行を読み直して同じ内容で置き換える。観測は変わらず、marker だけが進む。
func markScheduleSnapshot(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	ss := pgstoretest.ScheduleSync(pool)
	observed, err := ss.ListObserved(ctx, testSite)
	if err != nil {
		t.Fatalf("listing observed schedules: %v", err)
	}
	if err := ss.ReplaceObserved(ctx, testSite, observed); err != nil {
		t.Fatalf("marking schedule snapshot: %v", err)
	}
}

// seedIngestFor は既存の record_sync に対応する録画に原本アセットを追加する。
func seedIngestFor(t *testing.T, pool *pgxpool.Pool, recordID string) {
	t.Helper()
	target, err := pgstoretest.Recordings(pool).GetRecordSyncIngestTarget(context.Background(), testSite, recordID)
	if err != nil {
		t.Fatalf("looking up record_sync: %v", err)
	}
	if target.RecordingID == nil {
		t.Fatalf("record_sync %q has no recording_id", recordID)
	}
	dbtest.MediaAsset(t, pool, *target.RecordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.RelPath = "test/" + recordID + ".m2ts"
	})
}

// gaugeValue は Collect の結果から指定名のゲージ値を取り出す。
func gaugeValue(t *testing.T, c prometheus.Collector, name string) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 16)
	c.Collect(ch)
	close(ch)

	for m := range ch {
		if !strings.Contains(m.Desc().String(), `"`+name+`"`) {
			continue
		}
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("writing metric %q: %v", name, err)
		}
		if pb.Gauge == nil {
			t.Fatalf("metric %q is not a gauge", name)
		}
		return pb.Gauge.GetValue()
	}
	t.Fatalf("metric %q was not collected", name)
	return 0
}

func labeledGaugeValue(t *testing.T, c prometheus.Collector, name string, labels map[string]string) float64 {
	t.Helper()
	got, ok := labeledGaugeValueOk(t, c, name, labels)
	if !ok {
		t.Fatalf("metric %q with labels %v was not collected", name, labels)
	}
	return got
}

// labeledGaugeValueOk は labeledGaugeValue と同じ照合を行うが、系列が見つからない
// ことをテスト対象にできるよう Fatal せず (0, false) を返す。
func labeledGaugeValueOk(t *testing.T, c prometheus.Collector, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	ch := make(chan prometheus.Metric, 16)
	c.Collect(ch)
	close(ch)

	for m := range ch {
		if !strings.Contains(m.Desc().String(), `"`+name+`"`) {
			continue
		}
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("writing metric %q: %v", name, err)
		}
		if pb.Gauge == nil {
			t.Fatalf("metric %q is not a gauge", name)
		}
		gotLabels := make(map[string]string, len(pb.Label))
		for _, label := range pb.Label {
			gotLabels[label.GetName()] = label.GetValue()
		}
		matches := true
		for key, want := range labels {
			if gotLabels[key] != want {
				matches = false
				break
			}
		}
		if matches {
			return pb.Gauge.GetValue(), true
		}
	}
	return 0, false
}

func stringPtr(v string) *string { return &v }

// gatherText は Collect の結果を Prometheus の text format にして返す。
func gatherText(t *testing.T, c prometheus.Collector) string {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	families, err := reg.Gather()
	if err != nil {
		// コレクタがメトリクスを出さなくても Gather は成功する。
		// エラーは記録するだけで、出たぶんは検査に使う。
		t.Logf("Gather: %v", err)
	}
	var sb strings.Builder
	enc := expfmt.NewEncoder(&sb, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, f := range families {
		if err := enc.Encode(f); err != nil {
			t.Fatalf("encoding %s: %v", f.GetName(), err)
		}
	}
	return sb.String()
}

// 転送待ちの 2 本のゲージは、保存した件数を scrape のたびに DB から読む。止まっているかは今の上限から導出し、
// 上限の無い置き場と、まだ数えていない置き場は系列を出さない。
func TestTransferBacklogCollector_ReadsSavedCounts(t *testing.T) {
	pool := rokutest.SetupDB(t)
	media := pgstoretest.Media(pool)
	if _, err := media.RecordTransferBacklog(context.Background(), map[string]int64{"edge": 3, "other": 9}, time.Unix(1700000000, 0)); err != nil {
		t.Fatalf("recording backlog: %v", err)
	}
	cases := []struct {
		name        string
		limit       int
		wantStopped float64
	}{
		{name: "count reached the limit", limit: 3, wantStopped: 1},
		{name: "count below the limit", limit: 4, wantStopped: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewTransferBacklogCollector(media, map[string]int{"edge": tc.limit, "uncounted": 1})
			if got := labeledGaugeValue(t, c, "rokuban_storage_transfer_backlog_assets", map[string]string{"store": "edge"}); got != 3 {
				t.Errorf("backlog = %v, want 3", got)
			}
			if got := labeledGaugeValue(t, c, "rokuban_storage_transfer_backlog_counted_timestamp_seconds", map[string]string{"store": "edge"}); got != 1700000000 {
				t.Errorf("counted timestamp = %v, want 1700000000", got)
			}
			if got := labeledGaugeValue(t, c, "rokuban_encode_backpressure", map[string]string{"store": "edge"}); got != tc.wantStopped {
				t.Errorf("backpressure = %v, want %v", got, tc.wantStopped)
			}
			for _, absent := range []string{"other", "uncounted"} {
				if _, ok := labeledGaugeValueOk(t, c, "rokuban_storage_transfer_backlog_assets", map[string]string{"store": absent}); ok {
					t.Errorf("store %q has a series, want none", absent)
				}
			}
		})
	}
}
