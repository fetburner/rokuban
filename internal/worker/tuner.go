package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/capacity"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
)

// tunerSyncTimeout は全量同期 1 パスの上限。
//
// /api/tuners は数本〜十数本を返すだけなので所要は EPG と比べ物にならないが、
// mirakc が無応答のままジョブを掴み続けるのを避けるため上限は置く
// （EpgSyncWorker.Timeout と同じ姿勢）。
const tunerSyncTimeout = time.Minute

// TunerSyncArgs はチューナー射影ジョブの引数。
type TunerSyncArgs struct {
	Site string `json:"site"`
}

// Kind は River ジョブの種別名を返す。
func (TunerSyncArgs) Kind() string { return "tuner_sync" }

// InsertOpts は River ジョブの挿入オプションを返す。
//
// キューは epg_sync と同じ epg（a.Site で修飾。physicalQueueName、issue #185
// M4-13。必ず physicalQueueName を経由する --- qualifyQueueName のコメント参照）。
// どちらも「使い捨てプロジェクションの全量同期」で性質が同じであり、
// MaxWorkers 1 が既に重なりを防いでいる。チューナー構成の変更は
// mirakc の再起動を要するので更新頻度は低くてよく、EPG 同期の後ろで待たされても
// 実害がない（キューを増やすと worker.queues の設定面が広がる分だけ損）。
// EpgSyncArgs.InsertOpts と同じ修飾規則を使うこと --- 片方だけ修飾すると
// MaxWorkers: 1 による同時実行の抑制が site 単位に分かれて崩れる。
//
// ByState を pendingJobStates に絞る理由は EpgSyncArgs.InsertOpts と同じ
// （River の既定は completed を含むため、定期ジョブが実質ワンショットになる）。
// ByQueue: uniqueByQueue の理由は pendingJobStates 直後の doc コメント参照。
func (a TunerSyncArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: physicalQueueName(epgQueue, a.Site),
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: uniqueByQueue,
			ByState: pendingJobStates,
		},
	}
}

// TunerSyncWorker は mirakc の /api/tuners を tuner_sync に全量投影する River ワーカー。
//
// EpgSyncWorker と同じ使い捨てプロジェクション（真実は常に mirakc 側）なので、
// 差分同期はせず毎回の全量ポーリング + スイープでレベルトリガーに収束させる。
// 存在理由は不変条件 1（api ロールは mirakc に問い合わせない）で、
// 容量判定（internal/capacity）はこの射影だけを読む。
type TunerSyncWorker struct {
	river.WorkerDefaults[TunerSyncArgs]
	MirakcClient *mirakc.Client
	Pool         *pgxpool.Pool

	// Site はこのワーカープロセス自身の site（config.mirakc.site）。Work は
	// これと job.Args.Site を verifySite で照合してから mirakc に触る
	// （issue #139）。TunerSyncArgs は EpgSyncArgs と同じ epg キューを使う
	// 「使い捨てプロジェクションの全量同期」で、mirakc への ListTuners を伴う
	// ため EpgSyncWorker と同じ理由でガードが要る。空なら db.DefaultSite に
	// 解決する（verifySite 参照）。
	Site string
}

// Timeout は River の既定（1 分）と同じ上限を明示する。
func (w *TunerSyncWorker) Timeout(*river.Job[TunerSyncArgs]) time.Duration {
	return tunerSyncTimeout
}

