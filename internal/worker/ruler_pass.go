package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/ruler"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
)

// RulerPassWorker は ruler の 1 パス評価を実行する queue worker。
//
// 評価ロジックは internal/ruler に置き、永続化は store 契約を使う。ruler は
// mirakc に触れない（不変条件 1）ので、Pool から store を構成して渡す。
//
// # site 照合ガード（issue #139）は不要と判断
//
// 他サイトの worker が args.Site の異なる ruler_pass ジョブを掴んでも、触るのは
// この site の DB 行（reservations 等）だけで、mirakc にも FS にも触れない。
// 「A の mirakc に B の id を投げる」形の壊れ方（issue #139 本文）が原理的に
// 起きないので、verifySite は導入していない（「まだ書いていないだけ」ではなく
// 不要と判断した結果）。
type RulerPassWorker struct {
	Enqueuer      jobq.Enqueuer
	Pool          *pgxpool.Pool
	ProgramSearch store.ProgramSearch

	// RetentionGrace は番組終了後、reservations / program_intents を GC するまでの
	// 猶予。0 なら ruler 側の既定値を使う（epg.retention_grace をそのまま流用する
	// 運用を想定。ruler.Config.RetentionGrace のコメント参照）。
	RetentionGrace time.Duration

	// MaxDeletesPerPass は大量削除サーキットブレーカーの閾値。0 なら ruler 側の
	// 既定値を使う（config.yml の ruler.max_deletes_per_pass から注入される。
	// ruler.Config.MaxDeletesPerPass のコメント参照）。
	MaxDeletesPerPass int

	// RetractGrace は放送開始直前にルールから外れた予約を削除しない猶予。0 は無効
	// （config.yml の ruler.retract_grace から注入される。既定 1h は config 層が
	// 埋めるので、ここでの 0 は「明示的に無効化された」を意味する。
	// ruler.Config.RetractGrace のコメント参照）。
	RetractGrace time.Duration
}

// Work は ruler の 1 パス評価を実行する。対象サイトはジョブ引数の 1 サイトのみ
// （ruler.New はサイトのスライスを受け取れるが、ジョブの排他がサイト単位のため
// ここでは常に長さ 1 で渡す）。
func (w *RulerPassWorker) Work(ctx context.Context, args jobs.RulerPassArgs) error {
	reservations := pgstore.NewReservations(w.Pool)
	r := ruler.New([]string{args.Site}, reservations, pgstore.NewBreakers(w.Pool), pgstore.NewNeverScheduledEvents(w.Pool), reservations, &ruler.Config{
		RetentionGrace:    w.RetentionGrace,
		MaxDeletesPerPass: w.MaxDeletesPerPass,
		RetractGrace:      w.RetractGrace,
	}, w.ProgramSearch)
	if err := r.RunPass(ctx); err != nil {
		return err
	}

	// ruler パスの完了は reconcile_pass 起動契機のヒントの 1 つ（docs/recording.md
	// §3.2「ruler パスの完了」）。base が変われば mirakc に反映すべき差分が増えるため、
	// reconcile を早める。epg_sync 完了時の ruler_pass ヒント（epg.go 参照）と同じ形。
	// トランザクション整合の要求はなく、あくまで定期パスを前倒しするヒントなので、
	// Enqueuer が無い場合は投入せず次の定期パスに委ねる。
	if w.Enqueuer != nil {
		if insertErr := enqueueJob(ctx, w.Enqueuer, jobs.ReconcilePassArgs(args)); insertErr != nil {
			slog.Warn("ruler pass: inserting reconcile_pass hint failed", "err", insertErr)
		}
	}

	return nil
}
