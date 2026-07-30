-- +goose Up

-- 削除 reconcile の孤児回収が使う観測記録（issue #70、docs/storage.md §7）。
--
-- 「DB に無いファイル」は削除の必要条件であって十分条件ではない
-- （DB リストア直後は全ファイルが孤児に見えるため）。first_seen を記録し、
-- 一定期間（既定 14 日）連続で孤児であり続けたものだけを削除対象にする。
-- DB リストアで行ごと失われるため、エイジングの窓も自動的に開き直る。
CREATE TABLE orphan_files (
    rel_path   text PRIMARY KEY,
    first_seen timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE orphan_files;
