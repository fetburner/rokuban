-- issue #148: schedule_sync.reservation_id は書き手（reconciler.observeSchedules）
-- はいるが読み手が 1 つも無い列だった。この列を含む唯一の SELECT である
-- ListScheduleSyncsBySite は呼び出し元ゼロで、reconciler の「自分が作った
-- schedule か」の判定は常に tags = mirakc.IsOurs で行われている。
--
-- issue #99 は「FK（ON DELETE SET NULL）の要否を再検討できる」と書いたが、
-- PR #147 のレビューで FK だけを外す案は取り下げられた -- 外すとこの列は
-- 「削除済み予約を指す古い id」を持ちうるようになり、NULL より紛らわしく
-- なる（インシデント対応で直接 SELECT する人を誤らせる）。reservations.id は
-- ruler の導出削除・再実体化で変わる不安定な値（#53/#98/#99）であり、
-- 読み手のいない列にそれを保存し続ける理由が無いので、列自体を落とす
-- （CLAUDE.md 不変条件 10「意味を持たない行を作らない」/ 11「これを書く /
-- 使うコードは今あるか」）。
--
-- +goose Up
DROP INDEX IF EXISTS schedule_sync_reservation_id_idx;
ALTER TABLE schedule_sync DROP COLUMN reservation_id;

-- +goose Down

-- **片道であることを明示的に許容する**（schedule_sync は導出テーブルなので
-- 再構築できる。CLAUDE.md 不変条件 11「導出表は churn がほぼ無害」、
-- 00008/00010/00012/00017/00025 と同じ前例）。列を戻しても、失った
-- reservation_id の値（外部産と誤認していない observed schedule と
-- reservations 行の対応）は復元しない -- そもそも読み手が無かった値であり、
-- reconciler の次パスの observeSchedules がこの列自体を書かなくなっている
-- ため NULL のままになる。
ALTER TABLE schedule_sync ADD COLUMN reservation_id bigint REFERENCES reservations (id) ON DELETE SET NULL;
CREATE INDEX ON schedule_sync (reservation_id);
