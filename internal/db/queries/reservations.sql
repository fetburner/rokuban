-- name: GetReservation :one
SELECT id, rule_id, source FROM reservations
WHERE id = $1;

-- name: GetReservationBySiteAndProgramID :one
SELECT id, rule_id, source FROM reservations
WHERE site = $1 AND program_id = $2;

-- name: CreateManualReservation :one
INSERT INTO reservations (site, program_id, source, title, program_start_at, program_duration_ms)
VALUES ($1, $2, 'manual', $3, $4, $5)
RETURNING *;

-- 予約とユーザー意図を 1 行に合わせて返す。overrides は program_intents 側にあり、
-- 予約が存在しても意図がない（純粋なルール由来）ことはあるので LEFT JOIN。
-- name: GetReservationFull :one
SELECT sqlc.embed(r), i.action AS intent_action, i.overrides AS intent_overrides
FROM reservations r
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
WHERE r.id = $1;

-- name: ListReservationsBySite :many
SELECT sqlc.embed(r), i.action AS intent_action, i.overrides AS intent_overrides
FROM reservations r
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
WHERE r.site = $1
ORDER BY r.program_start_at;

-- name: ListActiveReservationsBySite :many
SELECT sqlc.embed(r), i.action AS intent_action, i.overrides AS intent_overrides
FROM reservations r
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
WHERE r.site = $1 AND r.state = 'active'
ORDER BY r.program_start_at;

-- name: DeleteReservation :execrows
DELETE FROM reservations WHERE id = $1;

-- name: DeleteReservationBySiteAndProgramID :execrows
DELETE FROM reservations WHERE site = $1 AND program_id = $2;

-- name: MarkReservationOrphaned :exec
UPDATE reservations
SET state = 'orphaned', updated_at = now()
WHERE id = $1 AND state = 'active';
