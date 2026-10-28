-- 大量削除サーキットブレーカーの永続状態（M2-5）。
-- 行の存在 = 発動中。再開は DELETE（internal/breaker のコメント参照）。

-- 発動。既に発動中なら件数とサンプルだけ更新し、tripped_at は最初の発動時刻を保つ
-- （「いつから止まっているか」が運用上の関心事なので、パスごとに現在時刻へ
-- 進めてはならない）。
-- name: TripCircuitBreaker :one
INSERT INTO circuit_breakers (scope, name, pending, threshold, detail)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (scope, name) DO UPDATE SET
    pending   = EXCLUDED.pending,
    threshold = EXCLUDED.threshold,
    detail    = EXCLUDED.detail
RETURNING *;

-- name: GetCircuitBreaker :one
SELECT * FROM circuit_breakers WHERE scope = $1 AND name = $2;

-- name: ListCircuitBreakers :many
SELECT * FROM circuit_breakers ORDER BY scope, name;

-- 再開（手動確認後）。execrows なので「発動していなかった」を呼び出し側が
-- 区別できる（API が 404 を返せる）。
-- name: ResumeCircuitBreaker :execrows
DELETE FROM circuit_breakers WHERE scope = $1 AND name = $2;
