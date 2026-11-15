-- +goose Up
-- CHECK の追加は ACCESS EXCLUSIVE を取る。既存行の検証とは別トランザクションにする。
-- NOT VALID なので既存の違反行があっても追加でき、新しい INSERT / UPDATE は検査する。
-- 後続の検証が制約違反（23514）で失敗しても、この追加は適用済みで残る。
-- 検証: TestPurgedRequiresDeletedMigrationsPreserveUnvalidatedConstraint。
-- ロックと検証の根拠: PostgreSQL 17 の ALTER TABLE の公開仕様。
-- https://www.postgresql.org/docs/17/sql-altertable.html
-- ロック待ちを含む所要時間は未計測。
ALTER TABLE recordings
    ADD CONSTRAINT recordings_purged_requires_deleted
    CHECK (purged_at IS NULL OR deleted_at IS NOT NULL) NOT VALID;

-- +goose Down
ALTER TABLE recordings DROP CONSTRAINT recordings_purged_requires_deleted;
