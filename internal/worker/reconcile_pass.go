package worker

import (
	"context"
	"time"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reconciler"
	"github.com/fetburner/rokuban/internal/store"
)

// ReconcilePassWorker は reconciler の 1 パス突き合わせを実行する queue worker。
//
// ロジックは internal/reconciler にそのまま置いてあり、ここでは呼び出すだけ
// （ロジックの移植はしない）。reconciler は mirakc への HTTP を伴うため、ruler と
// 違って MirakcClients を依存として持つ。
type ReconcilePassWorker struct {

	// MirakcClients は site → mirakc クライアントの map（issue #532）。この
	// 1 インスタンスが複数 site の reconciler_<site> キューを同時に購読しうる。
	// Work は verifySite で args.Site に対応するクライアントを取り出してから
	// reconciler.New に渡す（issue #139）。
	MirakcClients  map[string]*mirakc.Client
	ScheduleSync   store.ScheduleSync
	Breakers       store.Breakers
	NeverScheduled store.NeverScheduledEvents

	// StartDelayGrace は開始遅延検出器の猶予（reconciler.Config.StartDelayGrace）。
	// 0 なら reconciler 側の既定値を使う（config.yml の
	// reconciler.start_delay_grace から注入される）。
	StartDelayGrace time.Duration
}

// Work は reconciler の 1 パス突き合わせを実行する。対象サイトはジョブ引数の 1 サイトのみ
// （ジョブの排他がサイト単位のため）。
//
// reconciler.Reconciler 自身はパスを跨ぐ状態を持たないため、ジョブごとに毎回
// 新規に生成してよい（issue #24 M2-17）。大量削除サーキットブレーカー
// （breaker.ReconcileTotalLoss）は例外で、発動のラッチは Reconciler 構造体ではなく
// circuit_breakers テーブルに永続化される（issue #24 M2-5）。RunPass はパスの
// 先頭でその状態を DB から読み直すので、ここで毎回新規生成しても発動状態は
// 失われない。
func (w *ReconcilePassWorker) Work(ctx context.Context, args jobs.ReconcilePassArgs) error {
	// mirakc インスタンスはサイトスコープ。他サイトのジョブをこのプロセスの
	// mirakc に投げると、別インスタンスの schedules をこのサイトの予約として
	// 作成/削除しうる（issue #139）。reconciler.New/RunPass より前に照合する。
	client, err := verifySite(w.MirakcClients, args.Site, jobs.ReconcilerQueue)
	if err != nil {
		return err
	}

	rec := reconciler.New(args.Site, client, w.ScheduleSync, w.Breakers, w.NeverScheduled, &reconciler.Config{
		StartDelayGrace: w.StartDelayGrace,
	})
	return rec.RunPass(ctx)
}
