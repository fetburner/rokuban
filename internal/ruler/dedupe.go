package ruler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// dedupeCandidate は重複排除の判定対象 1 件。勝者ルールが決まった番組のうち、
// そのルールが dedupe_enabled なものだけを渡す。
//
// 題名と番組の識別子（network_id / service_id / event_id）は SQL 側で
// epg_programs を JOIN して取るため、ここには載せない。Go 側でスナップショットを
// 組み直すと「射影から消えた番組」の扱いが 2 箇所に散る（JOIN で落ちれば
// 判定対象から自然に外れる）。
type dedupeCandidate struct {
	ProgramID int64 `json:"program_id"`
	RuleID    int64 `json:"rule_id"`
}

// dedupeMatch は 1 番組分の重複排除の判定結果（マッチした録画とその類似度）。
// reservations.dedup_match_recording_id / dedup_similarity にそのまま焼く
// 「なぜスキップされたか」の根拠。
type dedupeMatch struct {
	RecordingID int64
	Similarity  float32
}

// evaluateDedupeSQL は候補番組ごとに「同じルールで既に録れている番組」を 1 文で探す。
//
// 予約 1 件ずつループして N 回問い合わせるのではなく、候補の集合を jsonb で渡して
// Postgres の集合演算 1 文で解く（internal/ruler/sql.go と同じ流儀。sqlc の
// 組み込みアナライザが jsonb_to_recordset の動的レコード型を解決できないため、
// internal/db/queries/ ではなくここに生 SQL として置く）。
//
// 判定条件（docs/recording.md §3.1「重複排除（再放送スキップ）」）:
//
//   - **同じ rule_id の recordings だけ**を比較対象にする。グローバルな突き合わせでは
//     なく、「同じルールが同じ番組シリーズを指している」という前提に乗る
//   - **status = 'finished' のみ**。'recording'（進行中）も 'failed' も「録れた」とは
//     みなさない
//   - **deleted_at では絞らない。** ごみ箱に入れても物理削除しても recordings 行は
//     tombstone として残り、重複排除はそれでも機能する契約（docs/schema.md §5
//     「ごみ箱を空にしても録画履歴・ドロップ統計・重複排除は壊れない」）。
//     `deleted_at IS NULL` を足すのは**書き忘れの修正ではなく契約違反**になる
//   - dedupe_window が NULL なら時間窓なし（全履歴が対象）。rules の CHECK は
//     dedupe_enabled のとき dedupe_threshold だけを要求し window は任意なので、
//     NULL は「無制限」と解釈する
//   - 類似度は pg_trgm の similarity()。記号除去 + 完全一致にしないのは
//     EPGStation#704 の教訓（囲み文字を一律除去する正規化は「前編/後編」の区別まで
//     消して誤判定する）。trgm なら語の違いが自然に類似度を下げる
//
// **自分自身の録画は除外する**（(network_id, service_id, event_id) の不一致）。
// 放送済みの番組の予約は GC（終了 + retention_grace）まで残るので、録画が finished に
// なった次のパスで similarity = 1.0 の自己一致が必ず起きる。そうなると UI が
// 「録画済みの番組」を「重複としてスキップ」と説明するうえ、effective.skip = true に
// なることで reconciler.markOrphaned / detectStartDelays の入力（listDesired の出力）
// からも外れてしまい、**重複排除が無関係な状態機械の DB 状態を変える**。
// site は比較に入れない --- 同一放送は全サイトで同じ programId を持ち、マッチした
// 全サイトで予約を作る N 予約が既定（docs/recording.md §3.1「サイトの扱い」）なので、
// サイト間の共食いも同時に防ぐ必要がある。
//
// DISTINCT ON で番組ごとのベストマッチ 1 件に絞る。tie-break に rec.id ASC を
// 入れて**決定的**にするのが要点: 同じ類似度の録画が複数あるときに勝者が毎パス
// 入れ替わると、base の差分書き込みが発火し続けて NOTIFY が鳴り止まず、mirakc に
// 更新 API がないため reconciler が schedule を DELETE + POST で作り直し続ける
// フラッピングになる（同 §3.1 の priority 同率タイと同じクラスの問題）。
//
// recordings.title への trgm GIN インデックスは張っていない（00013 で削除済み）。
// gin_trgm_ops が加速するのは % / <% / LIKE / 正規表現で、similarity() の関数呼び出しは
// インデックスに乗らない。乗せるなら % を前段フィルタにする形になるが、% は閾値を
// ルール単位ではなく GUC pg_trgm.similarity_threshold から読むため、
// rules.dedupe_threshold と直接は噛み合わない。将来必要になったら
//
//  1. 有効な dedupe ルールの**最小**閾値を Go 側で求め、
//     SELECT set_config('pg_trgm.similarity_threshold', $n, true) でトランザクション
//     ローカルに設定する（true = local。パス外に漏らさない）
//  2. JOIN 条件に rec.title % c.title を足して前段フィルタにする（最小閾値なので
//     全ルールの条件の上位集合になり、取りこぼさない）
//  3. 本判定は similarity() >= c.dedupe_threshold のまま残す
//
// という手順になる。家庭用の録画履歴 x 1 パスの候補数では素の走査で足りるので、
// 隠れたセッション状態を持ち込む前に実測してから入れる。
const evaluateDedupeSQL = `
WITH input AS (
    SELECT *
    FROM jsonb_to_recordset($2::jsonb) AS d(
        program_id bigint,
        rule_id    bigint
    )
),
candidate AS (
    SELECT d.program_id,
           d.rule_id,
           p.name AS title,
           p.network_id,
           p.service_id,
           p.event_id,
           ru.dedupe_threshold,
           ru.dedupe_window
    FROM input d
    JOIN rules ru       ON ru.id = d.rule_id AND ru.dedupe_enabled
    JOIN epg_programs p ON p.site = $1 AND p.program_id = d.program_id
)
SELECT DISTINCT ON (c.program_id)
       c.program_id,
       rec.id AS recording_id,
       similarity(rec.title, c.title) AS similarity
FROM candidate c
JOIN recordings rec
  ON  rec.rule_id = c.rule_id
  AND rec.status = 'finished'
  AND (rec.network_id, rec.service_id, rec.event_id)
      IS DISTINCT FROM (c.network_id, c.service_id, c.event_id)
  AND (c.dedupe_window IS NULL
       OR rec.program_start_at >= now() - c.dedupe_window)
  AND similarity(rec.title, c.title) >= c.dedupe_threshold
ORDER BY c.program_id, similarity(rec.title, c.title) DESC, rec.id ASC
`

// evaluateDedupe は候補番組ごとの重複排除の判定結果を返す。マッチしなかった番組は
// マップに現れない（呼び出し側は「無い = 根拠 2 列を NULL に戻す」として扱う）。
//
// candidates が空なら往復しない。
func evaluateDedupe(ctx context.Context, pool *pgxpool.Pool, site string, candidates []dedupeCandidate) (map[int64]dedupeMatch, error) {
	if len(candidates) == 0 {
		return nil, nil
	}

	payload, err := json.Marshal(candidates)
	if err != nil {
		return nil, fmt.Errorf("marshalling dedupe candidates: %w", err)
	}

	rows, err := pool.Query(ctx, evaluateDedupeSQL, site, payload)
	if err != nil {
		return nil, fmt.Errorf("querying dedupe matches: %w", err)
	}
	defer rows.Close()

	matches := make(map[int64]dedupeMatch)
	for rows.Next() {
		var programID int64
		var m dedupeMatch
		if err := rows.Scan(&programID, &m.RecordingID, &m.Similarity); err != nil {
			return nil, fmt.Errorf("scanning dedupe match: %w", err)
		}
		matches[programID] = m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating dedupe matches: %w", err)
	}
	return matches, nil
}
