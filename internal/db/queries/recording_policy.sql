-- ingest が recording_encode_policy 衛星表（issue #159。旧 recordings.keep_original /
-- encode_profiles）を焼くために読む実効オプション（M3-14、issue #103）。
-- reservations.sql は #52（M2 出口基準の並走）の間は触らない方針のため、
-- 読み取り専用のこのクエリを新規ファイルに切る（CLAUDE.md「クエリファイルは
-- 新規に切る」）。
--
-- reservations.sql の GetReservationFull と似ているが、program_snapshots は
-- 「どの予約を引くか」を決める結合キーとしてだけ使う。番組の事実のスナップ
-- ショット自体（title 等）はここでは要らない。この 2 つの jsonb（base /
-- overrides）を扱う箇所は db.EffectiveOptions を通す（CLAUDE.md 不変条件
-- 9/12 の教訓）。呼び出し側（internal/worker/ingest.go）の責務。
--
-- 宛先は recordings.reservation_id（bigint FK、ON DELETE SET NULL だった。
-- issue #158 で列自体を削除済み）ではなく放送イベントキー
-- (site, network_id, service_id, event_id) --- recordings が
-- 生まれたときから凍結して持つ列で、ruler の導出削除・再実体化で
-- reservations.id が変わっても値が変わらない（issue #149。CLAUDE.md 不変条件
-- 9「identity」: reservations.id は導出器 ruler が作るキーなので、FK 経由の
-- 参照にも使わない）。program_snapshots で (network_id, service_id, event_id)
-- → program_id を引き、reservations を program_id で結合する。
--
-- program_snapshots は放送後 GC される寿命の短い表（docs/storage.md §6 参照）
-- だが、ingest は録画終了直後に走るため、この JOIN が失敗するのは
-- 「そもそも予約が無い録画」（手動起動）か「GC が想定より早く走った」場合に
-- 限られる。前者は日常的に起きるので呼び出し側は :one の pgx.ErrNoRows を
-- 期待して分岐する。
-- name: GetReservationEncodePolicyByEvent :one
SELECT sqlc.embed(r), i.action AS intent_action, o.overrides AS overrides
FROM program_snapshots ps
JOIN reservations r ON r.site = ps.site AND r.program_id = ps.program_id
LEFT JOIN program_intents   i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE ps.site = sqlc.arg('site')
  AND ps.network_id = sqlc.arg('network_id')
  AND ps.service_id = sqlc.arg('service_id')
  AND ps.event_id = sqlc.arg('event_id');
