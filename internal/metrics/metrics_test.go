package metrics

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
	rokutest "github.com/fetburner/rokuban/internal/testutil"
)

const testSite = "default"

// M1-9 が最低限として挙げているメトリクスが registry に載っていること。
// 名前は運用側（scrape 設定・アラートルール）が依存するので固定する。
func TestNewRegistry_ExposesRequiredMetrics(t *testing.T) {
	reg := NewRegistry(nil)

	// 値が入らないと exposition に現れないメトリクスがあるので、
	// 全部に 1 度だけ書き込んでから確認する。
	IngestBytes.Add(1)
	IngestDuration.Observe(1)
	IngestJobs.WithLabelValues("success").Inc()
	IngestDroppedPackets.Add(1)
	IngestErrorPackets.Add(1)
	IngestScrambledPackets.Add(1)
	EncodeDuration.Observe(1)
	EncodeJobs.WithLabelValues("success").Inc()
	ReconcilePendingDiff.WithLabelValues("create").Set(0)
	ReconcilePendingDiff.WithLabelValues("update").Set(0)
	ReconcileSchedules.WithLabelValues("created").Inc()
	ReconcileSchedules.WithLabelValues("recreated").Inc()
	ReconcileCircuitBreakerTrips.Add(1)
	ReconcileScheduleLost.Add(1)
	ReconcileLastPass.SetToCurrentTime()
	ReconcileStartDelayed.WithLabelValues(testSite).Set(0)
	RulerProgramIDReuses.Inc()
	RecordingsFailed.WithLabelValues("need-rescheduling").Inc()
	RecordsBroken.WithLabelValues("io-error").Inc()
	SweepLastPass.SetToCurrentTime()
	EpgSyncDuration.Observe(1)
	EpgProgramsProjected.Set(1)
	EpgChannelsWithoutPrograms.Set(0)
	EpgSyncLastSuccess.SetToCurrentTime()
	TunersProjected.WithLabelValues(testSite).Set(2)
	TunerSyncLastSuccess.WithLabelValues(testSite).SetToCurrentTime()
	CapacityOverages.WithLabelValues(testSite).Set(0)
	LiveActiveSessions.Set(0)
	LiveSessionStartFailures.WithLabelValues("session_limit").Inc()
	LiveSessionEvictions.WithLabelValues("upstream", "retry_succeeded").Inc()
	LiveIdleGCReclaimed.Add(1)
	LiveLeaveHints.WithLabelValues("deadline_shortened").Inc()
	LiveIdleGCLastPass.SetToCurrentTime()
	EncodeReconcileLastPass.SetToCurrentTime()
	EncodeReconcileCandidates.Set(0)
	EncodeReconcileUnsatisfiable.WithLabelValues("h264").Set(0)
	ThumbnailReconcileLastPass.SetToCurrentTime()
	ThumbnailReconcileCandidates.Set(0)
	MediaAssetsMissing.WithLabelValues("original").Set(0)
	MissingAssetScanSuspectedStorageFailure.Add(1)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := make(map[string]bool, len(families))
	for _, f := range families {
		got[f.GetName()] = true
	}

	// M1-9 の「最低限」: reconcile 差分数 / ingest バイト・所要 /
	// ドロップ・scrambled カウンタ / recording.failed 理由別
	// （未 ingest record 総量は BacklogCollector 側でテストする）
	required := []string{
		"rokuban_ingest_bytes_total",
		"rokuban_ingest_duration_seconds",
		"rokuban_ingest_jobs_total",
		"rokuban_ingest_dropped_packets_total",
		"rokuban_ingest_error_packets_total",
		"rokuban_ingest_scrambled_packets_total",
		"rokuban_encode_duration_seconds",
		"rokuban_encode_jobs_total",
		"rokuban_reconcile_pending_diff",
		"rokuban_reconcile_schedules_total",
		"rokuban_reconcile_circuit_breaker_trips_total",
		"rokuban_reconcile_schedule_lost_total",
		"rokuban_reconcile_last_pass_timestamp_seconds",
		"rokuban_reconcile_start_delayed",
		"rokuban_recordings_failed_total",
		"rokuban_records_broken_total",
		"rokuban_sweep_last_pass_timestamp_seconds",
		"rokuban_epg_sync_duration_seconds",
		"rokuban_epg_programs_projected",
		"rokuban_epg_channels_without_programs",
		"rokuban_epg_sync_last_success_timestamp_seconds",
		// program_id 再利用検出（ruler）
		"rokuban_ruler_program_id_reuse_total",
		// M2-10: チューナー射影と容量超過
		"rokuban_tuners_projected",
		"rokuban_tuner_sync_last_success_timestamp_seconds",
		"rokuban_capacity_overages",
		// issue #91: ライブ視聴
		"rokuban_live_active_sessions",
		"rokuban_live_session_start_failures_total",
		"rokuban_live_session_evictions_total",
		"rokuban_live_idle_gc_reclaimed_total",
		"rokuban_live_idle_gc_last_pass_timestamp_seconds",
		// issue #191: 離脱ヒント（idle GC 回収数と対で読む）
		"rokuban_live_leave_hints_total",
		// issue #163: encode の desired−observed 定期パス。バックストップ自身が
		// 黙って止まる / 窓に張り付く / 設定から消えたプロファイルで落とす、の
		// 3 通りの黙り方に対応する（internal/worker/encode_reconcile.go）。
		"rokuban_encode_reconcile_last_pass_timestamp_seconds",
		"rokuban_encode_reconcile_candidates",
		"rokuban_encode_reconcile_unsatisfiable",
		"rokuban_thumbnail_reconcile_last_pass_timestamp_seconds",
		"rokuban_thumbnail_reconcile_candidates",
		// issue #343: active な media_asset の実体無し検出。
		// docs/operations/monitoring.md がこの 2 本を対で読む運用を約束して
		// いるので、片方の登録漏れが黙って通らないようにここに載せる
		// （ゲージが止まる条件はカウンタ側でしか分からない）。
		"rokuban_media_assets_missing",
		"rokuban_missing_asset_scan_suspected_storage_failure_total",
	}
	for _, name := range required {
		if !got[name] {
			t.Errorf("metric %q is not registered", name)
		}
	}

	// プロセスの状態も見られること（Go / process コレクタ）
	for _, name := range []string{"go_goroutines", "process_open_fds"} {
		if !got[name] {
			t.Errorf("runtime metric %q is not registered", name)
		}
	}
}

