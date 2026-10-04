package capacity

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ProgramRef は rulequery.MatchPrograms の結果 1 行を指す。同じ放送が 2 つの site に射影されると
// 独立した 2 つの予約が生まれるので、site も同一性の一部である。
type ProgramRef struct {
	Site      string
	ProgramID int64
}

// PreviewCandidate は検索で一致した番組の識別子と容量需要を並べて持つ。呼び出し側が
// 需要を射影する前に ruler 共有の dedupe 評価を掛けられるようにするため。
type PreviewCandidate struct {
	ProgramID    int64
	IntentAction *string
	Demand
}

// PreviewCandidates は、ルール保存で新たな予約になりうる検索一致を返す。既存の予約は除くが、
// 編集中のルール（ruleID）が今持つ予約は仮想的に外して再計算するので残す。skip 意図・取り込み
// 済みの番組・never-scheduled・放送済み（終了 <= now()）は ruler / reconciler と同様に除く。
// record 意図は dedupe の skip を覆せるので残す。チャンネル識別はルールコンパイラの service
// JOIN と同じく epg_services から引く。
func PreviewCandidates(ctx context.Context, pool *pgxpool.Pool, refs []ProgramRef, ruleID *int64) ([]PreviewCandidate, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	sites := make([]string, len(refs))
	programIDs := make([]int64, len(refs))
	for i, ref := range refs {
		sites[i] = ref.Site
		programIDs[i] = ref.ProgramID
	}
	rows, err := sqlcgen.New(pool).ListCapacityPreviewCandidates(ctx, sqlcgen.ListCapacityPreviewCandidatesParams{
		Sites: sites, ProgramIds: programIDs, RuleID: ruleID,
	})
	if err != nil {
		return nil, fmt.Errorf("listing capacity preview candidates: %w", err)
	}
	candidates := make([]PreviewCandidate, len(rows))
	for i, r := range rows {
		candidates[i] = PreviewCandidate{
			ProgramID:    r.ProgramID,
			IntentAction: r.IntentAction,
			Demand: Demand{Site: r.Site, ChannelType: r.ChannelType, Channel: r.Channel,
				StartAt: r.StartAt, EndAt: r.EndAt},
		}
	}
	return candidates, nil
}

// Preview は LoadAllSites と同じ現在の需要とチューナー射影を読み、適格な候補を足したときに
// 新たに容量超過になる区間を計算する。編集中のルールでは、その現行予約を先に仮想需要から外す。
func Preview(ctx context.Context, q *sqlcgen.Queries, ruleID *int64, candidates []Demand) ([]Overage, error) {
	rows, err := q.ListCapacityDemandAllSites(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing capacity preview baseline: %w", err)
	}
	current, withoutRule, err := demandsFromAllSiteRows(rows, ruleID)
	if err != nil {
		return nil, fmt.Errorf("resolving capacity preview baseline: %w", err)
	}

	tunerRows, err := q.ListTunerSyncAllSites(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing tuner projection for capacity preview: %w", err)
	}
	tuners := tunersFromRows(tunerRows)
	currentOverages := Compute(current, tuners)

	after := make([]Demand, 0, len(withoutRule)+len(candidates))
	after = append(after, withoutRule...)
	after = append(after, candidates...)
	newOverages := Compute(after, tuners)
	return NewlyAdded(currentOverages, newOverages), nil
}

// NewlyAdded は、after の区間のうち、同じかより大きい不足数で覆う before の区間を引く。
// 不足が悪化した区間は古い超過と重なっていても新規として残る。JammedTypes は覆いの判定に
// 使わない（その区間の証明の選び方であり、深刻度の比較は不足数で行う）。
func NewlyAdded(before, after []Overage) []Overage {
	ordered := append([]Overage(nil), after...)
	slices.SortFunc(ordered, func(a, b Overage) int {
		if a.Site != b.Site {
			if a.Site < b.Site {
				return -1
			}
			return 1
		}
		return a.StartAt.Compare(b.StartAt)
	})

	var out []Overage
	for _, post := range ordered {
		var covering []Overage
		for _, pre := range before {
			if pre.Site != post.Site || pre.Shortfall < post.Shortfall ||
				!pre.StartAt.Before(post.EndAt) || !pre.EndAt.After(post.StartAt) {
				continue
			}
			covering = append(covering, pre)
		}
		slices.SortFunc(covering, func(a, b Overage) int { return a.StartAt.Compare(b.StartAt) })

		cursor := post.StartAt
		for _, cover := range covering {
			coverStart := maxTime(cover.StartAt, post.StartAt)
			coverEnd := minTime(cover.EndAt, post.EndAt)
			if cursor.Before(coverStart) {
				out = appendOverageSegment(out, post, cursor, coverStart)
			}
			if coverEnd.After(cursor) {
				cursor = coverEnd
			}
			if !cursor.Before(post.EndAt) {
				break
			}
		}
		if cursor.Before(post.EndAt) {
			out = appendOverageSegment(out, post, cursor, post.EndAt)
		}
	}
	return out
}

func appendOverageSegment(out []Overage, source Overage, start, end time.Time) []Overage {
	if !end.After(start) {
		return out
	}
	segment := source
	segment.StartAt = start
	segment.EndAt = end
	segment.JammedTypes = slices.Clone(source.JammedTypes)
	if len(out) > 0 {
		last := &out[len(out)-1]
		if last.Site == segment.Site && last.EndAt.Equal(segment.StartAt) &&
			last.Shortfall == segment.Shortfall && slices.Equal(last.JammedTypes, segment.JammedTypes) {
			last.EndAt = segment.EndAt
			return out
		}
	}
	return append(out, segment)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
