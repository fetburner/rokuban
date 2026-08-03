-- +goose Up

-- 「完全削除が完了した」という不可逆な事実（issue #135）。
--
-- 削除 reconcile は media_assets しか触らず recordings 行には手を付けない
-- （docs/storage.md §7「物理削除後も tombstone は残る」）。一方でごみ箱ビュー
-- （ListTrashRecordings）は deleted_at IS NOT NULL だけを条件にしていたため、
-- 完全削除が終わった録画がごみ箱から永久に消えなかった。
--
-- 「media_assets に state <> 'deleted' が 0 行」を毎パス導出する案は採らない
-- （CLAUDE.md 不変条件 9）: アセットを一度も持ったことがない録画（status='failed'
-- で ingest まで到達しなかった行など）ではこの条件が purge 前から真であり、
-- 「消した」と「元から無い」を区別できない。不可逆な事実は列に持つ。
ALTER TABLE recordings
    ADD COLUMN purged_at timestamptz;

-- ごみ箱一覧（ListTrashRecordings）が「purged_at IS NULL」で引く側の部分索引。
CREATE INDEX recordings_purged_at_idx
    ON recordings (purged_at) WHERE purged_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS recordings_purged_at_idx;
ALTER TABLE recordings DROP COLUMN IF EXISTS purged_at;