// 同じコレクタを 2 つの registry に登録できること
// （複数回 NewRegistry を呼んでも panic しない）。
func TestNewRegistry_Twice(t *testing.T) {
	_ = NewRegistry(nil)
	_ = NewRegistry(nil)
}

func seedFinishedRecord(t *testing.T, pool *pgxpool.Pool, recordID string, contentLength int64, ingested bool) {
	t.Helper()
	ctx := context.Background()
	q := sqlcgen.New(pool)

	recordingID, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              testSite,
		NetworkID:         32678,
		ServiceID:         5168,
		EventID:           int32(len(recordID)*1000 + int(recordID[len(recordID)-1])),
		ServiceName:       "テストチャンネル",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "テスト番組 " + recordID,
		ProgramStartAt:    time.Now().Truncate(time.Second),
		ProgramDurationMs: 1800000,
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("creating recording: %v", err)
	}

	if err := q.UpsertRecordSync(ctx, sqlcgen.UpsertRecordSyncParams{
		Site:          testSite,
		RecordID:      recordID,
		RecordingID:   &recordingID,
		ProgramID:     recordingID,
		Status:        "finished",
		ContentLength: &contentLength,
		Tags:          []string{},
	}); err != nil {
		t.Fatalf("upserting record_sync: %v", err)
	}

	if ingested {
		if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
			RecordingID: recordingID,
			Kind:        db.AssetKindOriginal,
			RelPath:     "test/" + recordID + ".m2ts",
			SizeBytes:   contentLength,
		}); err != nil {
			t.Fatalf("creating media_asset: %v", err)
		}
	}
}

