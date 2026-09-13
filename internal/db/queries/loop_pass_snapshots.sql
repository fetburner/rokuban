-- ruler / record_sweep の成功鮮度は、それぞれの処理結果の行とは別の寿命を持つ
-- ため、専用の衛星表に保存する。呼び出し側は処理が成功した後だけ upsert する。

-- name: UpsertRulerPassSnapshot :exec
INSERT INTO ruler_pass_snapshots (site, last_success_at)
VALUES ($1, now())
ON CONFLICT (site) DO UPDATE SET
    last_success_at = EXCLUDED.last_success_at;

-- name: GetRulerPassSnapshot :one
SELECT last_success_at
FROM ruler_pass_snapshots
WHERE site = $1;

-- name: UpsertRecordSweepSnapshot :exec
INSERT INTO record_sweep_snapshots (site, last_success_at)
VALUES ($1, now())
ON CONFLICT (site) DO UPDATE SET
    last_success_at = EXCLUDED.last_success_at;

-- name: GetRecordSweepSnapshot :one
SELECT last_success_at
FROM record_sweep_snapshots
WHERE site = $1;
