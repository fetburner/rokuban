-- name: GetReservation :one
SELECT id, rule_id, source FROM reservations
WHERE id = $1;

-- name: GetReservationBySiteAndProgramID :one
SELECT id, rule_id, source FROM reservations
WHERE site = $1 AND program_id = $2;