// 未 ingest の record だけが滞留として数えられること。
func TestBacklogCollector(t *testing.T) {
	pool := rokutest.SetupDB(t)

	// 未 ingest 2 件（合計 300）、ingest 済み 1 件
	seedFinishedRecord(t, pool, "rec-a", 100, false)
	seedFinishedRecord(t, pool, "rec-b", 200, false)
	seedFinishedRecord(t, pool, "rec-c", 999, true)

	c := NewBacklogCollector(pool, testSite)

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
	c := NewBacklogCollector(pool, testSite)

	if got := gaugeValue(t, c, "rokuban_uningested_records"); got != 0 {
		t.Errorf("rokuban_uningested_records = %v, want 0", got)
	}
}

// DB クエリが失敗したときは 0 を報告せず、専用のエラーカウンタを進めること。
// 0 を報告すると「滞留なし」と区別できず、滞留アラートを黙って無効化してしまう。
func TestBacklogCollector_QueryFailure(t *testing.T) {
	pool := rokutest.SetupDB(t)
	c := NewBacklogCollector(pool, testSite)
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
	ctx := context.Background()
	q := sqlcgen.New(pool)

	const programID int64 = 6800001
	startAt := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	seedPresyncReservation(t, pool, programID, startAt)
	c := NewPresyncCollector(pool, testSite)

	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "missing"}); got != 1 {
		t.Errorf("without snapshot marker, missing = %v, want 1", got)
	}
	if got := labeledGaugeValue(t, c, "rokuban_schedule_snapshot_last_success_timestamp_seconds", nil); got != 0 {
		t.Errorf("without snapshot marker, snapshot timestamp = %v, want 0", got)
	}

	if err := q.UpsertScheduleSyncSnapshot(ctx, testSite); err != nil {
		t.Fatalf("marking schedule snapshot: %v", err)
	}
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
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site:        testSite,
		ProgramID:   programID,
		Title:       "終了済みへ変更",
		StartAt:     endedAt.Add(-time.Minute),
		DurationMs:  60_000,
		NetworkID:   32678,
		ServiceID:   5168,
		ChannelType: "GR",
		Channel:     "27",
		EventID:     68001,
		ServiceName: "テストチャンネル",
	}); err != nil {
		t.Fatalf("updating program snapshot time: %v", err)
	}
	if _, err := q.DeleteProgramOverrides(ctx, sqlcgen.DeleteProgramOverridesParams{Site: testSite, ProgramID: programID}); err != nil {
		t.Fatalf("deleting overrides: %v", err)
	}
	if err := q.UpsertScheduleSyncSnapshot(ctx, testSite); err != nil {
		t.Fatalf("refreshing schedule snapshot: %v", err)
	}
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "missing"}); got != 0 {
		t.Errorf("ended reservation missing = %v, want 0", got)
	}
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "options"}); got != 0 {
		t.Errorf("ended reservation options = %v, want 0", got)
	}

	const skippedProgramID int64 = 6800004
	seedPresyncReservation(t, pool, skippedProgramID, time.Now().Add(time.Hour))
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{Site: testSite, ProgramID: skippedProgramID}); err != nil {
		t.Fatalf("skipping reservation: %v", err)
	}
	if err := q.UpsertScheduleSyncSnapshot(ctx, testSite); err != nil {
		t.Fatalf("refreshing schedule snapshot after skip: %v", err)
	}
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "missing"}); got != 0 {
		t.Errorf("skipped reservation missing = %v, want 0", got)
	}
}

