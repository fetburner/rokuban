-- 監視向けの成功時刻（loop_success_markers の表の説明は migration の冒頭を参照）。
-- 呼び出し側は、パスが完走した後だけ upsert する。

-- name: UpsertLoopSuccessMarker :exec
INSERT INTO loop_success_markers (loop, scope, last_success_at)
VALUES ($1, $2, now())
ON CONFLICT (loop, scope) DO UPDATE SET
    last_success_at = EXCLUDED.last_success_at;

-- name: ListLoopSuccessMarkers :many
SELECT scope, last_success_at
FROM loop_success_markers
WHERE loop = $1
ORDER BY scope;
