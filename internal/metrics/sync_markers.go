package metrics

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fetburner/rokuban/internal/store"
)

// syncMarkersQueryTimeout は sync marker collector が scrape を占有する上限。
const syncMarkersQueryTimeout = 5 * time.Second

// SyncMarkerCollector は EPG 同期・チューナー同期・物理転送の確定時刻を scrape ごとに DB から読む。
//
// これらの書き手は ScaledJob の --once で動くので、プロセス内ゲージは常駐 Pod の /metrics に
// 値を残せない（0 になる）。確定時刻は loop_success_markers に書き、どの Pod からも同じ値を読む。
//
// 登録は設定された全 site に対して行う（呼び出し側が渡す sites）。EPG のラベルなし系列は、
// その site 集合の行の最大値（最新の site の時刻）。物理転送は置き場を問わない 1 行を読む。
// 3 つの読み取りは独立に扱う。失敗した読み取りの系列だけを落とし、0 に置き換えずに
// そのソースのエラーカウンタを進める（ReconcileMarkerCollector と同じ方針）。
type SyncMarkerCollector struct {
	markers store.LoopMarkers
	sites   []string

	epg      *prometheus.Desc
	tuner    *prometheus.Desc
	transfer *prometheus.Desc
	errors   *prometheus.CounterVec
}

// NewSyncMarkerCollector は設定された全 site（sites）の EPG 同期・チューナー同期と、物理転送の時刻を出す collector を作る。
func NewSyncMarkerCollector(markers store.LoopMarkers, sites []string) *SyncMarkerCollector {
	readErrors := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rokuban_sync_marker_scrape_errors_total",
		Help: "Failures while reading the saved EPG sync, tuner sync or storage transfer success markers during a scrape, by source. The series of the marker that could not be read is omitted for that scrape.",
	}, []string{"source"})
	// CounterVec は系列を初めて使うまで出ないので、起動直後から 0 で見えるようにする。
	for _, source := range []string{store.LoopEPGSync, store.LoopTunerSync, store.LoopStorageTransfer} {
		readErrors.WithLabelValues(source)
	}
	return &SyncMarkerCollector{
		markers: markers,
		sites:   slices.Clone(sites),
		epg: prometheus.NewDesc(
			"rokuban_epg_sync_last_success_timestamp_seconds",
			"Unix time (database clock) of the last completed full EPG sync, taking the latest over the configured sites. Zero means no site has completed one yet. A flat value means the sync job has stopped; a read failure omits the series instead.",
			nil, nil,
		),
		tuner: prometheus.NewDesc(
			"rokuban_tuner_sync_last_success_timestamp_seconds",
			"Unix time (database clock) of the last completed full tuner sync for the site. Zero means this site has not completed one yet. A read failure omits the series instead.",
			[]string{"site"}, nil,
		),
		transfer: prometheus.NewDesc(
			"rokuban_storage_transfer_last_success_timestamp_seconds",
			"Unix time (database clock) of the last physical copy that became active in any store. Zero means none has completed. It does not advance while the transfer queue is empty, so read it with the waiting-assets backlog. A read failure omits the series instead.",
			nil, nil,
		),
		errors: readErrors,
	}
}

// Describe は prometheus.Collector を満たす。
func (c *SyncMarkerCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.epg
	ch <- c.tuner
	ch <- c.transfer
	c.errors.Describe(ch)
}

// Collect は保存した時刻を現在値として出す。読めなかった loop の系列は出さず、エラーカウンタを進める。
func (c *SyncMarkerCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), syncMarkersQueryTimeout)
	defer cancel()

	c.collectEPG(ctx, ch)
	c.collectTuner(ctx, ch)
	c.collectTransfer(ctx, ch)
	c.errors.Collect(ch)
}

func (c *SyncMarkerCollector) collectEPG(ctx context.Context, ch chan<- prometheus.Metric) {
	saved, err := c.markers.ReadLoopMarkers(ctx, store.LoopEPGSync)
	if err != nil {
		c.recordError(store.LoopEPGSync, err)
		return
	}
	var latest time.Time
	for _, site := range c.sites {
		if at := saved[site]; at.After(latest) {
			latest = at
		}
	}
	ch <- prometheus.MustNewConstMetric(c.epg, prometheus.GaugeValue, unixSecondsOrZero(latest))
}

func (c *SyncMarkerCollector) collectTuner(ctx context.Context, ch chan<- prometheus.Metric) {
	saved, err := c.markers.ReadLoopMarkers(ctx, store.LoopTunerSync)
	if err != nil {
		c.recordError(store.LoopTunerSync, err)
		return
	}
	for _, site := range c.sites {
		ch <- prometheus.MustNewConstMetric(c.tuner, prometheus.GaugeValue, unixSecondsOrZero(saved[site]), site)
	}
}

func (c *SyncMarkerCollector) collectTransfer(ctx context.Context, ch chan<- prometheus.Metric) {
	saved, err := c.markers.ReadLoopMarkers(ctx, store.LoopStorageTransfer)
	if err != nil {
		c.recordError(store.LoopStorageTransfer, err)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.transfer, prometheus.GaugeValue, unixSecondsOrZero(saved[store.StorageTransferScope]))
}

func (c *SyncMarkerCollector) recordError(source string, err error) {
	slog.Error("metrics: reading sync markers", "source", source, "err", err)
	c.errors.WithLabelValues(source).Inc()
}
