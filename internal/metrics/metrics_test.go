package metrics

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
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
	ReconcileSchedules.WithLabelValues("created").Inc()
	ReconcileCircuitBreakerTrips.Add(1)
	RecordingsFailed.WithLabelValues("need-rescheduling").Inc()
	EpgSyncDuration.Observe(1)
	EpgProgramsProjected.Set(1)
	EpgChannelsWithoutPrograms.Set(0)

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
		"rokuban_reconcile_schedules_total",
		"rokuban_reconcile_circuit_breaker_trips_total",
		"rokuban_recordings_failed_total",
		"rokuban_epg_sync_duration_seconds",
		"rokuban_epg_programs_projected",
		"rokuban_epg_channels_without_programs",
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
