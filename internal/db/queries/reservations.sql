-- name: GetReservation :one
SELECT id, rule_id, source FROM reservations
WHERE id = $1;

-- name: GetReservationBySiteAndProgramID :one
SELECT id, rule_id, source FROM reservations
WHERE site = $1 AND program_id = $2;

-- name: CreateManualReservation :one
-- network_id/service_id/channel_type/channel は api がトランザクション内で
-- GetProgramChannelIdentity から引いた値をスナップショットする（サーバー権威。
-- クライアントからは受け取らない）。mirakc の programId 内部構造への依存を
-- reconciler から消すための列。
INSERT INTO reservations (
    site, program_id, source, title, program_start_at, program_duration_ms,
    network_id, service_id, channel_type, channel
)
VALUES ($1, $2, 'manual', $3, $4, $5, $6, $7, $8, $9)
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

-- 番組終了後の GC（issue #24 の M2-3、docs/schema.md §3「行の物理削除（GC）は
-- 「番組の終了時刻を過ぎた後」のみ」）。state を問わず（active/detached/orphaned
-- いずれも）終了時刻 + 猶予を過ぎたら削除する。recordings.reservation_id は
-- ON DELETE SET NULL なので、録画履歴（recordings/media_assets）はこの削除の
-- 影響を受けない。
-- name: DeleteEndedReservations :execrows
DELETE FROM reservations
WHERE program_start_at + (program_duration_ms * interval '1 millisecond') < $1;
