-- 容量超過の判定に使う需要の読み出し (issue #24 M2-10, issue #21 / docs/data.md §6.5)。
--
-- 需要の単位は予約件数ではなく**異なる物理チャンネル数**なので、
-- `(channel_type, channel)` を返す。**使い捨ての EPG 射影には JOIN しない** ---
-- 射影が刈られた/欠損した瞬間に容量判定が壊れる（docs/data.md §6.5 末尾）。
-- 予約時に焼き付けたスナップショット列（00009_reservation_channel.sql）を読む。
--
-- 絞り込みの分担:
--   - never-scheduled 除外: reconciler が「番組終了かつ schedule 非観測」と
--     一度判定して recordings に never-scheduled 行を作った予約は、以後
--     schedule を作らない（= 需要にならない）ので落とす。旧実装は
--     orphaned_at IS NULL で絞っていたが、#98 でこの列を廃止し recordings の
--     試行行に置き換えた。述語自体は never_scheduled_events view
--     （issue #157。internal/db/migrations/00030_never_scheduled_events_view.sql）
--     に一本化し、ListReservationsForSyncEvaluation
--     （internal/db/queries/reservations.sql）と全く同じ NOT EXISTS になる ---
--     status='failed' 全般ではなく never-scheduled マーカーだけを見る理由も
--     同所のコメント参照（mirakc 由来の途中失敗からの再試行経路を壊さない
--     ため）。これ以上のフィルタに使ってはならない（docs/schema.md §3。
--     active / detached はどちらも同期対象）
--   - `channel_type IS NOT NULL AND channel IS NOT NULL` という絞り込みはかつて
--     ここにあった（00009 以前の残骸は物理チャンネルが分からないので需要に
--     数えられない、という安全側の判断）。issue #101（00026）で
--     program_snapshots のチャンネル・イベント識別 6 列が NOT NULL 化され、
--     その状態自体が表現不可能になったため落とした（起きない状態のための
--     分岐を残さない）
--   - `effective.skip` は jsonb のマージが要るので Go 側（db.EffectiveOptions）で
--     判定する。ListOverlappingReservations / ListReservationsForSyncEvaluation と同じ分担
--
-- 番組の開始時刻・尺・チャンネル識別は program_snapshots に移設された（#27）ので
-- JOIN して引く。
--
-- 地平線（8 日）で切らずに全件返す。予約集合はローリングウィンドウ（ruler の GC）で
-- 既に有界であり、docs/data.md §6.5 は「窓ごとに解かず地平線全体を 1 回解く」を
-- 指定している。窓で切ると窓の境界を跨ぐ予約の扱いが要り、結合済み区間の端が
-- 窓に依存してしまう。
-- name: ListCapacityDemand :many
SELECT
    r.site,
    s.channel_type,
    s.channel,
    s.start_at AS program_start_at,
    -- ::timestamptz の明示キャストが必要。付けないと sqlc が timestamptz + interval の
    -- 型を推論できず、この列を int32 として生成する（Scan で必ず落ちる）。
    (s.start_at + (s.duration_ms * interval '1 millisecond'))::timestamptz AS program_end_at,
    r.base,
    i.action    AS intent_action,
    o.overrides AS overrides
FROM reservations r
JOIN program_snapshots s ON s.site = r.site AND s.program_id = r.program_id
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE r.site = $1
  AND NOT EXISTS (
      SELECT 1 FROM never_scheduled_events nse
      -- 宛先のキーは**放送イベント**であって予約 id ではない。
      -- reservations.id は ruler の導出削除・再実体化で変わる不安定な値で
      -- （#53 が mirakc の tag を program:{programId} に移した理由。#99 も同じ）、
      -- recordings.reservation_id（issue #158 で列自体を削除済み）は当時 ON DELETE SET NULL だった。予約 id で
      -- 引くと、EPG フリッカーやルール編集で予約行が作り直された瞬間に
      -- 「never-scheduled 行が無い」ことになり、終了済み予約が毎パス desired に
      -- 戻り続ける（CLAUDE.md 不変条件 9 の identity: 導出器が作るキーを
      -- 宛先にしない）。
      WHERE nse.site = r.site
        AND nse.network_id = s.network_id
        AND nse.service_id = s.service_id
        AND nse.event_id = s.event_id
  )
ORDER BY s.start_at;

-- ListCapacityDemand と同じ絞り込みだが site で絞らない全サイト版。
-- GET /api/capacity/overages が使う（issue #184 M4-12）。判定はサイトごとに
-- 独立に行われる（internal/capacity.Compute が r.site で group する）ので、
-- ここで全サイト分の需要をまとめて返してもサイト間の需要は混ざらない。
-- worker/tuner.go の定期ジョブは束縛サイト 1 つ分だけを扱えばよいので
-- ListCapacityDemand（site 絞り込みあり）を使い続ける。
-- name: ListCapacityDemandAllSites :many
SELECT
    r.site,
    s.channel_type,
    s.channel,
    s.start_at AS program_start_at,
    (s.start_at + (s.duration_ms * interval '1 millisecond'))::timestamptz AS program_end_at,
    r.base,
    i.action    AS intent_action,
    o.overrides AS overrides
FROM reservations r
JOIN program_snapshots s ON s.site = r.site AND s.program_id = r.program_id
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE NOT EXISTS (
      SELECT 1 FROM never_scheduled_events nse
      WHERE nse.site = r.site
        AND nse.network_id = s.network_id
        AND nse.service_id = s.service_id
        AND nse.event_id = s.event_id
  )
ORDER BY r.site, s.start_at;
