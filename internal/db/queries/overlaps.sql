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
-- 欠測除外は、放送イベントと snapshot の開始時刻で
-- never_scheduled_events 表を引く NOT EXISTS。
-- ListReservationsForSyncEvaluation
-- (internal/db/queries/reservations.sql) と
-- ListCapacityDemand (internal/db/queries/capacity.sql)
-- で使う述語と全く同じ。
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
      -- 宛先のキーは、放送イベントと snapshot の
      -- 開始時刻である。reservations 行は使わない。
      -- reservations は snapshot の GC 時に
      -- ON DELETE CASCADE で消えるが、欠測行は
      -- 同時には消えない。欠測表は snapshot に FK を
      -- 持たず、放送地平を超えた観測は別の GC で刈る。
      -- reservations 行を使うと GC 後に欠測行が無いと
      -- 誤認され、終了済み予約が毎パス desired に戻る。
      -- 導出器が作るキーを宛先にしない
      -- （CLAUDE.md 不変条件 9 の identity）。
      WHERE nse.site = r.site
        AND nse.network_id = s.network_id
        AND nse.service_id = s.service_id
        AND nse.event_id = s.event_id
        AND nse.program_start_at = s.start_at
  )
  AND s.start_at < sqlc.arg(window_end)::timestamptz
  AND s.start_at + (s.duration_ms * interval '1 millisecond') > sqlc.arg(window_start)::timestamptz
ORDER BY s.start_at;
