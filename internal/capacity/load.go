package capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// Load は DB から需要とチューナー射影を読み、超過区間の結合済みリストを返す。
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
func Load(ctx context.Context, q *sqlcgen.Queries, site string) ([]Overage, error) {
	demands, err := loadDemand(ctx, q, site)
	if err != nil {
		return nil, err
	}

	rows, err := q.ListTunerSync(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing tuner projection for site %s: %w", site, err)
	}
	return Compute(demands, tunersFromRows(rows)), nil
}

// LoadAllSites は Load の全サイト版。GET /api/capacity/overages が使う
// （issue #184 M4-12）。
func LoadAllSites(ctx context.Context, q *sqlcgen.Queries) ([]Overage, error) {
	demands, err := loadDemandAllSites(ctx, q)
	if err != nil {
		return nil, err
	}

	rows, err := q.ListTunerSyncAllSites(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing tuner projection for all sites: %w", err)
	}
	return Compute(demands, tunersFromRows(rows)), nil
}

func tunersFromRows(rows []sqlcgen.TunerSync) []Tuner {
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

// loadDemand は需要になる予約だけを読み出す。
//
// SQL 側で never-scheduled 除外（issue #98。旧 state <> 'orphaned'）と
// チャンネルスナップショットの有無を絞り、effective.skip はここ（Go 側）で
// db.EffectiveOptions を通して判定する ---
// base / overrides の jsonb マージと program_intents.action の解決が要るため
// （不透明な overrides を SQL で読まない、という既存の規律。
// internal/api/reservations_overlaps.go と同じ分担）。
//
// **数えるのは reconciler が実際に schedule を作る予約だけ**（issue #21 の実装メモ）。
// skip が立っている予約は schedule が作られないのでチューナーを消費しない。
func loadDemand(ctx context.Context, q *sqlcgen.Queries, site string) ([]Demand, error) {
	rows, err := q.ListCapacityDemand(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing capacity demand for site %s: %w", site, err)
	}

	demands := make([]Demand, 0, len(rows))
	for _, r := range rows {
		d, ok, err := demandFromRow(r.Site, r.ChannelType, r.Channel, r.ProgramStartAt, r.ProgramEndAt, r.Base, r.Overrides, r.IntentAction)
		if err != nil {
			return nil, err
		}
		if ok {
			demands = append(demands, d)
		}
	}
	return demands, nil
}

// loadDemandAllSites は loadDemand の全サイト版（issue #184 M4-12）。
func loadDemandAllSites(ctx context.Context, q *sqlcgen.Queries) ([]Demand, error) {
	rows, err := q.ListCapacityDemandAllSites(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing capacity demand for all sites: %w", err)
	}

	demands := make([]Demand, 0, len(rows))
	for _, r := range rows {
		d, ok, err := demandFromRow(r.Site, r.ChannelType, r.Channel, r.ProgramStartAt, r.ProgramEndAt, r.Base, r.Overrides, r.IntentAction)
		if err != nil {
			return nil, err
		}
		if ok {
			demands = append(demands, d)
		}
	}
	return demands, nil
}

// demandFromRow は ListCapacityDemand(AllSites) の 1 行を Demand に変換する。
// effective.skip な行は ok=false を返す（呼び出し元は捨てる）。
//
// program_snapshots のチャンネル識別列は issue #101 で NOT NULL 化
// された。NULL を仮定した nil ガードはここにあったが、その状態自体が表現
// 不可能になったため落とした（起きない状態のための分岐を残さない）。
func demandFromRow(site, channelType, channel string, startAt, endAt time.Time, base, overrides json.RawMessage, intentAction *string) (Demand, bool, error) {
	eff, err := db.EffectiveOptions(base, overrides, intentAction)
	if err != nil {
		return Demand{}, false, fmt.Errorf("resolving effective options for a reservation on %s: %w", site, err)
	}
	if eff.IsSkipped() {
		return Demand{}, false, nil
	}
	return Demand{
		Site:        site,
		ChannelType: channelType,
		Channel:     channel,
		StartAt:     startAt,
		EndAt:       endAt,
	}, true, nil
}
