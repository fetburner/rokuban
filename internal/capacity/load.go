package capacity

import (
	"context"
	"fmt"

	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// Load は store から需要とチューナー射影を読み、超過区間の結合済みリストを返す。
//
// 地平線全体を 1 回解く（窓で切らない。docs/data.md §6.5）。予約集合は ruler の GC で
// 既に有界なので、範囲検索は結果の上で Intersecting に落とす。
//
// site は判定の分割キー。worker/tuner.go の定期ジョブは束縛サイト 1 つ分だけを
// 扱えばよいのでこれを使う。api（GET /api/capacity/overages）は site に束縛
// されない（不変条件 1）ので LoadAllSites を使う（issue #184 M4-12）。
// Compute 側はいずれの経路でもサイトごとに独立に判定する形を保っている
// （N 予約の決定に依存した性質。docs/data.md §6.5「判定はサイトごとに
// 独立して行う」）。
func Load(ctx context.Context, storage store.Capacity, site string) ([]Overage, error) {
	input, err := storage.ReadSite(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("reading capacity input for site %s: %w", site, err)
	}
	demands, _, err := demandsFromRows(input.Demands, nil)
	if err != nil {
		return nil, err
	}
	return Compute(demands, tunersFromStore(input.Tuners)), nil
}

// LoadAllSites は Load の全サイト版。GET /api/capacity/overages が使う
// （issue #184 M4-12）。
func LoadAllSites(ctx context.Context, storage store.Capacity) ([]Overage, error) {
	input, err := storage.ReadAllSites(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading capacity input for all sites: %w", err)
	}
	demands, _, err := demandsFromRows(input.Demands, nil)
	if err != nil {
		return nil, err
	}
	return Compute(demands, tunersFromStore(input.Tuners)), nil
}

func tunersFromStore(rows []store.Tuner) []Tuner {
	tuners := make([]Tuner, 0, len(rows))
	for _, r := range rows {
		tuners = append(tuners, Tuner{
			Site:        r.Site,
			Types:       r.Types,
			IsAvailable: r.IsAvailable,
			IsFault:     r.IsFault,
		})
	}
	return tuners
}

func demandsFromRows(rows []store.CapacityDemand, excludeRuleID *int64) ([]Demand, []Demand, error) {
	all := make([]Demand, 0, len(rows))
	withoutRule := make([]Demand, 0, len(rows))
	for _, r := range rows {
		d, ok, err := demandFromRow(r)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue
		}
		all = append(all, d)
		if excludeRuleID == nil || r.RuleID == nil || *r.RuleID != *excludeRuleID {
			withoutRule = append(withoutRule, d)
		}
	}
	return all, withoutRule, nil
}

// demandFromRow は store の容量需要 1 行を Demand に変換する。
// effective.skip な行は ok=false を返す（呼び出し元は捨てる）。
//
// program_snapshots のチャンネル識別列は issue #101 で NOT NULL 化
// された。NULL を仮定した nil ガードはここにあったが、その状態自体が表現
// 不可能になったため落とした（起きない状態のための分岐を残さない）。
func demandFromRow(row store.CapacityDemand) (Demand, bool, error) {
	eff, err := reservation.EffectiveOptions(row.Base, row.Overrides, row.IntentAction)
	if err != nil {
		return Demand{}, false, fmt.Errorf("resolving effective options for a reservation on %s: %w", row.Site, err)
	}
	if eff.IsSkipped() {
		return Demand{}, false, nil
	}
	return Demand{
		Site:        row.Site,
		ChannelType: row.ChannelType,
		Channel:     row.Channel,
		StartAt:     row.StartAt,
		EndAt:       row.EndAt,
	}, true, nil
}
