-- 容量超過の判定に使う需要の読み出し (issue #24 M2-10, issue #21 / docs/data.md §6.5)。
--
-- 需要の単位は予約件数ではなく**異なる物理チャンネル数**なので、
-- `(channel_type, channel)` を返す。**使い捨ての EPG 射影には JOIN しない** ---
-- 射影が刈られた/欠損した瞬間に容量判定が壊れる（docs/data.md §6.5 末尾）。
-- 予約時に焼き付けたスナップショット列（00009_reservation_channel.sql）を読む。
--
-- 絞り込みの分担:
--   - `orphaned_at IS NULL`: 番組が終了して schedule を作る意味がない行を落とす
--     （#28/#30 で state <> 'orphaned' から置き換え）。これ以上のフィルタに
--     使ってはならない（docs/schema.md §3。active / detached はどちらも同期対象）
--   - `channel_type IS NOT NULL AND channel IS NOT NULL`: 00009 以前の残骸は
--     物理チャンネルが分からないので需要に数えられない。数えない側に倒すのは、
--     既知の盲点をすべて「警告を見逃す」方向に揃えるため（docs/data.md §6.5）
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
  AND r.orphaned_at IS NULL
  AND s.channel_type IS NOT NULL
  AND s.channel IS NOT NULL
ORDER BY s.start_at;
