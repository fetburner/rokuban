-- チューナー射影 (issue #24 M2-10, issue #21 の論点 1 → 案 B、docs/data.md §6.5)。
--
-- tuner_sync は epg_services / epg_programs と同じ**使い捨てプロジェクション**で、
-- 真実は常に mirakc 側にある。よって差分同期はせず、毎パス全量 upsert + スイープで
-- レベルトリガーに収束させる（migrations/00015_tuner_sync.sql のコメント参照）。

-- 全量同期のスイープ基準時刻。observed_at は DB の now() で書かれるため、
-- 基準時刻もアプリのクロックではなく DB から取る（クロックスキューで
-- プロジェクション全体を消して再投入する事故を防ぐ）。EpgSweepMark と同じ理由・
-- 同じ形だが、epg.sql とは並行作業で取り合いになるので別に持つ。
-- name: TunerSweepMark :one
SELECT now()::timestamptz AS mark;

-- name: UpsertTunerSync :batchexec
INSERT INTO tuner_sync (
    site, tuner_index, name, types, is_available, is_fault, observed_at
) VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (site, tuner_index) DO UPDATE SET
    name         = EXCLUDED.name,
    types        = EXCLUDED.types,
    is_available = EXCLUDED.is_available,
    is_fault     = EXCLUDED.is_fault,
    observed_at  = now();

-- name: DeleteStaleTunerSync :execrows
DELETE FROM tuner_sync
WHERE site = $1 AND observed_at < $2;

-- 射影されたチューナーを全件返す。
--
-- is_available / is_fault で絞らないのは、**cap(A) に数えるかの判定を
-- internal/capacity（Hall 条件が住んでいる場所）に置く**ため。ここで絞ると
-- 「存在するが数えない」と「そもそも射影が無い」が区別できなくなり、
-- 射影が空のときに「容量ゼロ = 全区間が超過」と主張してしまう
-- （internal/capacity.Compute のコメント参照）。
-- name: ListTunerSync :many
SELECT * FROM tuner_sync
WHERE site = $1
ORDER BY tuner_index;
