package ruler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// rulerInputRow は upsertReservationsFromPass に渡す 1 行分の入力。
// jsonb_to_recordset で受け取るため、フィールド名は SQL 側の列名に合わせた
// snake_case の json タグを持つ（reservations.base の camelCase 規約とは無関係）。
//
// RuleID が nil の行は「勝者ルールなし」（program_intents だけで desired）を表し、
// SQL 側で base を凍結する（ruler は上書きしない）。
// HasProjection が false の行は EPG プロジェクションから番組が消えたことを表し、
// SQL 側でスナップショット列（Title 以下）を無視して現在の行の値を素通しする。
// このとき Title 等には意味のない値を入れてよい。
//
// DedupMatchRecordingID / DedupSimilarity は重複排除の判定根拠（M2-6）。
// マッチしなければ両方 nil = NULL に戻す（前のパスの根拠を残さない。導出値は
// 毎パス作り直す --- CLAUDE.md 不変条件 9）。必ず 2 つ揃って設定/解除するので
// reservations_dedup_evidence_check（両方 NULL か両方非 NULL）を破れない。
type rulerInputRow struct {
	ProgramID             int64           `json:"program_id"`
	RuleID                *int64          `json:"rule_id"`
	Base                  json.RawMessage `json:"base"`
	HasProjection         bool            `json:"has_projection"`
	Title                 string          `json:"title"`
	StartAt               *time.Time      `json:"start_at"`
	DurationMs            *int64          `json:"duration_ms"`
	NetworkID             *int32          `json:"network_id"`
	ServiceID             *int32          `json:"service_id"`
	ChannelType           *string         `json:"channel_type"`
	Channel               *string         `json:"channel"`
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
// resolved CTE が base / 番組スナップショット / state / rule_id を
// すべて「新しい値」として解決し、ON CONFLICT ... DO UPDATE ... WHERE の
// IS DISTINCT FROM で実際に値が変わる行だけ UPDATE する。reservations には
// SSE 用の行トリガーがあるため、変化のない行を書き直すと NOTIFY が
// 全行 x 毎パス飛んでしまう（docs/recording.md §3.1「書き込みは差分」）。
const upsertReservationsFromPassSQL = `
WITH input AS (
    SELECT *
    FROM jsonb_to_recordset($2::jsonb) AS d(
        program_id      bigint,
        rule_id         bigint,
        base            jsonb,
        has_projection  boolean,
        title           text,
        start_at        timestamptz,
        duration_ms     bigint,
        network_id      integer,
        service_id      integer,
        channel_type    text,
        channel         text,
        dedup_match_recording_id bigint,
        dedup_similarity         real
    )
),
-- reservations.source は持たない（issue #26 で削除。00012_drop_reservations_source.sql）。
-- 「手動予約かどうか」は program_intents.action='record' の有無、「いまルールが
-- base を供給しているか」は rule_id IS NOT NULL で別々に読めるので、ここで
-- 1 列に合成して書き戻す必要がない。recordings.source（録画時点の provenance）は
-- internal/watcher が record 処理のたびに program_intents を見て導出する。
resolved AS (
    SELECT
        d.program_id,
        d.rule_id,
        CASE
            WHEN r.state = 'orphaned' THEN r.state
            WHEN d.rule_id IS NOT NULL THEN 'active'
            WHEN r.rule_id IS NOT NULL THEN 'detached'
            ELSE COALESCE(r.state, 'active')
        END AS state,
        CASE WHEN d.rule_id IS NOT NULL THEN d.base ELSE r.base END AS base,
        CASE WHEN d.has_projection THEN d.title ELSE r.title END AS title,
        CASE WHEN d.has_projection THEN d.start_at ELSE r.program_start_at END AS program_start_at,
        CASE WHEN d.has_projection THEN d.duration_ms ELSE r.program_duration_ms END AS program_duration_ms,
        CASE WHEN d.has_projection THEN d.network_id ELSE r.network_id END AS network_id,
        CASE WHEN d.has_projection THEN d.service_id ELSE r.service_id END AS service_id,
        CASE WHEN d.has_projection THEN d.channel_type ELSE r.channel_type END AS channel_type,
        CASE WHEN d.has_projection THEN d.channel ELSE r.channel END AS channel,
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
    site, program_id, rule_id, state, base,
    title, program_start_at, program_duration_ms,
    network_id, service_id, channel_type, channel,
    dedup_match_recording_id, dedup_similarity
)
SELECT $1, program_id, rule_id, state, base,
       title, program_start_at, program_duration_ms,
       network_id, service_id, channel_type, channel,
       dedup_match_recording_id, dedup_similarity
FROM resolved
ON CONFLICT (site, program_id) DO UPDATE SET
    rule_id              = EXCLUDED.rule_id,
    state                = EXCLUDED.state,
    base                 = EXCLUDED.base,
    title                = EXCLUDED.title,
    program_start_at     = EXCLUDED.program_start_at,
    program_duration_ms  = EXCLUDED.program_duration_ms,
    network_id           = EXCLUDED.network_id,
    service_id           = EXCLUDED.service_id,
    channel_type         = EXCLUDED.channel_type,
    channel              = EXCLUDED.channel,
    dedup_match_recording_id = EXCLUDED.dedup_match_recording_id,
    dedup_similarity     = EXCLUDED.dedup_similarity,
    updated_at           = now()
WHERE reservations.rule_id             IS DISTINCT FROM EXCLUDED.rule_id
   OR reservations.state               IS DISTINCT FROM EXCLUDED.state
   OR reservations.base                IS DISTINCT FROM EXCLUDED.base
   OR reservations.title               IS DISTINCT FROM EXCLUDED.title
   OR reservations.program_start_at    IS DISTINCT FROM EXCLUDED.program_start_at
   OR reservations.program_duration_ms IS DISTINCT FROM EXCLUDED.program_duration_ms
   OR reservations.network_id          IS DISTINCT FROM EXCLUDED.network_id
   OR reservations.service_id          IS DISTINCT FROM EXCLUDED.service_id
   OR reservations.channel_type        IS DISTINCT FROM EXCLUDED.channel_type
   OR reservations.channel             IS DISTINCT FROM EXCLUDED.channel
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

// insertReservationRuleMatchesSQL は reservation_rule_matches を集合演算 1 文で追加する。
// 呼び出し側が対象 reservation_id の既存行を先に削除してから呼ぶ（このテーブルには
// SSE 用の行トリガーがないため、reservations と違い差分書き込みは要求されない。
// 「毎パス書き換え」でよい — docs/recording.md §3.1「複数ルール解決」）。
const insertReservationRuleMatchesSQL = `
INSERT INTO reservation_rule_matches (reservation_id, rule_id)
SELECT * FROM unnest($1::bigint[], $2::bigint[])
`

func insertReservationRuleMatches(ctx context.Context, tx pgx.Tx, reservationIDs, ruleIDs []int64) error {
	if len(reservationIDs) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, insertReservationRuleMatchesSQL, reservationIDs, ruleIDs); err != nil {
		return fmt.Errorf("inserting reservation_rule_matches: %w", err)
	}
	return nil
}
