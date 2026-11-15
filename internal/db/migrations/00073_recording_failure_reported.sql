-- +goose Up
-- watcher が記録する、録画ごとの不可逆な報告事実。
ALTER TABLE recordings ADD COLUMN failure_reported boolean NOT NULL DEFAULT false;

-- 既存ログの内容を読むのは、この一度きりの移行だけ。
UPDATE recordings r SET failure_reported = true
WHERE EXISTS (
    SELECT 1 FROM jsonb_array_elements(r.quality_events) e(value)
    WHERE e.value->>'event' = 'recording.failed'
);

-- +goose Down
ALTER TABLE recordings DROP COLUMN failure_reported;
