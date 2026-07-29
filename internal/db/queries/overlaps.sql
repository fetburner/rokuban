-- 予約時の重なり警告 (issue #24 M2-8, issue #21 の「案 C」)。
--
-- チューナー射影は使わない。同時間帯に既に何件の予約があるかという事実だけを
-- 返す（docs/data.md §6.5 が扱う容量超過判定は M2-10 の領分で、ここでは行わない）。

-- 半開区間 [window_start, window_end) と重なる予約を、自分自身
-- （program_id = target_program_id）を除き、orphaned_at IS NULL で絞って返す
-- （#28/#30: state 列は orphaned_at に置き換わった。orphaned でない行を除外して
-- よい理由は state <> 'orphaned' のときと同じ）。番組の開始時刻・尺は
-- program_snapshots に移設された（#27）ので JOIN して引く。
-- 半開区間の判定はここ（SQL）で行うが、effective.skip の判定は Go 側で
-- db.EffectiveOptions を通す（internal/api/reservations_overlaps.go）。
-- program_intents / program_overrides との JOIN は ListReservationsBySite
-- (internal/db/queries/reservations.sql) と同じ形。
-- name: ListOverlappingReservations :many
SELECT sqlc.embed(r), sqlc.embed(s), i.action AS intent_action, o.overrides AS overrides
FROM reservations r
JOIN program_snapshots s ON s.site = r.site AND s.program_id = r.program_id
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE r.site = $1
  AND r.program_id <> sqlc.arg(target_program_id)::bigint
  AND r.orphaned_at IS NULL
  AND s.start_at < sqlc.arg(window_end)::timestamptz
  AND s.start_at + (s.duration_ms * interval '1 millisecond') > sqlc.arg(window_start)::timestamptz
ORDER BY s.start_at;
