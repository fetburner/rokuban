-- 番組単位のユーザー意図（issue #18 の案 A）。api だけが書き、ruler は読むだけ。

-- name: UpsertProgramIntent :one
INSERT INTO program_intents (
    site, program_id, action, overrides, program_start_at, program_duration_ms
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (site, program_id) DO UPDATE SET
    action     = EXCLUDED.action,
    overrides  = EXCLUDED.overrides,
    updated_at = now()
RETURNING *;

-- 取消（intent{skip}）は既存の overrides を保ったまま action だけを倒す。
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
