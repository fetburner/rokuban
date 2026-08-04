-- issue #158: recordings.reservation_id（bigint FK、ON DELETE SET NULL）は
-- reservations.id を宛先にした結合キーで、reservations.id は ruler の導出
-- 削除・再実体化のたびに変わる不安定な値である（CLAUDE.md 不変条件 9
-- 「identity」）。この列を宛先に予約を引く実装は #29 / #53 / #98 / #99 /
-- #149 / #152 と 6 回同じ形のバグを生んだ --- 録画開始から後続処理が終わる
-- までの窓で ruler が予約を消して作り直すと FK が黙って NULL に落ち、
-- 「予約が無い」と誤認する。
--
-- #149（encode policy の解決）と #152（開始遅延検出）はすでに読み取りを
-- 放送イベントキー (site, network_id, service_id, event_id) 経由（前者は
-- program_snapshots を介して reservations.program_id に結合、後者は
-- program_snapshots が持つ同キーを直接引く）に置き換えた。残る読者は
-- internal/api/recordings.go の表示用コピーだけで、これも本 issue で
-- 同じキー経由に置き換える（Recording API からは reservationId を削除し、
-- ソースは openapi.yaml/生成物側で表現する）。internal/db/queries/catalog.sql
-- の import は元から reservation_id を NULL 固定で書いており
-- （「導出物なので export しない」と明記済み）、この変更の影響を受けない。
--
-- 列が存在する限り「便利な結合キー」としての引力を持ち続け、7 例目を防ぐ
-- のはレビューの運になる。列自体を落として間違いを表現不可能にする
-- （CLAUDE.md 不変条件 10）。副次的に、ruler の毎パスの導出削除のたびに
-- 走る ON DELETE SET NULL の FK メンテナンス（recordings 側インデックスの
-- 走査）も消える。
--
-- +goose Up
DROP INDEX IF EXISTS recordings_reservation_id_idx;
ALTER TABLE recordings DROP COLUMN reservation_id;

-- +goose Down

-- **片道であることを明示的に許容する**（recordings は永続資産の表だが、
-- reservation_id 自体は導出ポインタ --- 復元しても意味のある値を持てない。
-- CLAUDE.md 不変条件 11「導出表は churn がほぼ無害」、00008/00010/00012/
-- 00028 と同じ前例）。列を戻しても、失った reservation_id の値（当時の
-- reservations.id への対応）は復元しない。書き手（watcher / reconciler）は
-- もうこの列を埋めないので NULL のままになる。
ALTER TABLE recordings ADD COLUMN reservation_id bigint REFERENCES reservations (id) ON DELETE SET NULL;
CREATE INDEX ON recordings (reservation_id);
