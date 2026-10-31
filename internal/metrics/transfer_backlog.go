package metrics

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fetburner/rokuban/internal/store"
)

// transferBacklogQueryTimeout は transfer backlog collector が scrape を占有する上限。
const transferBacklogQueryTimeout = 5 * time.Second

// TransferBacklogCollector は、encode reconcile が保存した置き場ごとの転送待ちの件数を scrape ごとに
// DB から読み直し、次の 2 本を出す。
//
//   - rokuban_storage_transfer_backlog_assets{store}: 直近に数えた転送待ちの論理資産数
//   - rokuban_encode_backpressure{store}: 件数が上限に達していれば 1、そうでなければ 0
//
// cleanup キューは ScaledJob の --once で動くので、worker のプロセス内ゲージは scrape できない。
// 保存した件数を読むだけで、scrape が件数を数え直す重い集計は走らせない。
// limits は storage.transfer.backlog_limit の正の値で、止まっているかを今の上限から導出する。
// 上限の無い置き場と、まだ数えていない置き場は系列を出さない。
type TransferBacklogCollector struct {
	reader store.TransferBacklogReader
	limits map[string]int

	backlog      *prometheus.Desc
	backpressure *prometheus.Desc
	errors       prometheus.Counter
}

// NewTransferBacklogCollector は上限付きの置き場の collector を作る。
func NewTransferBacklogCollector(reader store.TransferBacklogReader, limits map[string]int) *TransferBacklogCollector {
	return &TransferBacklogCollector{
		reader: reader,
		limits: limits,
		backlog: prometheus.NewDesc(
			"rokuban_storage_transfer_backlog_assets",
			"Logical media assets waiting for a desired-store copy, by source FS store, for stores with storage.transfer.backlog_limit, as last counted by encode reconcile. An asset with copies in several source stores counts once per store.",
			[]string{"store"}, nil,
		),
		backpressure: prometheus.NewDesc(
			"rokuban_encode_backpressure",
			"1 while encode reconcile is not enqueuing encodes that read originals from the store because its last counted transfer backlog reached storage.transfer.backlog_limit, else 0.",
			[]string{"store"}, nil,
		),
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "rokuban_transfer_backlog_scrape_errors_total",
			Help: "Failures while reading the saved transfer backlog during a scrape.",
		}),
	}
}

// Describe は prometheus.Collector を満たす。
func (c *TransferBacklogCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.backlog
	ch <- c.backpressure
	c.errors.Describe(ch)
}

// Collect は保存した件数を現在値として出す。読めないときは値を出さずエラーカウンタだけを進める。
func (c *TransferBacklogCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), transferBacklogQueryTimeout)
	defer cancel()

	saved, err := c.reader.ReadTransferBacklog(ctx)
	if err != nil {
		slog.Error("metrics: reading transfer backlog", "err", err)
		c.errors.Inc()
		c.errors.Collect(ch)
		return
	}
	names := make([]string, 0, len(c.limits))
	for name := range c.limits {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		count, ok := saved[name]
		if !ok {
			continue
		}
		stopped := 0.0
		if count >= int64(c.limits[name]) {
			stopped = 1
		}
		ch <- prometheus.MustNewConstMetric(c.backlog, prometheus.GaugeValue, float64(count), name)
		ch <- prometheus.MustNewConstMetric(c.backpressure, prometheus.GaugeValue, stopped, name)
	}
	c.errors.Collect(ch)
}
