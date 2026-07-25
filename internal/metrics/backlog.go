package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// backlogQueryTimeout は scrape 中の DB クエリの上限。
// Prometheus の scrape timeout（既定 10 秒）より短くする。
const backlogQueryTimeout = 5 * time.Second

// BacklogCollector は未 ingest record の滞留量を scrape のたびに DB から取り直す。
//
// プロセス内に値を溜めないので、どのロールが scrape されても同じ値が出る。
// 「イベントはヒント、真実はテーブル再読」と同じ考え方で、ingest が詰まっている
// かどうかはカウンタの差分ではなく DB の現状で判断する。
//
// エッジの録画バッファのサイジングはこの値とディスク残量アラートを対で使う
// （issue #4 のサイジングコメント）。
type BacklogCollector struct {
	pool *pgxpool.Pool
	site string

	records *prometheus.Desc
	bytes   *prometheus.Desc
	errors  prometheus.Counter
}

// NewBacklogCollector は BacklogCollector を生成する。
func NewBacklogCollector(pool *pgxpool.Pool, site string) *BacklogCollector {
	labels := prometheus.Labels{"site": site}
	return &BacklogCollector{
		pool: pool,
		site: site,
		records: prometheus.NewDesc(
			"rokuban_uningested_records",
			"Finished mirakc records not yet committed to Rokuban (ingest backlog).",
			nil, labels,
		),
		bytes: prometheus.NewDesc(
			"rokuban_uningested_record_bytes",
			"Total bytes of finished mirakc records not yet committed to Rokuban.",
			nil, labels,
		),
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "rokuban_uningested_backlog_scrape_errors_total",
			Help:        "Failures while querying the ingest backlog during a scrape.",
			ConstLabels: labels,
		}),
	}
}

// Describe は prometheus.Collector を満たす。
func (c *BacklogCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.records
	ch <- c.bytes
	c.errors.Describe(ch)
}

// Collect は prometheus.Collector を満たす。
//
// クエリが失敗した場合はメトリクスを出さずに専用のエラーカウンタだけを進める。
// 0 を報告すると「滞留なし」と区別できず、滞留アラートを黙って無効化してしまう。
func (c *BacklogCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), backlogQueryTimeout)
	defer cancel()

	row, err := sqlcgen.New(c.pool).GetUningestedRecordBacklog(ctx, c.site)
	if err != nil {
		slog.Error("metrics: querying ingest backlog", "site", c.site, "err", err)
		c.errors.Inc()
		c.errors.Collect(ch)
		return
	}

	ch <- prometheus.MustNewConstMetric(c.records, prometheus.GaugeValue, float64(row.Records))
	ch <- prometheus.MustNewConstMetric(c.bytes, prometheus.GaugeValue, float64(row.Bytes))
	c.errors.Collect(ch)
}
