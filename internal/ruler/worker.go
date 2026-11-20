package ruler

import (
	"context"
	"log/slog"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
)

// PassWorker は ruler の 1 パス評価を実行する queue worker。
//
// 評価ロジックは Ruler（このパッケージ）に置き、永続化は store 契約を使う。ruler は
// mirakc に触れない（不変条件 1）。ジョブの種類・キューの定義は internal/jobs にある。
//
// # site 照合ガード（issue #139）は不要と判断
//
// 他サイトの worker が args.Site の異なる ruler_pass ジョブを掴んでも、触るのは
// この site の DB 行（reservations 等）だけで、mirakc にも FS にも触れない。
// 「A の mirakc に B の id を投げる」形の壊れ方（issue #139 本文）が原理的に
// 起きないので、jobs.VerifySite を呼ばない（「まだ書いていないだけ」ではなく
// 不要と判断した結果）。
// Stores は ruler が予約の導出と適用に使う永続化の契約。PassWorker に埋め込み、ruler だけが使う。
type Stores struct {
	RulerPass     store.RulerPass
	ProgramSearch store.ProgramSearch
}

// PassWorker は ruler の 1 パス評価を実行する queue worker。
//
// 評価ロジックは Ruler（このパッケージ）に置き、永続化は store 契約を使う。ruler は
// mirakc に触れない（不変条件 1）。ジョブの種類・キューの定義は internal/jobs にある。
//
// # site 照合ガード（issue #139）は不要と判断
//
// 他サイトの worker が args.Site の異なる ruler_pass ジョブを掴んでも、触るのは
// この site の DB 行（reservations 等）だけで、mirakc にも FS にも触れない。
// 「A の mirakc に B の id を投げる」形の壊れ方（issue #139 本文）が原理的に
// 起きないので、jobs.VerifySite を呼ばない（「まだ書いていないだけ」ではなく
// 不要と判断した結果）。
type PassWorker struct {
	// Config は ruler の設定値。本番では config.yml の epg.retention_grace・
	// ruler.max_deletes_per_pass・ruler.retract_grace から注入される。
	// RetentionGrace と MaxDeletesPerPass は 0 なら ruler 側の既定値、RetractGrace は 0 で
	// 無効（解釈は New が持つ）。Now は本番では nil のままにする。
	Config Config

	Stores
	Enqueuer       jobq.Enqueuer
	Breakers       store.Breakers
	NeverScheduled store.NeverScheduledEvents
	LoopMarkers    store.LoopMarkers
}

// Work は ruler の 1 パス評価を実行する。対象サイトはジョブ引数の 1 サイトのみ
// （New はサイトのスライスを受け取れるが、ジョブの排他がサイト単位のため
// ここでは常に長さ 1 で渡す）。
func (w *PassWorker) Work(ctx context.Context, args jobs.RulerPassArgs) error {
	r := New([]string{args.Site}, w.RulerPass, w.Breakers, w.NeverScheduled, w.LoopMarkers, &w.Config, w.ProgramSearch)
	if err := r.RunPass(ctx); err != nil {
		return err
	}

	// ruler パスの完了は reconcile_pass 起動契機のヒントの 1 つ（docs/recording.md
	// §3.2「ruler パスの完了」）。base が変われば mirakc に反映すべき差分が増えるため、
	// reconcile を早める。epg_sync 完了時の ruler_pass ヒント（epg.go 参照）と同じ形。
	// トランザクション整合の要求はなく、あくまで定期パスを前倒しするヒントなので、
	// Enqueuer が無い場合は投入せず次の定期パスに委ねる。
	if w.Enqueuer != nil {
		if _, insertErr := w.Enqueuer.Enqueue(ctx, jobs.ReconcilePassArgs(args)); insertErr != nil {
			slog.Warn("ruler pass: inserting reconcile_pass hint failed", "err", insertErr)
		}
	}

	return nil
}
