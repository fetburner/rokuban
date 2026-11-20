package metrics

import (
	"testing"
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
	IngestHashMismatches.Inc()
	TSScanDroppedPackets.Add(1)
	TSScanErrorPackets.Add(1)
	TSScanScrambledPackets.Add(1)
	TSScanDuration.Observe(1)
	TSScanJobs.WithLabelValues("success").Inc()
	EncodeDuration.Observe(1)
	EncodeJobs.WithLabelValues("success").Inc()
	ReconcilePendingDiff.WithLabelValues("create").Set(0)
	ReconcilePendingDiff.WithLabelValues("delete_deferred").Set(0)
	ReconcilePendingDiff.WithLabelValues("update").Set(0)
	ReconcileSchedules.WithLabelValues("created").Inc()
	ReconcileSchedules.WithLabelValues("recreated").Inc()
	ReconcileCircuitBreakerTrips.Add(1)
	ReconcileScheduleLost.Add(1)
	ReconcileWriteWithheld.WithLabelValues("delete", "gone").Inc()
	ReconcileStartDelayed.WithLabelValues(testSite).Set(0)
	ReconcileStartDelayErrors.WithLabelValues(testSite).Inc()
	RulerProgramIDReuses.Inc()
	RecordingsFailed.WithLabelValues("need-rescheduling").Inc()
	RecordsBroken.WithLabelValues("io-error").Inc()
	EpgSyncDuration.Observe(1)
	EpgProgramsProjected.Set(1)
	EpgChannelsWithoutPrograms.Set(0)
	TunersProjected.WithLabelValues(testSite).Set(2)
	CapacityOverages.WithLabelValues(testSite).Set(0)
	LiveSessionStartFailures.WithLabelValues("session_limit").Inc()
	LiveSessionStartFailures.WithLabelValues("record_not_ready_timeout").Inc()
	LiveSessionEvictions.WithLabelValues("upstream", "retry_succeeded").Inc()
	LiveIdleGCReclaimed.Add(1)
	LiveLeaveHints.WithLabelValues("deadline_shortened").Inc()
	LiveIdleGCLastPass.SetToCurrentTime()
	EncodeReconcileCandidates.Set(0)
	EncodeReconcileUnsatisfiable.WithLabelValues("h264").Set(0)
	ThumbnailReconcileCandidates.Set(0)
	MediaAssetsMissing.WithLabelValues("original").Set(0)
	OrphanFilesUnresolved.Set(0)
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
	// TS scan 統計 / recording.failed 理由別
	// （未 ingest record 総量は BacklogCollector 側でテストする）
	required := []string{
		"rokuban_ingest_bytes_total",
		"rokuban_ingest_duration_seconds",
		"rokuban_ingest_jobs_total",
		"rokuban_ingest_hash_mismatches_total",
		"rokuban_ts_scan_dropped_packets_total",
		"rokuban_ts_scan_error_packets_total",
		"rokuban_ts_scan_scrambled_packets_total",
		"rokuban_ts_scan_duration_seconds",
		"rokuban_ts_scan_jobs_total",
		"rokuban_encode_duration_seconds",
		"rokuban_encode_jobs_total",
		"rokuban_reconcile_pending_diff",
		"rokuban_reconcile_schedules_total",
		"rokuban_reconcile_circuit_breaker_trips_total",
		"rokuban_reconcile_schedule_lost_total",
		"rokuban_reconcile_write_withheld_total",
		"rokuban_reconcile_start_delayed",
		"rokuban_reconcile_start_delay_errors_total",
		"rokuban_recordings_failed_total",
		"rokuban_records_broken_total",
		"rokuban_epg_sync_duration_seconds",
		"rokuban_epg_programs_projected",
		"rokuban_epg_channels_without_programs",
		// program_id 再利用検出（ruler）
		"rokuban_ruler_program_id_reuse_total",
		// M2-10: チューナー射影と容量超過
		"rokuban_tuners_projected",
		"rokuban_capacity_overages",
		// issue #91: ライブ視聴
		"rokuban_live_active_sessions",
		"rokuban_live_session_start_failures_total",
		"rokuban_live_session_evictions_total",
		"rokuban_live_idle_gc_reclaimed_total",
		"rokuban_live_idle_gc_last_pass_timestamp_seconds",
		"rokuban_recording_hls_cache_bytes",
		"rokuban_recording_hls_cache_requests_total",
		"rokuban_recording_hls_active_transforms",
		"rokuban_recording_hls_unavailable_total",
		// issue #191: 離脱ヒント（idle GC 回収数と対で読む）
		"rokuban_live_leave_hints_total",
		// issue #163: encode の desired−observed 定期パス。バックストップ自身が
		// 黙って止まる / 窓に張り付く / 設定から消えたプロファイルで落とす、の
		// 3 通りの黙り方に対応する（internal/worker/encode_reconcile.go）。
		"rokuban_encode_reconcile_candidates",
		"rokuban_encode_reconcile_unsatisfiable",
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
	for _, name := range []string{
		"rokuban_ingest_dropped_packets_total",
		"rokuban_ingest_error_packets_total",
		"rokuban_ingest_scrambled_packets_total",
		// プロセス内の最終パス時刻は DB の印（loop_success_markers）に置き換えた。
		// ScaledJob では scrape できず、常駐 Pod では成功時刻を表さないため登録しない。
		"rokuban_ruler_last_pass_timestamp_seconds",
		"rokuban_sweep_last_pass_timestamp_seconds",
		"rokuban_ts_scan_reconcile_last_pass_timestamp_seconds",
		"rokuban_encode_reconcile_last_pass_timestamp_seconds",
		"rokuban_thumbnail_reconcile_last_pass_timestamp_seconds",
		"rokuban_delete_reconcile_last_pass_timestamp_seconds",
		"rokuban_reconcile_last_pass_timestamp_seconds",
		// storage 観測の最終時刻も DB の storage_sync から読む系列に一本化した
		// （rokuban_storage_root_observed_timestamp_seconds）。
		"rokuban_storage_sync_last_success_timestamp_seconds",
		"rokuban_storage_root_last_success_timestamp_seconds",
	} {
		if got[name] {
			t.Errorf("removed metric %q is still registered", name)
		}
	}

	// プロセスの状態も見られること（Go / process コレクタ）
	for _, name := range []string{"go_goroutines", "process_open_fds"} {
		if !got[name] {
			t.Errorf("runtime metric %q is not registered", name)
		}
	}
}

func TestLiveActiveSessionsInitializesKinds(t *testing.T) {
	families, err := NewRegistry(nil).Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	got := make(map[string]float64)
	for _, family := range families {
		if family.GetName() != "rokuban_live_active_sessions" {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "kind" {
					got[label.GetValue()] = metric.GetGauge().GetValue()
				}
			}
		}
	}

	for _, kind := range []string{"live"} {
		value, ok := got[kind]
		if !ok {
			t.Errorf("active session metric is missing kind=%q at startup", kind)
		} else if value != 0 {
			t.Errorf("active session metric kind=%q = %v at startup, want 0", kind, value)
		}
	}
}

// 同じコレクタを 2 つの registry に登録できること
// （複数回 NewRegistry を呼んでも panic しない）。
func TestNewRegistry_Twice(t *testing.T) {
	_ = NewRegistry(nil)
	_ = NewRegistry(nil)
}

// ruler / record_sweep の成功時刻はプロセス内ゲージではなく DB の marker から
// scrape できることを固定する。ScaledJob が終了した後も常駐プロセスから読める
// 経路なので、collector が旧ゲージを読む実装に戻るとこのテストで分かる。
