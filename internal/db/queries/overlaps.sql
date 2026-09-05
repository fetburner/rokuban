-- 予約時の重なり警告 (issue #24 M2-8, issue #21 の「案 C」)。
--
-- チューナー射影は使わない。同時間帯に既に何件の予約があるかという事実だけを
-- 返す（docs/data.md §6.5 が扱う容量超過判定は M2-10 の領分で、ここでは行わない）。

-- 半開区間 [window_start, window_end) と重なる予約を、自分自身
-- （program_id = target_program_id）を除き、never-scheduled 済みの予約を
-- 除外して返す。番組の開始時刻・尺は program_snapshots に移設された（#27）
-- ので JOIN して引く。半開区間の判定はここ（SQL）で行うが、effective.skip
-- の判定は Go 側で reservation.EffectiveOptions を通す
-- （internal/api/reservations_overlaps.go）。program_intents /
-- program_overrides との JOIN は ListReservationsFull
-- (internal/db/queries/reservations.sql) と同じ形。
--
-- 欠測除外の述語は never_scheduled_events 表を放送イベントキーで引く NOT EXISTS
-- で、ListReservationsForSyncEvaluation (internal/db/queries/reservations.sql) /
-- ListCapacityDemand (internal/db/queries/capacity.sql) と全く同じ。
-- 理由は internal/db/queries/reservations.sql のコメント参照。
-- name: ListOverlappingReservations :many
SELECT sqlc.embed(r), sqlc.embed(s), i.action AS intent_action, o.overrides AS overrides
FROM reservations r
JOIN program_snapshots s ON s.site = r.site AND s.program_id = r.program_id
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE r.site = $1
  AND r.program_id <> sqlc.arg(target_program_id)::bigint
  AND NOT EXISTS (
      SELECT 1 FROM never_scheduled_events nse
      -- 宛先のキーは**放送イベント**であって reservations 行ではない。reservations は
      -- program_snapshots への FK が ON DELETE CASCADE なので、スナップショットが
      -- GC された瞬間に一緒に消える。never_scheduled_events は program_snapshots への
      -- FK を持たないので GC 後も観測が残り続ける（docs/schema/reservations.md の
      -- 「行の物理削除」）。reservations 行に依存すると、GC された瞬間に
      -- 「never-scheduled 行が無い」ことになり、終了済み予約が毎パス desired に
      -- 戻り続ける（CLAUDE.md 不変条件 9 の identity: 導出器が作るキーを宛先にしない、
      -- と同じ族）。
      WHERE nse.site = r.site
        AND nse.network_id = s.network_id
        AND nse.service_id = s.service_id
        AND nse.event_id = s.event_id
  )
  AND s.start_at < sqlc.arg(window_end)::timestamptz
  AND s.start_at + (s.duration_ms * interval '1 millisecond') > sqlc.arg(window_start)::timestamptz
ORDER BY s.start_at;