// Work はチューナーの全量同期を 1 パス実行する。
// upsert 後、今回観測しなかった行を削除する。
func (w *TunerSyncWorker) Work(ctx context.Context, job *river.Job[TunerSyncArgs]) error {
	site := job.Args.Site
	log := slog.With("site", site)

	// mirakc インスタンスはサイトスコープ。他サイトのジョブをこのプロセスの
	// mirakc に投げると、別インスタンスのチューナー構成をこのサイトの投影として
	// 書きうる（issue #139）。ListTuners より前に照合する。
	if err := verifySite(w.Site, site, epgQueue); err != nil {
		return err
	}

	q := sqlcgen.New(w.Pool)

	// スイープ基準は upsert より前の DB 時刻（EpgSweepMark と同じ理由。
	// クロックスキューで射影全体を消す事故を防ぐ）。
	mark, err := q.TunerSweepMark(ctx)
	if err != nil {
		return fmt.Errorf("getting sweep mark: %w", err)
	}

	tuners, err := w.MirakcClient.ListTuners(ctx)
	if err != nil {
		return fmt.Errorf("listing tuners: %w", err)
	}

	params := make([]sqlcgen.UpsertTunerSyncParams, 0, len(tuners))
	for _, t := range tuners {
		types, ok := projectableTunerTypes(t)
		if !ok {
			// tuner_sync.types の CHECK 制約（types <@ ARRAY['GR','BS','CS','SKY']）に
			// 引っかかる値があると、そのチューナーを含むバッチごと失敗する。
			// 未知の種別を持つチューナーだけ捨てて同期は続行する
			// （epg_services が未知の channel_type のサービスだけ捨てるのと同じ規律。
			// validChannelTypes のコメント参照）。cap(A) は 1 本少なく数えられるので
			// 警告が過剰に出る方向にずれるが、同期パス全体を失敗させて射影が
			// 丸ごと陳腐化する方が損である。
			log.Warn("tuner sync: skipping tuner with unknown channel type",
				"tuner_index", t.Index, "name", t.Name, "types", t.Types)
			continue
		}
		params = append(params, sqlcgen.UpsertTunerSyncParams{
			Site:        site,
			TunerIndex:  int32(t.Index),
			Name:        t.Name,
			Types:       types,
			IsAvailable: t.IsAvailable,
			IsFault:     t.IsFault,
		})
	}

	if len(params) > 0 {
		if err := execBatch(q.UpsertTunerSync(ctx, params)); err != nil {
			return fmt.Errorf("upserting tuners: %w", err)
		}
	}

	// 空レスポンスでスイープを走らせると射影が消え、容量判定が「チューナーを 1 本も
	// 知らない」状態に落ちる。そのとき capacity.Compute は**何も主張しない**ので
	// 警告が黙って消える（洪水にはならないが、見逃しになる）。EpgSyncWorker が
	// 空レスポンスでスイープを見送るのと同じ理由で、削除は見送って次のパスに委ねる。
	var stale int64
	if len(params) == 0 {
		log.Warn("tuner sync: mirakc returned no projectable tuners, skipping sweep",
			"tuners_fetched", len(tuners))
	} else if stale, err = q.DeleteStaleTunerSync(ctx, sqlcgen.DeleteStaleTunerSyncParams{
		Site:       site,
		ObservedAt: mark,
	}); err != nil {
		return fmt.Errorf("deleting stale tuners: %w", err)
	}

	metrics.TunersProjected.WithLabelValues(site).Set(float64(len(params)))
	metrics.TunerSyncLastSuccess.WithLabelValues(site).SetToCurrentTime()

	// 容量超過区間のゲージは射影が変わったこのパスで入れ直す。予約側の変更でも
	// 値は変わりうるが、このゲージは「構成の余裕を眺める」ためのもの（アラートの
	// 一次情報ではない。metrics.CapacityOverages のコメント参照）なので、
	// 同期間隔の鮮度で足りる。UI が読むのは常に API 側の再計算結果。
	overages := -1
	if list, err := capacity.Load(ctx, q, site); err != nil {
		// 射影の同期そのものは成功しているので、ここでジョブを失敗させて
		// リトライさせる意味はない（次のパスが入れ直す）。ゲージも更新しない
		// （0 を入れると「超過なし」と区別できず、判定を黙って無効化してしまう。
		// metrics.BacklogCollector が失敗時に 0 を報告しないのと同じ理由）。
		log.Warn("tuner sync: computing capacity overages failed", "err", err)
	} else {
		overages = len(list)
		metrics.CapacityOverages.WithLabelValues(site).Set(float64(overages))
	}

	log.Info("tuner sync complete",
		"tuners_fetched", len(tuners),
		"tuners_projected", len(params),
		"stale", stale,
		"capacity_overages", overages,
	)
	return nil
}

// projectableTunerTypes はチューナーの types を tuner_sync に載せられる形で返す。
//
// 未知の種別が 1 つでも混ざっていたら投影しない（false を返す）。未知のものだけを
// 落として残りを投影する形にはしない --- そのチューナーは実際には落とした種別も
// 掴めるので、cap(A) を「対応していないチューナー」として数えることになり、
// **警告を見逃す**方向（docs/data.md §6.5 が禁じている向き）に誤る。
//
// types が空配列のチューナーは投影する。どの cap(A) にも数えられないだけで無害
// （internal/db/queries/tuner_sync.sql のコメント参照）。
func projectableTunerTypes(t mirakc.Tuner) ([]string, bool) {
	types := make([]string, 0, len(t.Types))
	for _, name := range t.Types {
		if !validChannelTypes[name] {
			return nil, false
		}
		types = append(types, name)
	}
	return types, true
}
