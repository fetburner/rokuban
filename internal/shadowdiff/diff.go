// Package shadowdiff は Rokuban と EPGStation の予約集合を突き合わせる
// 軽量シャドー差分（issue #6 / #24 の M2-14）。DB ともネットワークとも
// 一切やり取りしない純関数として提供し、呼び出し側（cmd/rokuban）が
// 両者から集めたデータを渡す形にする。
package shadowdiff

import (
	"sort"
	"time"

	"github.com/fetburner/rokuban/internal/epgstation"
)

// RokubanReservation は Rokuban 側の予約 1 件。呼び出し側が DB から組み立てる。
type RokubanReservation struct {
	ProgramID int64
	Title     string
	StartAt   time.Time
	// Skipped は program_intents.action = 'skip' の意図があること（= reconciler が
	// schedule を作らない）を示す。skip 意図は reservations 行を持たない
	// （issue #18 の案 A）ので、呼び出し側は program_intents を別途引いて
	// Skipped: true の行として混ぜ込む。
	Skipped bool
}

// Item は差分レポートの 1 件。
type Item struct {
	ProgramID int64
	Title     string
	StartAt   time.Time
	// Reason は Expected 分類のときだけ埋まる、差分が説明可能である理由。
	Reason string
}

// Report は Compare の結果。4 分類に分けて持つ。
type Report struct {
	// Both は両方にある（一致）。
	Both []Item
	// RokubanOnly は Rokuban だけにある、説明のつかない差分。
	RokubanOnly []Item
	// EPGStationOnly は EPGStation だけにある、説明のつかない差分。
	EPGStationOnly []Item
	// Expected は差分だが allowlist で説明可能なもの。
	Expected []Item
}

// HasUnexplained は説明できない差分（RokubanOnly か EPGStationOnly）が
// 1 件でもあるかを返す。CLI の終了コード判定に使う。
func (r Report) HasUnexplained() bool {
	return len(r.RokubanOnly) > 0 || len(r.EPGStationOnly) > 0
}

// Compare は Rokuban と EPGStation の予約集合を programId で突き合わせて
// Report を組み立てる。
//
// 照合キーは programId のみ。時刻や題名は使わない（EPGStation の
// isHalfWidth 変換で題名が変わりうるため、docs 記載の通り）。
//
// allowlist（Expected に落とす条件。docs/recording.md §9 と issue #18 の案 A に対応）:
//   - EPGStation 側が IsTimeSpecified または ProgramID == nil
//     → Rokuban に時刻指定予約の機能はなく、programId を持たないので照合もできない
//   - EPGStation 側が IsSkip → EPGStation 側でユーザーが除外した予約
//   - EPGStation 側が IsOverlap → EPGStation の重複排除で除外された予約
//   - Rokuban 側が Skipped（program_intents.action = 'skip'）→ Rokuban で除外した予約
//
// IsConflict は意図的に allowlist へ入れない。チューナー競合は両者で起きうる条件が
// 同じはずで、片方だけにあるなら説明が要る。
func Compare(rokuban []RokubanReservation, epg []epgstation.Reserve) Report {
	active := make(map[int64]RokubanReservation)
	skipped := make(map[int64]RokubanReservation)
	for _, r := range rokuban {
		if r.Skipped {
			skipped[r.ProgramID] = r
		} else {
			active[r.ProgramID] = r
		}
	}

	var report Report

	// programId を持たない（または時刻指定の）EPGStation 予約は、そもそも
	// programId で照合しようがないので個別に Expected へ落とす。
	epgByProgramID := make(map[int64]epgstation.Reserve)
	for _, e := range epg {
		if e.ProgramID == nil || e.IsTimeSpecified {
			report.Expected = append(report.Expected, Item{
				ProgramID: 0,
				Title:     e.Name,
				StartAt:   e.StartAtTime(),
				Reason:    "時刻指定予約は programId を持たないので照合できない",
			})
			continue
		}
		epgByProgramID[*e.ProgramID] = e
	}

	seen := make(map[int64]bool, len(active)+len(epgByProgramID))
	for id := range active {
		seen[id] = true
	}
	for id := range epgByProgramID {
		seen[id] = true
	}

	for id := range seen {
		a, inRokuban := active[id]
		e, inEPG := epgByProgramID[id]

		switch {
		case inRokuban && inEPG:
			report.Both = append(report.Both, Item{
				ProgramID: id,
				Title:     a.Title,
				StartAt:   a.StartAt,
			})
		case inRokuban && !inEPG:
			report.RokubanOnly = append(report.RokubanOnly, Item{
				ProgramID: id,
				Title:     a.Title,
				StartAt:   a.StartAt,
			})
		case !inRokuban && inEPG:
			if s, ok := skipped[id]; ok {
				report.Expected = append(report.Expected, Item{
					ProgramID: id,
					Title:     s.Title,
					StartAt:   s.StartAt,
					Reason:    "Rokuban 側でこの番組を除外している（program_intents.action = 'skip'）ため、EPGStation 側にのみ存在するのは想定通り",
				})
			} else if e.IsSkip {
				report.Expected = append(report.Expected, Item{
					ProgramID: id,
					Title:     e.Name,
					StartAt:   e.StartAtTime(),
					Reason:    "EPGStation 側で除外された予約（isSkip）",
				})
			} else if e.IsOverlap {
				report.Expected = append(report.Expected, Item{
					ProgramID: id,
					Title:     e.Name,
					StartAt:   e.StartAtTime(),
					Reason:    "EPGStation の重複排除で除外された予約（isOverlap）",
				})
			} else {
				report.EPGStationOnly = append(report.EPGStationOnly, Item{
					ProgramID: id,
					Title:     e.Name,
					StartAt:   e.StartAtTime(),
				})
			}
		}
	}

	sortItems(report.Both)
	sortItems(report.RokubanOnly)
	sortItems(report.EPGStationOnly)
	sortItems(report.Expected)

	return report
}

// sortItems は開始時刻・programId の順で安定的に並べ替える。
// レポートの出力順とテストの再現性を確保するため。
func sortItems(items []Item) {
	sort.Slice(items, func(i, j int) bool {
		if !items[i].StartAt.Equal(items[j].StartAt) {
			return items[i].StartAt.Before(items[j].StartAt)
		}
		return items[i].ProgramID < items[j].ProgramID
	})
}
