-- 番組単位のパラメータ上書き（M2-4 / 00010 で program_intents から分離。issue #18
-- の案 A の続き）。api だけが書き、ruler は読むだけ（program_intents と同じ規律）。
--
-- overrides の中身は internal/api がマージする（db.ReservationOptions の型付き
-- フィールドとして。docs/recording.md §4.2「jsonb を許す条件」）。SQL 側は
-- 常に上書き後の完成形を受け取って書くだけで、jsonb の内容を検査・加工しない。

-- name: GetProgramOverrides :one
SELECT * FROM program_overrides WHERE site = $1 AND program_id = $2;

-- name: UpsertProgramOverrides :one
INSERT INTO program_overrides (
    site, program_id, overrides, program_start_at, program_duration_ms
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (site, program_id) DO UPDATE SET
    overrides  = EXCLUDED.overrides,
    updated_at = now()
RETURNING *;

-- name: DeleteProgramOverrides :execrows
DELETE FROM program_overrides WHERE site = $1 AND program_id = $2;

-- 番組終了後の GC。program_intents と同じ cutoff で ruler.runGC から呼ばれる
-- （上書きの寿命を放送の寿命に揃える。docs/schema.md §3.5）。
-- name: DeleteEndedProgramOverrides :execrows
DELETE FROM program_overrides
WHERE program_start_at + (program_duration_ms * interval '1 millisecond') < $1;
