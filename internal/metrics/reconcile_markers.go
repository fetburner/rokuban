package metrics

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fetburner/rokuban/internal/store"
)

// reconcileMarkersQueryTimeout は reconcile marker collector が scrape を占有する上限。
const reconcileMarkersQueryTimeout = 5 * time.Second

// 読み取りの失敗を数える source の値。読めなかった表の系列だけを落とし、
// その系列を 0 に置き換えないので、どの表が読めなかったかをこのラベルで分ける。
const (
	reconcileMarkerSourcePass    = "reconcile_pass"
	reconcileMarkerSourceDelete  = "delete_reconcile"
	reconcileMarkerSourceStorage = "storage_sync"
)

// ReconcileMarkerCollector は desired−observed の全件パスの完走時刻と、置き場ごとの
// storage sync の観測時刻を scrape ごとに DB から読み直す。
//
// encode / thumbnail / ts_scan の reconcile と削除 reconcile は ScaledJob の --once で
// 動くため、プロセス内ゲージは常駐プロセスの /metrics から読めない。この collector は
// 完走時刻を保存した loop_success_markers（loop = reconcile_pass / delete_reconcile）と、
// 既存の storage_sync.observed_at を読んで、どの Pod からも同じ値を出す。
//
// 削除の集合は削除パスを書く worker がある置き場、容量観測の集合は FS の置き場とする。
// 設定から外れた置き場の行は出さない。
//
// 3 つの読み取りは独立に扱う。失敗した読み取りの系列だけを落とし、0 に置き換えずに
// reconcileMarkerScrapeErrors を進める（LoopPassCollector と同じ方針）。
type ReconcileMarkerCollector struct {
	markers      store.LoopMarkers
	storage      store.StorageSyncReader
	stores       []string
	deleteStores []string

	encode     *prometheus.Desc
	thumbnail  *prometheus.Desc
	tsScan     *prometheus.Desc
	deleteDesc *prometheus.Desc
	storageDsc *prometheus.Desc
	errors     *prometheus.CounterVec
}

// NewReconcileMarkerCollector は全件パスの完走時刻と FS の置き場の観測時刻を出す collector を作る。
// stores は FS の置き場、deleteStores は削除パスを書く worker がある置き場の名前。
func NewReconcileMarkerCollector(markers store.LoopMarkers, storage store.StorageSyncReader, stores []string, deleteStores []string) *ReconcileMarkerCollector {
	names := slices.Clone(stores)
	slices.Sort(names)
	readErrors := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rokuban_reconcile_marker_scrape_errors_total",
		Help: "Failures while reading the saved reconcile pass markers or storage sync observations during a scrape, by source. The series of the table that could not be read is omitted for that scrape.",
	}, []string{"source"})
	// CounterVec は系列を初めて使うまで出ないので、起動直後から 0 で見えるようにする。
	for _, source := range []string{reconcileMarkerSourcePass, reconcileMarkerSourceDelete, reconcileMarkerSourceStorage} {
		readErrors.WithLabelValues(source)
	}
	return &ReconcileMarkerCollector{
		markers:      markers,
		storage:      storage,
		stores:       names,
		deleteStores: slices.Clone(deleteStores),
		encode: prometheus.NewDesc(
			"rokuban_encode_reconcile_last_success_timestamp_seconds",
			"Unix time (database clock) of the last completed page of a full (non-targeted) encode reconcile pass. A pass that continues across pages writes this at the end of every page, so it means the pass is running, not that every candidate was seen. Zero means no full pass has completed yet.",
			nil, nil,
		),
		thumbnail: prometheus.NewDesc(
			"rokuban_thumbnail_reconcile_last_success_timestamp_seconds",
			"Unix time (database clock) of the last completed page of a full (non-targeted) thumbnail reconcile pass, written only when the seek tiles list also succeeded. A pass that continues across pages writes this at the end of every page, so it means the pass is running, not that every candidate was seen. Zero means no full pass has completed yet.",
			nil, nil,
		),
		tsScan: prometheus.NewDesc(
			"rokuban_ts_scan_reconcile_last_success_timestamp_seconds",
			"Unix time (database clock) of the last completed page of a full (non-targeted) ts-scan reconcile pass. A pass that continues across pages writes this at the end of every page, so it means the pass is running, not that every candidate was seen. Zero means no full pass has completed yet.",
			nil, nil,
		),
		deleteDesc: prometheus.NewDesc(
			"rokuban_delete_reconcile_last_success_timestamp_seconds",
			"Unix time (database clock) of the last delete reconcile pass for the store that completed, including a pass that the bulk-delete circuit breaker stopped. Zero means no pass has completed for the store yet.",
			[]string{"store"}, nil,
		),
		storageDsc: prometheus.NewDesc(
			"rokuban_storage_root_observed_timestamp_seconds",
			"Unix time (database clock) of the last successful statfs observation of the root in the store, read from storage_sync. A stale root keeps its old value while the other roots advance. A store that has no observation yet reports root=\"media\" as zero.",
			[]string{"store", "root"}, nil,
		),
		errors: readErrors,
	}
}

