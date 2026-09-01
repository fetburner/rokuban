package epgimport

import (
	"context"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
)

// HistoryItem は EPGStation の RecordedHistory 1 行（id/name/channelId/endAt
// の 4 列。l3tnun/EPGStation src/db/entities/RecordedHistory.ts で確認。
// REST API には公開されていないため、運用者が EPGStation の DB から直接
// SELECT して書き出す JSON を入力にする。--library-json と同じ理由
// （internal/epgimport/library.go のパッケージコメント）に加え、
// RecordedHistory 自体に REST エンドポイントが無いという制約もある）。
type HistoryItem struct {
	ChannelID int64 `json:"channelId"`
	// ChannelType は LibraryItem と同じ理由で運用者が埋める。
	ChannelType string `json:"channelType"`
	Name        string `json:"name"`
	EndAt       int64  `json:"endAt"` // UnixtimeMS
}

// HistoryImportResult は ImportHistory の結果。
type HistoryImportResult struct {
	Registered int
	Warnings   []string
}

// ImportHistory は EPGStation の RecordedHistory を rokuban の recordings
// （tombstone。media_assets は持たない）へ取り込み、再放送重複排除の種にする
// （issue #72「履歴: RecordedHistory → 再放送重複排除の種」）。
//
// **設計判断・要確認（issue が決めていない）**: rokuban の重複排除
// （internal/ruler/dedupe.go）は `recordings.rule_id = <候補番組を勝ち取った
// ルールの id>` が一致する行だけを比較対象にする（同じルールが同じ番組
// シリーズを指しているという前提）。EPGStation の RecordedHistory は
// ruleId を一切持たない（4 列だけの薄いテーブル）ため、どの rokuban
// ルールに帰属させるべきか特定できない。in-place 登録（internal/inplace.
// Register が使う UpsertInPlaceRecording）自体も rule_id を書く列を
// 持たない（in-place はそもそも ruler の外から来た録画という設計）。
//
// そのため、ここで作る recordings 行は **rule_id = NULL** になる。現在の
// dedupe SQL は `rec.rule_id = c.rule_id` の等値結合なので、rule_id が
// NULL の行は絶対にマッチせず、重複排除は機能しない —— 「再放送重複排除の
// 種」という issue の目的をこの実装は完全には満たせていない。
//
// 一方で、これは新種の失敗モードではない: 同じ資料（docs/recording/
// ruler.md「ルールの削除は履歴のスコープを消す」）が、ルール削除で
// スコープが一度リセットされても新ルールで 1 本録れれば以降また弾かれる、
// という一過性の過剰録画は許容する設計だと明言している。history import
// で rule_id が付かないことの実害も同じ形（新ルールが対象シリーズで
// 1 本録れた時点で、その録画が新しい抑制元になり以降は弾かれる）に収まる。
//
// この行はそれでも意味を持つ（不変条件 10）: 「この番組は放送済みで録画歴
// がある」という事実そのものは残るため、将来 dedupe.go 側で rule_id が
// NULL の履歴も候補にする拡張（internal/ruler は対象外パッケージなので
// このタスクでは行っていない）を入れれば、追加の import をせずそのまま
// 効くようになる。
func ImportHistory(ctx context.Context, q *sqlcgen.Queries, site string, items []HistoryItem) (HistoryImportResult, error) {
	var res HistoryImportResult
	for _, item := range items {
		networkID, serviceID := mirakc.SplitServiceID(item.ChannelID)
		eventID := syntheticEventID(item.Name, item.EndAt)

		channelType := item.ChannelType
		if channelType == "" {
			channelType = "GR"
			res.Warnings = append(res.Warnings, fmt.Sprintf("history %q has no channelType — defaulted to GR", item.Name))
		}

		// 冪等性チェック: この行はできた瞬間に tombstone にする（下記）ので、
		// recordings_unique_active_event（deleted_at IS NULL の部分索引）に
		// 乗らない。ON CONFLICT で任せると再実行のたびに新しい行ができて
		// しまう（実測して見つけた。TestImportHistory_IdempotentRerun）ため、
		// deleted_at を問わず先に自分で引く。
		if _, err := q.FindRecordingByEventAnyState(ctx, sqlcgen.FindRecordingByEventAnyStateParams{
			Site: site, NetworkID: int32(networkID), ServiceID: int32(serviceID), EventID: eventID,
		}); err == nil {
			res.Registered++
			continue
		} else if !isNoRows(err) {
			return res, fmt.Errorf("looking up history %q: %w", item.Name, err)
		}

		// EPGStation の RecordedHistory は開始時刻・長さを持たない
		// （endAt のみ）。program_start_at/program_duration_ms は NOT NULL
		// なので、durationMs=0 の「終了時刻を開始時刻とみなす」フォールバック
		// にする。dedupe は program_start_at を dedupe_window の窓判定にしか
		// 使わない（internal/ruler/dedupe.go）ので、時刻がわずかにずれても
		// 「窓に入るかどうか」以上の実害はない。
		recordingID, err := q.UpsertInPlaceRecording(ctx, sqlcgen.UpsertInPlaceRecordingParams{
			Source:            db.SourceManual,
			Site:              site,
			NetworkID:         int32(networkID),
			ServiceID:         int32(serviceID),
			EventID:           eventID,
			ServiceName:       "unknown",
			ChannelType:       channelType,
			Channel:           "unknown",
			Title:             item.Name,
			ProgramStartAt:    time.UnixMilli(item.EndAt),
			ProgramDurationMs: 0,
			Status:            db.RecordingStatusFinished,
		})
		if err != nil {
			return res, fmt.Errorf("upserting history %q: %w", item.Name, err)
		}
		// tombstone として登録する（ファイル実体を rokuban が一度も管理して
		// いないため、通常のライブラリ一覧には出さない。ごみ箱契約と同じ
		// 「deleted_at が立っていても重複排除は機能する」を利用する）。
		if _, err := q.SoftDeleteRecording(ctx, recordingID); err != nil {
			return res, fmt.Errorf("tombstoning history %q: %w", item.Name, err)
		}
		res.Registered++
	}
	return res, nil
}
