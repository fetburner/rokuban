package reservation

import (
	"encoding/json"
	"fmt"
	"time"
)

// SyncReservation は同期評価に使う予約の列を持つ。
type SyncReservation struct {
	ProgramID int64
	Base      json.RawMessage
}

// SyncProgramSnapshot は同期評価に使う番組スナップショットの列を持つ。
type SyncProgramSnapshot struct {
	Title       string
	StartAt     time.Time
	DurationMs  int64
	ChannelType string
	Channel     string
	NetworkID   int32
	ServiceID   int32
	EventID     int32
}

// SyncEvaluationRow は同期候補クエリの 1 行を表す。
type SyncEvaluationRow struct {
	Reservation     SyncReservation
	ProgramSnapshot SyncProgramSnapshot
	IntentAction    *string
	Overrides       json.RawMessage
}

// SyncCandidate は同期評価クエリの 1 行を、解決した実効オプションと skip 判定に組み合わせたもの。
//
// クエリは同期判定前の候補を返す。effective.skip を含む実効オプションの判定を
// 呼び出し側が個別に書くと適用漏れが起きるため、この型と EvaluateSyncCandidates に
// 判定を集約する。
type SyncCandidate struct {
	// Reservation は同期判定に使う予約行の列。
	Reservation SyncReservation
	// Snapshot は番組の事実のスナップショット。Reservation が存在すれば必ず対応する行がある。
	Snapshot SyncProgramSnapshot
	// Options は base / overrides / program_intents.action を合成した実効オプション。
	// Err != nil のときは意味を持たない（ゼロ値）。
	Options Options
	// Skipped は Options.IsSkipped() の結果。Err != nil のときは意味を持たない。
	Skipped bool
	// Err は base / overrides の jsonb が壊れていて EffectiveOptions が失敗した場合のエラー。
	// 握り潰さず、対象行を除外して続けるか処理全体を失敗させるかを呼び出し元が決める。
	Err error
}

// EvaluateSyncCandidates は候補行を EffectiveOptions に通して、同期対象を判定する。
// SQL 側は候補までしか絞らないため、呼び出し側は Skipped を適用して最終判断する。
// 呼び出し元が候補だけを見て絞り込みを自前で再実装すると、絞り込みの半分を書き忘れる見逃しが再発する。
//
// 行ごとの unmarshal エラーは Err に載せて返す（握り潰さない）。
func EvaluateSyncCandidates(rows []SyncEvaluationRow) []SyncCandidate {
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
