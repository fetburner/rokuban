-- 番組単位のユーザー意図（issue #18 の案 A）。api だけが書き、ruler は読むだけ。
--
-- overrides（パラメータの上書き）は M2-4（00010）で program_overrides に分離
-- 済み。この表が持つのは action（record/skip）のみ（docs/schema.md §3.5）。

-- name: UpsertProgramIntent :one
INSERT INTO program_intents (
    site, program_id, action, program_start_at, program_duration_ms
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (site, program_id) DO UPDATE SET
    action     = EXCLUDED.action,
    updated_at = now()
RETURNING *;

-- 取消（intent{skip}）は action だけを倒す。overrides は別表なので触らない。
-- 番組情報のスナップショットは新規作成時のみ必要。
-- name: SkipProgram :one
INSERT INTO program_intents (
    site, program_id, action, program_start_at, program_duration_ms
) VALUES ($1, $2, 'skip', $3, $4)
ON CONFLICT (site, program_id) DO UPDATE SET
    action     = 'skip',
    updated_at = now()
RETURNING *;

-- name: GetProgramIntent :one
SELECT * FROM program_intents WHERE site = $1 AND program_id = $2;

-- name: DeleteProgramIntent :execrows
DELETE FROM program_intents WHERE site = $1 AND program_id = $2;

-- 番組終了後の GC。意図の寿命を放送の寿命に揃える。
-- name: DeleteEndedProgramIntents :execrows
DELETE FROM program_intents
WHERE program_start_at + (program_duration_ms * interval '1 millisecond') < $1;

-- shadow-diff（M2-14）用。skip 意図は reservations 行を持たないため
-- （案 A の核心。上の UpsertProgramIntent のコメント参照）、EPGStation との
-- 差分照合では program_intents を直接引く必要がある。番組名は表示のためだけに
-- EPG プロジェクションから補う。放送終了間際で epg_programs から刈られていれば
-- NULL のままでよい（照合キーは programId であってタイトルではない）。
-- name: ListSkippedProgramIntentsBySite :many
SELECT i.program_id, i.program_start_at, i.program_duration_ms, p.name
FROM program_intents i
LEFT JOIN epg_programs p ON p.site = i.site AND p.program_id = i.program_id
WHERE i.site = $1 AND i.action = 'skip'
ORDER BY i.program_start_at;