// presync_pending_earliest_start_timestamp_seconds は「開始が近い」を PromQL 側で
// 判定するための最小 start_at。件数 gauge だけでは 8 日先の 1 件と 2 分後開始の
// 1 件が同値になり、区別できない（issue #680）。
func TestPresyncCollector_EarliestStart(t *testing.T) {
	pool := rokutest.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	const farProgramID int64 = 6800101
	farStart := time.Now().Add(8 * 24 * time.Hour).Truncate(time.Millisecond)
	seedPresyncReservation(t, pool, farProgramID, farStart)
	if err := q.UpsertScheduleSyncSnapshot(ctx, testSite); err != nil {
		t.Fatalf("marking schedule snapshot: %v", err)
	}
	c := NewPresyncCollector(pool, testSite)

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
	if err := q.UpsertScheduleSyncSnapshot(ctx, testSite); err != nil {
		t.Fatalf("refreshing schedule snapshot: %v", err)
	}
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
	q := sqlcgen.New(pool)
	if err := q.UpsertScheduleSyncSnapshot(ctx, testSite); err != nil {
		t.Fatalf("marking schedule snapshot: %v", err)
	}

	var staleAt time.Time
	if err := pool.QueryRow(ctx, `
		UPDATE schedule_sync_snapshots
		SET snapshot_at = now() - interval '1 hour'
		WHERE site = $1
		RETURNING snapshot_at`, testSite).Scan(&staleAt); err != nil {
		t.Fatalf("aging schedule snapshot: %v", err)
	}

	c := NewPresyncCollector(pool, testSite)
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
	ctx := context.Background()
	q := sqlcgen.New(pool)

	const programID int64 = 6800002
	seedPresyncReservation(t, pool, programID, time.Now().Add(time.Hour))
	contentPath, err := json.Marshal(map[string]string{"contentPath": "custom/program.m2ts"})
	if err != nil {
		t.Fatalf("marshalling content path override: %v", err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: testSite, ProgramID: programID, Overrides: contentPath,
	}); err != nil {
		t.Fatalf("upserting content path override: %v", err)
	}
	seedObservedSchedule(t, pool, programID, mirakc.Options{
		Priority:    10,
		ContentPath: stringPtr("other/program.m2ts"),
	}, []string{mirakc.ProgramTag(programID)})
	if err := q.UpsertScheduleSyncSnapshot(ctx, testSite); err != nil {
		t.Fatalf("marking schedule snapshot: %v", err)
	}

	c := NewPresyncCollector(pool, testSite)
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "options"}); got != 1 {
		t.Errorf("content path mismatch options = %v, want 1", got)
	}
}

