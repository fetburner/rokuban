-- issue #129 症状 2: 同一 active-event (site, network_id, service_id, event_id) に
-- status='failed' の行が recordings_unique_active_event の枠を占有したまま残り、
-- 後から着地した成功 record の INSERT が一意制約違反で落ちる問題への対応。
--
-- superseded_at は「この行が active-event の枠を明け渡した」という不可逆な事実
-- だけを持つ列。ユーザーの「ごみ箱送り」を表す deleted_at とは意味が異なるため
-- 別列にした（CLAUDE.md 不変条件 9: 導出値/事実を同じ列に同居させない。ここは
-- 2 つとも不可逆な事実だが、意味の異なる 2 つの「消える理由」を 1 列に混ぜると
-- ごみ箱ビュー・GC がユーザー操作でない行をユーザー操作と誤読する）。
--
-- +goose Up
ALTER TABLE recordings ADD COLUMN superseded_at timestamptz;

DROP INDEX recordings_unique_active_event;
CREATE UNIQUE INDEX recordings_unique_active_event
    ON recordings (site, network_id, service_id, event_id)
    WHERE deleted_at IS NULL AND superseded_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS recordings_unique_active_event;
CREATE UNIQUE INDEX recordings_unique_active_event
    ON recordings (site, network_id, service_id, event_id)
    WHERE deleted_at IS NULL;

ALTER TABLE recordings DROP COLUMN superseded_at;
