-- +goose Up
-- VALIDATE CONSTRAINT は SHARE UPDATE EXCLUSIVE を取り、既存行を検証する。
-- 追加の ACCESS EXCLUSIVE の下で検証しないよう、直前のマイグレーションと分ける。
-- 既存の違反行があると検証は制約違反（23514）で失敗する。
-- 直前のマイグレーションは適用済みで残り、新しい INSERT / UPDATE の検査は続く。
-- 検証: TestPurgedRequiresDeletedMigrationsPreserveUnvalidatedConstraint。
-- ロックと検証の根拠: PostgreSQL 17 の ALTER TABLE の公開仕様。
-- https://www.postgresql.org/docs/17/sql-altertable.html
-- ロック待ちを含む所要時間は未計測。
ALTER TABLE recordings VALIDATE CONSTRAINT recordings_purged_requires_deleted;

-- +goose Down
-- 検証を取り消す操作は無い。制約の削除は直前のマイグレーションの Down が行う。
