-- 番組単位のユーザー意図（issue #18 の案 A）。api だけが書き、ruler は読むだけ。
--
-- overrides（パラメータの上書き）は M2-4 で program_overrides に分離
-- 済み。この表が持つのは action（record/skip）のみ（docs/schema.md §3.5）。

-- program_start_at / program_duration_ms は #27 で program_snapshots に抽出され、
-- program_intents からは落ちた。FK (site, program_id) REFERENCES program_snapshots
-- があるので、呼び出し側はこの INSERT より先に program_snapshots の行を
-- upsert しておくこと。
-- name: UpsertProgramIntent :one
INSERT INTO program_intents (
    site, program_id, action
) VALUES ($1, $2, $3)
ON CONFLICT (site, program_id) DO UPDATE SET
    action     = EXCLUDED.action,
    updated_at = now()
RETURNING *;

-- 取消（intent{skip}）は action だけを倒す。overrides は別表なので触らない。
-- name: SkipProgram :one
INSERT INTO program_intents (
    site, program_id, action
) VALUES ($1, $2, 'skip')
ON CONFLICT (site, program_id) DO UPDATE SET
    action     = 'skip',
    updated_at = now()
RETURNING *;

-- name: GetProgramIntent :one
SELECT * FROM program_intents WHERE site = $1 AND program_id = $2;

-- name: DeleteProgramIntent :execrows
DELETE FROM program_intents WHERE site = $1 AND program_id = $2;

-- 番組終了後の GC は DeleteEndedProgramSnapshots（internal/db/queries/program_snapshots.sql）
-- 1 本に集約された（#27）。program_intents は program_snapshots への FK が
-- ON DELETE CASCADE なので、program_snapshots 側の行が消えれば一緒に落ちる。
-- 個別の DeleteEndedProgramIntents は撤去した。
