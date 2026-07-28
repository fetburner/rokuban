-- 開始遅延検出器（issue #24 M2-7、docs/recording.md §3.3「開始遅延検出器」）。
--
-- reconciler.detectStartDelays は listDesired が返した desired 予約
-- （effective.skip 済み除外、state <> 'orphaned' 済み除外）のうち「開始時刻 +
-- 猶予 < now() < 終了時刻」の窓にある予約 ID をここに渡し、その中で
-- recordings.started_at が埋まっている（= watcher が mirakc の record から
-- 観測済み）予約 ID だけを引く。渡した ID からここで返った ID を除いた差集合が
-- 「開始時刻を過ぎたのに録画開始が観測されていない」候補になる。
--
-- recordings 行そのものが無い予約（録画がまだ一切観測されていない）は当然
-- ここに出てこないので、呼び出し側では「返らなかった ID = 観測なし」として
-- 扱えばよい（started_at が NULL の行がある場合と同じ扱いになる）。
-- name: ListStartedReservationIDs :many
SELECT reservation_id FROM recordings
WHERE site = $1
  AND reservation_id = ANY(sqlc.arg(reservation_ids)::bigint[])
  AND started_at IS NOT NULL;
