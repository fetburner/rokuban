-- 開始遅延検出器（issue #24 M2-7、docs/recording.md §3.3「開始遅延検出器」）。
--
-- reconciler.detectStartDelays は listDesired が返した desired 予約
-- （effective.skip 済み除外、never-scheduled 済み除外。issue #98 で
-- state <> 'orphaned' から置き換わった）のうち「開始時刻 +
-- 猶予 < now() < 終了時刻」の窓にある予約の放送イベントキー
-- (network_id, service_id, event_id) をここに渡し、その中で
-- recordings.started_at が埋まっている（= watcher が mirakc の record から
-- 観測済み）ものだけを引く。渡したキーからここで返ったキーを除いた差集合が
-- 「開始時刻を過ぎたのに録画開始が観測されていない」候補になる。
--
-- **宛先のキーは放送イベントであって予約 id ではない**（issue #152。
-- CLAUDE.md 不変条件 9 の identity、#29 / #53 / #98 / #99 / #149 と同じ族の
-- 6 例目）。reservations.id は ruler の導出削除・再実体化で変わる不安定な値で、
-- recordings.reservation_id（issue #158 で列自体を削除済み）は当時 ON DELETE SET NULL だった。予約 id で引くと、
-- 録画中に EPG フリッカーやルール編集で予約行が作り直された瞬間に「started 済み
-- recordings 行が見つからない」ことになり、検出窓（開始 + 猶予 〜 終了）の間
-- 毎パス開始遅延を誤検知する。detectStartDelays の入力（listDesired の出力）は
-- program_snapshots を JOIN 済みで放送イベントキーを手元に持っているので、
-- そのキーで引く（never_recorded / ListReservationsForSyncEvaluation
-- （internal/db/queries/reservations.sql）が既に行っている置き換えと同じ形）。
--
-- 3 本の parallel array をタプルとして引くのに ANY / IN は使えない。
-- unnest(a, b, c) の多引数形は sqlc の組み込みアナライザ（実 DB 接続なしの
-- カタログ解析）が列型を解決できず `function unnest(unknown, unknown, unknown)
-- does not exist` で generate が失敗する（internal/db/queries/ruler.sql の
-- jsonb_to_recordset のコメントと同じ制約）。generate_subscripts + 添字アクセスは
-- 単一引数の組み込み関数だけで済むので、この制約に当たらない。
--
-- recordings 行そのものが無いキー（録画がまだ一切観測されていない）は当然
-- ここに出てこないので、呼び出し側では「返らなかったキー = 観測なし」として
-- 扱えばよい（started_at が NULL の行がある場合と同じ扱いになる）。
-- name: ListStartedBroadcastEventKeys :many
SELECT DISTINCT rec.network_id, rec.service_id, rec.event_id
FROM recordings rec
WHERE rec.site = $1
  AND rec.started_at IS NOT NULL
  AND EXISTS (
      SELECT 1 FROM generate_subscripts(sqlc.arg(network_ids)::integer[], 1) AS i
      WHERE (sqlc.arg(network_ids)::integer[])[i] = rec.network_id
        AND (sqlc.arg(service_ids)::integer[])[i] = rec.service_id
        AND (sqlc.arg(event_ids)::integer[])[i] = rec.event_id
  );
