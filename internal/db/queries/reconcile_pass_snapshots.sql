-- desired−observed の全件パスの完走時刻（migration の表の説明を参照）。
-- 呼び出し側は、パスが完走した後だけ upsert する。

-- name: UpsertReconcilePassSnapshot :exec
INSERT INTO reconcile_pass_snapshots (pass, last_success_at)
VALUES ($1, now())
ON CONFLICT (pass) DO UPDATE SET
    last_success_at = EXCLUDED.last_success_at;

-- name: ListReconcilePassSnapshots :many
SELECT pass, last_success_at
FROM reconcile_pass_snapshots
ORDER BY pass;

-- name: UpsertDeleteReconcileSnapshot :exec
INSERT INTO delete_reconcile_snapshots (store, last_success_at)
VALUES ($1, now())
ON CONFLICT (store) DO UPDATE SET
    last_success_at = EXCLUDED.last_success_at;

-- name: ListDeleteReconcileSnapshots :many
SELECT store, last_success_at
FROM delete_reconcile_snapshots
ORDER BY store;
