-- 番組単位のパラメータ上書き（M2-4 で program_intents から分離。issue #18
-- の案 A の続き）。api だけが書き、ruler は読むだけ（program_intents と同じ規律）。
--
-- overrides の中身は internal/api がマージする（reservation.Options の型付き
-- フィールドとして。docs/recording.md §4.2「jsonb を許す条件」）。SQL 側は
-- 常に上書き後の完成形を受け取って書くだけで、jsonb の内容を検査・加工しない。

-- name: GetProgramOverrides :one
SELECT * FROM program_overrides WHERE site = $1 AND program_id = $2;

-- program_start_at / program_duration_ms は #27 で program_snapshots に抽出され、
-- program_overrides からは落ちた。FK (site, program_id) REFERENCES program_snapshots
-- があるので、呼び出し側はこの INSERT より先に program_snapshots の行を
-- upsert しておくこと。
-- name: UpsertProgramOverrides :one
INSERT INTO program_overrides (
    site, program_id, overrides
) VALUES ($1, $2, $3)
ON CONFLICT (site, program_id) DO UPDATE SET
    overrides  = EXCLUDED.overrides,
    updated_at = now()
RETURNING *;

-- name: DeleteProgramOverrides :execrows
DELETE FROM program_overrides WHERE site = $1 AND program_id = $2;

-- 番組終了後の GC は DeleteEndedProgramSnapshots（internal/db/queries/program_snapshots.sql）
-- 1 本に集約された（#27）。program_overrides は program_snapshots への FK が
-- ON DELETE CASCADE なので、program_snapshots 側の行が消えれば一緒に落ちる。
-- 個別の DeleteEndedProgramOverrides は撤去した。
