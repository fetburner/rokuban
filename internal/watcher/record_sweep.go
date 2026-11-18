package watcher

import (
	"context"
	"fmt"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/webhook"
)

// SweepWorker は watcher の定期全量突き合わせ（(c)）を実行する queue worker。
//
// ロジックは Watcher.Sweep にそのまま置いてあり、ここでは呼び出すだけ
// （ロジックの移植はしない）。M2-16 で processRecord が
// record_sync の (site, record_id) 行ロックにより冪等化されているため、SSE 由来の
// 処理（(a)(b)、Watcher.Run の常駐側）とこのジョブ（(c)）が同一 record を並行処理
// しても recordings は重複しない（docs/recording.md §3.3「record 処理は並行実行
// しても壊れない」）。
type SweepWorker struct {
	Enqueuer jobq.Enqueuer

	// MirakcClients は site → mirakc クライアントの map（issue #532）。この
	// 1 インスタンスが複数 site の watcher_<site> キューを同時に購読しうる。Work は
	// jobs.VerifySite で args.Site に対応するクライアントを取り出してから
	// New に渡す（issue #139）。
	MirakcClients      map[string]*mirakc.Client
	RecordObservations store.RecordObservations
	LoopMarkers        store.LoopMarkers
	// Webhook は processRecord 経由の finished 遷移通知に使う（M3-11）。nil 可。
	Webhook *webhook.Client
}

// Work は watcher の定期全量突き合わせを実行する。対象サイトはジョブ引数の 1 サイトのみ
// （ジョブの排他がサイト単位のため）。
//
// Watcher は観測を commit した後に ingest ジョブをヒントとして enqueue する。
func (w *SweepWorker) Work(ctx context.Context, args jobs.RecordSweepArgs) error {
	// mirakc インスタンスはサイトスコープ。他サイトのジョブをこのプロセスの
	// mirakc に投げると、`GET /api/recording/records` の全量取得から他インスタンスの
	// record をこのサイトの recording として processRecord が取り込みうる
	// （issue #139）。New/Sweep より前に照合する。
	client, err := jobs.VerifySite(w.MirakcClients, args.Site, jobs.RecordSweepQueue)
	if err != nil {
		return err
	}

	if w.Enqueuer == nil {
		return fmt.Errorf("record sweep: job enqueuer is nil")
	}

	wt := New(args.Site, client, w.RecordObservations, w.LoopMarkers, w.Enqueuer, w.Webhook)
	return wt.Sweep(ctx)
}
