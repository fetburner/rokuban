package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fetburner/rokuban/internal/store"
)

// loopPassQueryTimeout は loop pass marker collector が scrape を占有する上限。
// Prometheus の scrape timeout（既定 10 秒）より短くする。
const loopPassQueryTimeout = 5 * time.Second

// LoopPassCollector は ruler / record_sweep の成功 marker を scrape ごとに DB から
// 読み直す。ScaledJob の --once で処理した後も、常駐プロセスの /metrics から同じ
// site の成功鮮度を観測できる。
//
// ScaledJob の停止検出はこの collector が出す site ラベル付きメトリクスを使う。
// プロセス内の最終パス時刻ゲージは、常駐 Pod でしか読めず値が成功時刻を表さないため持たない。
type LoopPassCollector struct {
	markers store.LoopMarkers
	site    string

	ruler  *prometheus.Desc
	sweep  *prometheus.Desc
	errors prometheus.Counter
}

// NewLoopPassCollector は 1 サイト分の ruler / record_sweep collector を作る。
func NewLoopPassCollector(markers store.LoopMarkers, site string) *LoopPassCollector {
	labels := prometheus.Labels{"site": site}
	return &LoopPassCollector{
		markers: markers,
		site:    site,
		ruler: prometheus.NewDesc(
			"rokuban_ruler_last_success_timestamp_seconds",
			"Unix time of the last successful ruler pass for this site. Zero means no pass has completed.",
			nil, labels,
		),
		sweep: prometheus.NewDesc(
			"rokuban_sweep_last_success_timestamp_seconds",
			"Unix time of the last successful record_sweep pass for this site. Zero means no pass has completed.",
			nil, labels,
		),
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "rokuban_loop_pass_marker_scrape_errors_total",
			Help:        "Failures while querying ruler or record_sweep success markers during a scrape.",
			ConstLabels: labels,
		}),
	}
}

// Describe は prometheus.Collector を満たす。
func (c *LoopPassCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.ruler
	ch <- c.sweep
	c.errors.Describe(ch)
}

// Collect は DB-backed marker を現在値として出す。marker がまだ無いサイトは
// 0 を出すが、DB への問い合わせ自体が失敗した場合は 0 を出さずエラーカウンタ
// だけを進める（「未実行」と「観測不能」を混同しない）。
func (c *LoopPassCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), loopPassQueryTimeout)
	defer cancel()

	result, err := c.read(ctx)
	if err != nil {
		slog.Error("metrics: querying loop pass markers", "site", c.site, "err", err)
		c.errors.Inc()
		c.errors.Collect(ch)
		return
	}

	ch <- prometheus.MustNewConstMetric(c.ruler, prometheus.GaugeValue, result.rulerAt)
	ch <- prometheus.MustNewConstMetric(c.sweep, prometheus.GaugeValue, result.sweepAt)
	c.errors.Collect(ch)
}

type loopPassResult struct {
	rulerAt float64
	sweepAt float64
}

func (c *LoopPassCollector) read(ctx context.Context) (loopPassResult, error) {
	rulers, err := c.markers.ReadLoopMarkers(ctx, store.LoopRuler)
	if err != nil {
		return loopPassResult{}, fmt.Errorf("reading ruler markers: %w", err)
	}
	sweeps, err := c.markers.ReadLoopMarkers(ctx, store.LoopRecordSweep)
	if err != nil {
		return loopPassResult{}, fmt.Errorf("reading record sweep markers: %w", err)
	}

	return loopPassResult{
		rulerAt: unixSecondsOrZero(rulers[c.site]),
		sweepAt: unixSecondsOrZero(sweeps[c.site]),
	}, nil
}
