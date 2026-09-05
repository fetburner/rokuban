package reservation

import (
	"fmt"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// SyncCandidate は ListReservationsForSyncEvaluation の 1 行を、そこから解決した
// 実効オプションと skip 判定に組み合わせたもの。
//
// クエリ名が約束するのは「同期対象の候補」までで、effective.skip による
// 絞り込みは含まない。呼び出し元が自前でこの 2 段目（EffectiveOptions）を
// 呼ぶ形だと移植漏れが起きうる（shadow-diff がその移植を忘れ、Rokuban が
// 録らない予約を EPGStation と「一致」と誤報告した実例が issue #54）。
// この型と EvaluateSyncCandidates が 2 段目を 1 か所にまとめる。
//
// Reservation と Snapshot を分けて持つのは #27 で番組の事実のスナップショット
// （title / 開始時刻 / 尺 / チャンネル識別）が reservations から program_snapshots
// に抽出されたため。sqlcgen.Reservation はもう ruler の 1 パスの導出出力
// （site / program_id / rule_id / base / dedup 根拠 / timestamps）だけを
// 持つ（CLAUDE.md 不変条件 12）。「番組終了後に schedule が観測されなかった」
// という観測は一時 orphaned_at 列を経たが、issue #98 で recordings の
// 試行行に移設され orphaned_at 自体も廃止された。この型はもともと
// orphaned_at を読んでいなかったので影響を受けない。
type SyncCandidate struct {
	// Reservation は予約行そのもの。
	Reservation sqlcgen.Reservation
	// Snapshot は番組の事実のスナップショット（program_snapshots）。FK により
	// Reservation が存在すれば必ず対応する行がある。
	Snapshot sqlcgen.ProgramSnapshot
	// Options は base / overrides / program_intents.action を合成した実効
	// オプション。Err != nil のときは意味を持たない（ゼロ値）。
	Options Options
	// Skipped は Options.IsSkipped() の結果。Err != nil のときは意味を持たない。
	Skipped bool
	// Err は base / overrides の jsonb が壊れていて EffectiveOptions が失敗した
	// 場合のエラー。呼び出し元の責務が分かれる箇所なのでここでは握り潰さない:
	// reconciler はこの予約だけをログして同期対象から除外し、shadow-diff は
	// 比較全体を失敗させる。挙動が違うため、ここで一方に決め打ちできない。
	Err error
}

// EvaluateSyncCandidates は ListReservationsForSyncEvaluation の結果行それぞれを
// EffectiveOptions に通し、(予約行, スナップショット, 実効オプション, skip 判定)
// の組にして返す。
//
// SQL 側は「候補」までしか絞っていない（issue #98 で never-scheduled
// 除外に置き換わった。internal/db/queries/reservations.sql のコメント参照）。
// 「同期対象か」を最終的に決める effective.skip の絞り込みは、呼び出し元が
// この関数の結果の Skipped で行う。呼び出し元が素の
// ListReservationsForSyncEvaluation の結果だけを見て自前で絞り込みを再実装すると、
// 今回と同じ形の見逃し（絞り込みの半分を書き忘れる）が再発する。
//
// 行ごとの unmarshal エラーは SyncCandidate.Err に載せて返す（握り潰さない）。
// どう扱うか（該当行だけ除外するか、全体を失敗させるか）は呼び出し元に委ねる。
func EvaluateSyncCandidates(rows []sqlcgen.ListReservationsForSyncEvaluationRow) []SyncCandidate {
	candidates := make([]SyncCandidate, 0, len(rows))
	for _, row := range rows {
		opts, err := EffectiveOptions(row.Reservation.Base, row.Overrides, row.IntentAction)
		if err != nil {
			candidates = append(candidates, SyncCandidate{
				Reservation: row.Reservation,
				Snapshot:    row.ProgramSnapshot,
				Err: fmt.Errorf("resolving effective options for program %d: %w",
					row.Reservation.ProgramID, err),
			})
			continue
		}
		candidates = append(candidates, SyncCandidate{
			Reservation: row.Reservation,
			Snapshot:    row.ProgramSnapshot,
			Options:     opts,
			Skipped:     opts.IsSkipped(),
		})
	}
	return candidates
}
