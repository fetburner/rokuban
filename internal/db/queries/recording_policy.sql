-- ingest が recordings.keep_original / encode_profiles を焼くために読む実効オプション
-- （M3-14、issue #103）。reservations.sql は #52（M2 出口基準の並走）の間は触らない
-- 方針のため、読み取り専用のこのクエリを新規ファイルに切る
-- （CLAUDE.md「クエリファイルは新規に切る」）。
--
-- reservations.sql の GetReservationFull と似ているが、program_snapshots への
-- INNER JOIN をしない。ingest がここで必要なのは base / overrides / intent の
-- 3 つだけで、番組の事実のスナップショット（title 等）は要らない。
-- この 2 つの jsonb（base / overrides）を扱う箇所は db.EffectiveOptions を通す
-- （CLAUDE.md 不変条件 9/12 の教訓）。呼び出し側（internal/worker/ingest.go）の責務。
-- name: GetReservationEncodePolicy :one
SELECT sqlc.embed(r), i.action AS intent_action, o.overrides AS overrides
FROM reservations r
LEFT JOIN program_intents   i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE r.id = $1;