// Describe は prometheus.Collector を満たす。
func (c *ReconcileMarkerCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.encode
	ch <- c.thumbnail
	ch <- c.tsScan
	ch <- c.deleteDesc
	ch <- c.storageDsc
	c.errors.Describe(ch)
}

// Collect は保存した時刻を現在値として出す。読めなかった表の系列は出さず、エラーカウンタを進める。
func (c *ReconcileMarkerCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), reconcileMarkersQueryTimeout)
	defer cancel()

	c.collectPasses(ctx, ch)
	c.collectDeletes(ctx, ch)
	c.collectStorage(ctx, ch)
	c.errors.Collect(ch)
}

func (c *ReconcileMarkerCollector) collectPasses(ctx context.Context, ch chan<- prometheus.Metric) {
	saved, err := c.markers.ReadLoopMarkers(ctx, store.LoopReconcilePass)
	if err != nil {
		c.recordError(reconcileMarkerSourcePass, err)
		return
	}
	for _, p := range []struct {
		desc *prometheus.Desc
		pass string
	}{
		{c.encode, store.ReconcilePassEncode},
		{c.thumbnail, store.ReconcilePassThumbnail},
		{c.tsScan, store.ReconcilePassTSScan},
	} {
		ch <- prometheus.MustNewConstMetric(p.desc, prometheus.GaugeValue, unixSecondsOrZero(saved[p.pass]))
	}
}

func (c *ReconcileMarkerCollector) collectDeletes(ctx context.Context, ch chan<- prometheus.Metric) {
	saved, err := c.markers.ReadLoopMarkers(ctx, store.LoopDeleteReconcile)
	if err != nil {
		c.recordError(reconcileMarkerSourceDelete, err)
		return
	}
	for _, name := range c.deleteStores {
		ch <- prometheus.MustNewConstMetric(c.deleteDesc, prometheus.GaugeValue, unixSecondsOrZero(saved[name]), name)
	}
}

func (c *ReconcileMarkerCollector) collectStorage(ctx context.Context, ch chan<- prometheus.Metric) {
	rows, err := c.storage.ListStorageSync(ctx)
	if err != nil {
		c.recordError(reconcileMarkerSourceStorage, err)
		return
	}
	for _, name := range c.stores {
		observed := 0
		for _, row := range rows {
			if row.Store != name {
				continue
			}
			observed++
			ch <- prometheus.MustNewConstMetric(c.storageDsc, prometheus.GaugeValue,
				float64(row.ObservedAt.UnixNano())/float64(time.Second), name, row.Root)
		}
		if observed == 0 {
			// まだ一度も観測していない置き場は、media の root を 0 として出す。
			ch <- prometheus.MustNewConstMetric(c.storageDsc, prometheus.GaugeValue, 0, name, "media")
		}
	}
}

func (c *ReconcileMarkerCollector) recordError(source string, err error) {
	slog.Error("metrics: reading reconcile markers", "source", source, "err", err)
	c.errors.WithLabelValues(source).Inc()
}

// unixSecondsOrZero は未記録（ゼロ値の時刻）を 0 に変換する。
func unixSecondsOrZero(at time.Time) float64 {
	if at.IsZero() {
		return 0
	}
	return float64(at.UnixNano()) / float64(time.Second)
}
