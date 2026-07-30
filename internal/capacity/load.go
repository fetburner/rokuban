package capacity

import (
	"context"
	"fmt"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// Load は DB から需要とチューナー射影を読み、超過区間の結合済みリストを返す。
//
// 地平線全体を 1 回解く（窓で切らない。docs/data.md §6.5）。予約集合は ruler の GC で
// 既に有界なので、範囲検索は結果の上で Intersecting に落とす。
//
// site は判定の分割キー。現状の設定（config.mirakc.site）は単一サイトなので
// 呼び出し側はその 1 つを渡すが、Compute 側はサイトごとに独立に判定する形を
// 保っている（N 予約の決定に依存した性質。docs/data.md §6.5「判定はサイトごとに
// 独立して行う」）。
func Load(ctx context.Context, q *sqlcgen.Queries, site string) ([]Overage, error) {
	demands, err := LoadDemand(ctx, q, site)
	if err != nil {
		return nil, err
	}

	rows, err := q.ListTunerSync(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing tuner projection for site %s: %w", site, err)
	}
	tuners := make([]Tuner, 0, len(rows))
	for _, r := range rows {
		tuners = append(tuners, Tuner{
			Site:        r.Site,
			Types:       r.Types,
			IsAvailable: r.IsAvailable,
			IsFault:     r.IsFault,
		})
	}

	return Compute(demands, tuners), nil
}

// LoadDemand は需要になる予約だけを読み出す。
//
// SQL 側で state <> 'orphaned' とチャンネルスナップショットの有無を絞り、
// effective.skip はここ（Go 側）で db.EffectiveOptions を通して判定する ---
// base / overrides の jsonb マージと program_intents.action の解決が要るため
// （不透明な overrides を SQL で読まない、という既存の規律。
// internal/api/reservations_overlaps.go と同じ分担）。
//
// **数えるのは reconciler が実際に schedule を作る予約だけ**（issue #21 の実装メモ）。
// skip が立っている予約は schedule が作られないのでチューナーを消費しない。
func LoadDemand(ctx context.Context, q *sqlcgen.Queries, site string) ([]Demand, error) {
	rows, err := q.ListCapacityDemand(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing capacity demand for site %s: %w", site, err)
	}

	demands := make([]Demand, 0, len(rows))
	for _, r := range rows {
		eff, err := db.EffectiveOptions(r.Base, r.Overrides, r.IntentAction)
		if err != nil {
			return nil, fmt.Errorf("resolving effective options for a reservation on %s: %w", r.Site, err)
		}
		if eff.IsSkipped() {
			continue
		}
		// SQL の WHERE で NOT NULL を保証しているが、sqlc は nullable な列を
		// ポインタで生成するのでここでも落とす（クエリの条件が緩んだときに
		// nil デリファレンスで落ちるより、需要に数えない方が安全側）。
		if r.ChannelType == nil || r.Channel == nil {
			continue
		}
		demands = append(demands, Demand{
			Site:        r.Site,
			ChannelType: *r.ChannelType,
			Channel:     *r.Channel,
			StartAt:     r.ProgramStartAt,
			EndAt:       r.ProgramEndAt,
		})
	}
	return demands, nil
}
