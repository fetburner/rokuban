-- 書き手は encode reconcile だけ。表の意味は migration のコメントにある。

-- name: LockTransferBacklogSnapshots :exec
-- 並行する 2 つのパスが「前の値」を同じ値として読まないよう、読み書きを直列にする。
SELECT pg_advisory_xact_lock(hashtextextended('transfer_backlog_snapshots', 0));

-- name: ListTransferBacklogSnapshots :many
SELECT store, assets, counted_at
FROM transfer_backlog_snapshots
ORDER BY store;

-- counted_at が保存済みより新しいときだけ書く。影響行数 1 が「書いた」、0 が「古いので捨てた」。
-- name: UpsertTransferBacklogSnapshot :execrows
INSERT INTO transfer_backlog_snapshots (store, assets, counted_at)
VALUES ($1, $2, $3)
ON CONFLICT (store) DO UPDATE SET
    assets = EXCLUDED.assets,
    counted_at = EXCLUDED.counted_at
WHERE transfer_backlog_snapshots.counted_at < EXCLUDED.counted_at;

-- name: DeleteTransferBacklogSnapshotsExcept :exec
DELETE FROM transfer_backlog_snapshots
WHERE NOT (store = ANY($1::text[]));
