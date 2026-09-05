-- ingest が recording_encode_policy 衛星表（issue #159。旧 recordings.keep_original /
-- encode_profiles）を焼くために読む実効オプション（M3-14、issue #103）。
-- reservations.sql は #52（M2 出口基準の並走）の間は触らない方針のため、
-- 読み取り専用のこのクエリを新規ファイルに切る（CLAUDE.md「クエリファイルは
-- 新規に切る」）。
--
-- reservations.sql の GetReservationFull と似ているが、program_snapshots は
-- 「どの予約を引くか」を決める結合キーとしてだけ使う。番組の事実のスナップ
-- ショット自体（title 等）はここでは要らない。この 2 つの jsonb（base /
-- overrides）を扱う箇所は reservation.EffectiveOptions を通す（CLAUDE.md 不変条件
-- 9/12 の教訓）。呼び出し側（internal/worker/ingest.go）の責務。
--
-- 宛先は recordings.reservation_id（bigint FK、ON DELETE SET NULL だった。
-- issue #158 で列自体を削除済み）ではなく放送イベントキー
-- (site, network_id, service_id, event_id) --- recordings が
-- 生まれたときから凍結して持つ列で、ruler の導出削除・再実体化で
-- 予約行が入れ替わっても値が変わらない（issue #149。CLAUDE.md 不変条件
-- 9「identity」: 導出器 ruler が作る予約行のキーを、FK 経由の参照にも使わない）。
-- program_snapshots で (network_id, service_id, event_id)
-- → program_id を引き、reservations を program_id で結合する。
--
-- program_snapshots は放送後 epg.retention_grace（既定 24h）で GC される寿命の
-- 短い表（docs/storage.md §6 参照）で、ingest は通常なら録画終了直後に走る。
-- この JOIN が失敗する原因は 3 つある:
--
--   1. そもそも予約が無い録画（手動起動）。日常的に起きるので呼び出し側は
--      :one の pgx.ErrNoRows を期待して分岐する
--   2. GC が想定より早く走った、または予約が恒久的に削除された
--   3. **GC は設計どおりに走ったが、ingest がエッジの滞留で猶予を跨いで遅れた**
--      （issue #214）。エッジのリングバッファは回線断・クラウド側障害での
--      N 日の滞留を前提にサイジングするので、これは異常系ではなく設計が
--      明示的に許容するシナリオ。docs/storage.md §6「凍結が依存する寿命と、
--      エッジの滞留の交点」
--
-- 3 を「ingest は録画終了直後に走るので起こらない」と書いていたのが #214 の
-- 発端なので、この列挙を「〜に限られる」の形に戻さない。
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
