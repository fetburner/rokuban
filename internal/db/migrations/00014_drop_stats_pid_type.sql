-- +goose Up

-- M2-13: ドロップ統計に PID 種別（映像 / 音声 / 固定 PSI テーブル名）を記録する。
--
-- PAT/PMT の stream_type までしか読まない（記述子は読まない。
-- docs/recording.md「例外の境界」）。分類できない PID は NULL のままにする
-- （PSI 解析の失敗を ingest の失敗にしない。同ドキュメント）。
--
-- 値の集合は circuit_breakers.name と同じ理由で CHECK を置かない: 権威は
-- Go 側（internal/tsstat）にあり、既知の固定 PID 名（PAT/CAT/NIT/SDT/EIT/TOT）と
-- PMT の stream_type 分類（video/audio/other）が混在する開いた集合のため。
ALTER TABLE drop_stats
    ADD COLUMN pid_type text;

-- +goose Down

ALTER TABLE drop_stats
    DROP COLUMN IF EXISTS pid_type;