func TestPresyncCollector_ReMaterializationReevaluatesCurrentOptions(t *testing.T) {
	pool := rokutest.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	const programID int64 = 6800005
	seedPresyncReservation(t, pool, programID, time.Now().Add(time.Hour))
	seedObservedSchedule(t, pool, programID, mirakc.Options{Priority: 10}, []string{mirakc.ProgramTag(programID)})
	if err := q.UpsertScheduleSyncSnapshot(ctx, testSite); err != nil {
		t.Fatalf("marking schedule snapshot: %v", err)
	}
	c := NewPresyncCollector(pool, testSite)
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "options"}); got != 0 {
		t.Fatalf("initial options mismatch = %v, want 0", got)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM reservations WHERE site = $1 AND program_id = $2`, testSite, programID); err != nil {
		t.Fatalf("deleting reservation: %v", err)
	}
	if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{Site: testSite, ProgramID: programID}); err != nil {
		t.Fatalf("recreating reservation: %v", err)
	}
	overrides, err := json.Marshal(map[string]int{"priority": 11})
	if err != nil {
		t.Fatalf("marshalling priority override: %v", err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: testSite, ProgramID: programID, Overrides: overrides,
	}); err != nil {
		t.Fatalf("upserting priority override: %v", err)
	}

	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": "options"}); got != 1 {
		t.Errorf("re-materialized options mismatch = %v, want 1", got)
	}
}

// state=scheduled 以外の options 不一致は、reconciler が今すぐ再作成できない
// ため options_deferred に分離する。番組終了後は未同期の母集団から消える。
func TestPresyncCollector_DeferredOptionsDisappearAfterProgramEnds(t *testing.T) {
	pool := rokutest.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	const programID int64 = 6800006
	startAt := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	seedPresyncReservation(t, pool, programID, startAt)
	seedObservedScheduleWithState(t, pool, programID, mirakc.ScheduleStateRecording, mirakc.Options{
		Priority: 11,
	}, []string{mirakc.ProgramTag(programID)})
	if err := q.UpsertScheduleSyncSnapshot(ctx, testSite); err != nil {
		t.Fatalf("marking schedule snapshot: %v", err)
	}

	c := NewPresyncCollector(pool, testSite)
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": reasonOptions}); got != 0 {
		t.Errorf("scheduled options = %v, want 0", got)
	}
	if got := labeledGaugeValue(t, c, "rokuban_presync_pending", map[string]string{"reason": reasonOptionsDeferred}); got != 1 {
		t.Errorf("deferred options = %v, want 1", got)
	}

	endedAt := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site:        testSite,
		ProgramID:   programID,
		Title:       "終了済みへ変更",
		StartAt:     endedAt,
		DurationMs:  30 * time.Minute.Milliseconds(),
		NetworkID:   32678,
		ServiceID:   5168,
		ChannelType: "GR",
		Channel:     "27",
		EventID:     68006,
		ServiceName: "テストチャンネル",
	}); err != nil {
		t.Fatalf("updating program snapshot time: %v", err)
	}
	if err := q.UpsertScheduleSyncSnapshot(ctx, testSite); err != nil {
		t.Fatalf("refreshing schedule snapshot: %v", err)
	}

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
	c := NewPresyncCollector(pool, testSite)
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

func seedPresyncReservation(t *testing.T, pool *pgxpool.Pool, programID int64, startAt time.Time) {
	t.Helper()
	ctx := context.Background()
	q := sqlcgen.New(pool)
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site:        testSite,
		ProgramID:   programID,
		Title:       "presync test",
		StartAt:     startAt,
		DurationMs:  30 * time.Minute.Milliseconds(),
		NetworkID:   32678,
		ServiceID:   5168,
		ChannelType: "GR",
		Channel:     "27",
		EventID:     int32(programID % 100000),
		ServiceName: "テストチャンネル",
	}); err != nil {
		t.Fatalf("upserting program snapshot: %v", err)
	}
	if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: testSite, ProgramID: programID,
	}); err != nil {
		t.Fatalf("creating reservation: %v", err)
	}
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
	batch := sqlcgen.New(pool).UpsertScheduleSync(context.Background(), []sqlcgen.UpsertScheduleSyncParams{{
		Site:      testSite,
		ProgramID: programID,
		State:     state,
		Options:   optionsJSON,
		Tags:      tags,
	}})
	var batchErr error
	batch.Exec(func(_ int, err error) {
		if err != nil && batchErr == nil {
			batchErr = err
		}
	})
	if closeErr := batch.Close(); closeErr != nil && batchErr == nil {
		batchErr = closeErr
	}
	if batchErr != nil {
		t.Fatalf("upserting observed schedule: %v", batchErr)
	}
}

// seedIngestFor は既存の record_sync に対応する録画に原本アセットを追加する。
func seedIngestFor(t *testing.T, pool *pgxpool.Pool, recordID string) {
	t.Helper()
	ctx := context.Background()
	q := sqlcgen.New(pool)

	recordingID, err := q.GetRecordSyncRecordingID(ctx, sqlcgen.GetRecordSyncRecordingIDParams{
		Site:     testSite,
		RecordID: recordID,
	})
	if err != nil {
		t.Fatalf("looking up record_sync: %v", err)
	}
	if recordingID == nil {
		t.Fatalf("record_sync %q has no recording_id", recordID)
	}
	if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: *recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "test/" + recordID + ".m2ts",
		SizeBytes:   1,
	}); err != nil {
		t.Fatalf("creating media_asset: %v", err)
	}
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
