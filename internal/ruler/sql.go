package ruler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// rulerInputRow は upsertReservationsFromPass に渡す 1 行分の入力。
// jsonb_to_recordset で受け取るため、フィールド名は SQL 側の列名に合わせた
// snake_case の json タグを持つ（reservations.base の camelCase 規約とは無関係）。
//
// RuleID が nil の行は「勝者ルールなし」（program_intents だけで desired）を表し、
// SQL 側で base を凍結する（ruler は上書きしない）。
//
// 番組の事実のスナップショット（title / 開始時刻 / 尺 / チャンネル識別）は
// #27 で program_snapshots に抽出され、この構造体・upsertReservationsFromPassSQL
// からは完全に消えた。「射影にある間は更新、消えたら凍結」の実装は
// UpsertProgramSnapshotsFromProjection（internal/db/queries/program_snapshots.sql）
// 1 本に集約されており、reservations の upsert 側に対応するロジック
// （旧 has_projection の分岐）はもう存在しない。
//
// DedupMatchRecordingID / DedupSimilarity は重複排除の判定根拠（M2-6）。
// マッチしなければ両方 nil = NULL に戻す（前のパスの根拠を残さない。導出値は
// 毎パス作り直す --- CLAUDE.md 不変条件 9）。必ず 2 つ揃って設定/解除するので
// reservations_dedup_evidence_check（両方 NULL か両方非 NULL）を破れない。
type rulerInputRow struct {
	ProgramID             int64           `json:"program_id"`
	RuleID                *int64          `json:"rule_id"`
	Base                  json.RawMessage `json:"base"`
	DedupMatchRecordingID *int64          `json:"dedup_match_recording_id"`
	DedupSimilarity       *float32        `json:"dedup_similarity"`
}

// upsertResult は upsertReservationsFromPass が RETURNING する 1 行。
type upsertResult struct {
	ID        int64
	ProgramID int64
	Created   bool
}

// upsertReservationsFromPassSQL は全量評価の結果を 1 文で reservations に反映する。
//
// sqlc の組み込みアナライザ（実 DB 接続なしのカタログ解析）が
// jsonb_to_recordset の動的レコード型を解決できず generate に失敗するため
// （internal/db/queries/ruler.sql のコメント参照）、rulequery パッケージの流儀に
// 倣ってここに生 SQL として置き、pgx 経由で直接実行する。
//
// resolved CTE が base / rule_id / 重複排除の根拠を「新しい値」として解決し、
// ON CONFLICT ... DO UPDATE ... WHERE の IS DISTINCT FROM で実際に値が変わる行
// だけ UPDATE する。reservations には SSE 用の行トリガーがあるため、変化のない
// 行を書き直すと NOTIFY が全行 x 毎パス飛んでしまう
// （docs/recording.md §3.1「書き込みは差分」）。
//
// #27 でスナップショット列（title / 開始時刻 / 尺 / チャンネル識別）と
// state 列が reservations から抽出・撤去されたため、resolved CTE から
// state の CASE（前パスの rule_id を見ていたため、ルールを削除した経路では
// detached に決して遷移しなかった #30 症状 1 の本体）と has_projection による
// 分岐が両方消えた。「番組終了後に schedule が観測されなかった」という観測は
// 一時 reservations.orphaned_at（reconciler だけが書く不可逆な観測）を経たが、
// issue #98 で recordings の試行行に移設され orphaned_at 列自体も廃止された。
// ruler はこの観測に一切関与しない（recordings にも一切書かない）ので、
// この upsert の INSERT/UPDATE 列にはそもそも登場しない。
const upsertReservationsFromPassSQL = `
WITH input AS (
    SELECT *
    FROM jsonb_to_recordset($2::jsonb) AS d(
        program_id      bigint,
        rule_id         bigint,
        base            jsonb,
        dedup_match_recording_id bigint,
        dedup_similarity         real
    )
),
-- reservations.source は持たない（issue #26 で削除）。
-- 「手動予約かどうか」は program_intents.action='record' の有無、「いまルールが
-- base を供給しているか」は rule_id IS NOT NULL で別々に読めるので、ここで
-- 1 列に合成して書き戻す必要がない。recordings.source（録画時点の provenance）は
-- internal/watcher が record 処理のたびに program_intents を見て導出する。
resolved AS (
    SELECT
        d.program_id,
        d.rule_id,
        CASE WHEN d.rule_id IS NOT NULL THEN d.base ELSE r.base END AS base,
        -- 重複排除の根拠（M2-6）は base と全く同じ凍結ルールに従う: ルールが今
        -- base を供給しているなら今回の判定結果、供給していない（rule_id が
        -- 外れた）なら既存行の値をそのまま凍結する。判定したのはそのルールなので、
        -- base だけ凍結して根拠を消すと「なぜ skip なのか説明できない base」が残る。
        --
        -- 2 列が同一の条件で分岐することが reservations_dedup_evidence_check
        -- （両方 NULL か両方非 NULL）を構造的に破れないことの根拠でもある。
        CASE WHEN d.rule_id IS NOT NULL THEN d.dedup_match_recording_id ELSE r.dedup_match_recording_id END AS dedup_match_recording_id,
        CASE WHEN d.rule_id IS NOT NULL THEN d.dedup_similarity ELSE r.dedup_similarity END AS dedup_similarity
    FROM input d
    LEFT JOIN reservations r ON r.site = $1 AND r.program_id = d.program_id
)
INSERT INTO reservations (
    site, program_id, rule_id, base,
    dedup_match_recording_id, dedup_similarity
)
SELECT $1, program_id, rule_id, base,
       dedup_match_recording_id, dedup_similarity
FROM resolved
ON CONFLICT (site, program_id) DO UPDATE SET
    rule_id              = EXCLUDED.rule_id,
    base                 = EXCLUDED.base,
    dedup_match_recording_id = EXCLUDED.dedup_match_recording_id,
    dedup_similarity     = EXCLUDED.dedup_similarity,
    updated_at           = now()
WHERE reservations.rule_id             IS DISTINCT FROM EXCLUDED.rule_id
   OR reservations.base                IS DISTINCT FROM EXCLUDED.base
   OR reservations.dedup_match_recording_id IS DISTINCT FROM EXCLUDED.dedup_match_recording_id
   OR reservations.dedup_similarity    IS DISTINCT FROM EXCLUDED.dedup_similarity
RETURNING id, program_id, (xmax = 0) AS created
`

// upsertReservationsFromPass は 1 サイト分の desired 行を 1 文で反映する。
// rows が空なら何もしない（jsonb_to_recordset('[]') は 0 行を返すため呼んでも
// 無害だが、往復を避けるため早期リターンする）。
func upsertReservationsFromPass(ctx context.Context, tx pgx.Tx, site string, rows []rulerInputRow) ([]upsertResult, error) {
	if len(rows) == 0 {
		return nil, nil
	}

	payload, err := json.Marshal(rows)
	if err != nil {
		return nil, fmt.Errorf("marshalling ruler input rows: %w", err)
	}

	pgRows, err := tx.Query(ctx, upsertReservationsFromPassSQL, site, payload)
	if err != nil {
		return nil, fmt.Errorf("upserting reservations: %w", err)
	}
	defer pgRows.Close()

	var results []upsertResult
	for pgRows.Next() {
		var res upsertResult
		if err := pgRows.Scan(&res.ID, &res.ProgramID, &res.Created); err != nil {
			return nil, fmt.Errorf("scanning upsert result: %w", err)
		}
		results = append(results, res)
	}
	if err := pgRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating upsert results: %w", err)
	}
	return results, nil
}
