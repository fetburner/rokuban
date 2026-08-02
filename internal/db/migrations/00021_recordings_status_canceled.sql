-- +goose Up

-- issue #130: 実 mirakc に繋いだシャドー運用で、watcher が
-- `recording.status = "canceled"` を持つ record を取り込めず、同一トランザクション
-- 内の recordings_status_check 違反 → ロールバック → record_sync に観測が残らない
-- → 永久リトライ、という壊れ方をしていた。
--
-- mirakc の RecordingStatus は 4 バリアントの網羅的 enum
-- （mirakc-core/src/recording.rs、Web 型への変換も網羅的 match）で、ワイヤに出る
-- 値は recording / finished / canceled / failed の 4 つで閉じていることを
-- ソースで確認した上で、CHECK をこの 4 値に張り替える。
--
-- 既存行の移行は不要（canceled の行は CHECK 違反で 1 件も INSERT できていない
-- ため、旧 3 値の行しか存在しない）。
ALTER TABLE recordings
    DROP CONSTRAINT recordings_status_check;

ALTER TABLE recordings
    ADD CONSTRAINT recordings_status_check
        CHECK (status IN ('recording', 'finished', 'canceled', 'failed'));

-- +goose Down

ALTER TABLE recordings
    DROP CONSTRAINT IF EXISTS recordings_status_check;

ALTER TABLE recordings
    ADD CONSTRAINT recordings_status_check
        CHECK (status IN ('recording', 'finished', 'failed'));
