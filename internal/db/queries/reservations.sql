-- name: GetReservation :one
SELECT id, rule_id, source FROM reservations
WHERE id = $1;

-- name: GetReservationBySiteAndProgramID :one
SELECT id, rule_id, source FROM reservations
WHERE site = $1 AND program_id = $2;

-- name: CreateManualReservation :one
INSERT INTO reservations (site, program_id, source, overrides, title, program_start_at, program_duration_ms)
VALUES ($1, $2, 'manual', $3, $4, $5, $6)
RETURNING *;

-- name: GetReservationFull :one
SELECT * FROM reservations WHERE id = $1;

-- name: ListReservationsBySite :many
SELECT * FROM reservations
WHERE site = $1
ORDER BY program_start_at;

-- name: ListActiveReservationsBySite :many
SELECT * FROM reservations
WHERE site = $1 AND state = 'active'
ORDER BY program_start_at;

-- name: DeleteReservation :execrows
DELETE FROM reservations WHERE id = $1;

-- name: MarkReservationOrphaned :exec
UPDATE reservations
SET state = 'orphaned', updated_at = now()
WHERE id = $1 AND state = 'active';
