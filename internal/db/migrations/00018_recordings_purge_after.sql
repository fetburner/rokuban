-- +goose Up

-- 即時物理削除の要求印（M3-7 / issue #69）。
--
-- UI の「今すぐ完全削除」はファイルを消さず、この列を now() に立てるだけ。
-- 物理 unlink は M3-8 の削除 reconcile が `purge_after IS NOT NULL AND purge_after <= now()`
-- （または `deleted_at + 猶予`）を拾って実行する。
--
-- 猶予経過による通常 purge とは独立した「前倒し」の合図なので、deleted_at を
-- 十分過去に書き換える案は採らない（「いつごみ箱に入れたか」の履歴を壊すため）。
ALTER TABLE recordings
    ADD COLUMN purge_after timestamptz;

-- 削除 reconcile が即時対象を拾うための部分インデックス。
CREATE INDEX recordings_purge_after_idx
    ON recordings (purge_after) WHERE purge_after IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS recordings_purge_after_idx;
ALTER TABLE recordings DROP COLUMN IF EXISTS purge_after;
