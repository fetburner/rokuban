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

-- 予約とユーザー意図・上書きを 1 行に合わせて返す。action は program_intents、
-- overrides は program_overrides（M2-4 / 00010 で分離）にあり、予約が存在しても
-- 意図・上書きのどちらかしかない（あるいはどちらも無い）ことがあるので
-- 両方 LEFT JOIN する。
-- name: GetReservationFull :one
SELECT sqlc.embed(r), i.action AS intent_action, o.overrides AS overrides
FROM reservations r
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE r.id = $1;

-- name: ListReservationsBySite :many
SELECT sqlc.embed(r), i.action AS intent_action, o.overrides AS overrides
FROM reservations r
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE r.site = $1
ORDER BY r.program_start_at;

-- reconciler が mirakc へ同期する対象（docs/schema.md §3「state を『mirakc への
-- 同期対象か』のフィルタに使ってはならない」、docs/recording.md §4.3）。
-- 同期の可否を決めるのは effective.skip であり、state で除外してよいのは
-- orphaned だけ（番組が終了しているので schedule を作る意味がない）。
-- active/detached はどちらも「実質 manual として動く」ことがあるため同期対象に
-- 含める。旧名 ListActiveReservationsBySite は state='active' でしか絞っておらず
-- detached の予約に schedule が作られないバグの原因だった（M2-4 で修正）。
-- name: ListSyncableReservationsBySite :many
SELECT sqlc.embed(r), i.action AS intent_action, o.overrides AS overrides
FROM reservations r
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE r.site = $1 AND r.state <> 'orphaned'
ORDER BY r.program_start_at;

-- PATCH /api/reservations/{id} と DELETE .../overrides の直列化のため、予約行を
-- FOR UPDATE でロックする。Rokuban は構造的に単一世帯用アプリで認証機構を
-- 持たないため同時 PATCH は事実上起きないが、1 行のロックで足りるので取る
-- （docs/recording.md §4.2「マージは Go 側で型付きに行う」）。
-- name: LockReservation :one
SELECT id FROM reservations WHERE id = $1 FOR UPDATE;

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
