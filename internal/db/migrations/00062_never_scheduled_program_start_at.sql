-- +goose Up

-- 旧行には番組の開始時刻が無く、観測時の program_snapshots が残っていても
-- program_id の再利用後は別番組の snapshot を指す可能性があるため、正しい開始時刻を
-- 安全に補完できない。イベント単位の欠測を新しい複合 identity に誤って移さないよう
-- 既存行を破棄する。これらは再 reconcile で必要なら再観測される。
DELETE FROM never_scheduled_events;

ALTER TABLE never_scheduled_events
    ADD COLUMN program_start_at timestamptz NOT NULL;

ALTER TABLE never_scheduled_events
    DROP CONSTRAINT never_scheduled_events_pkey,
    ADD PRIMARY KEY (site, network_id, service_id, event_id, program_start_at);

-- +goose Down

-- 旧主キーでは同じ放送イベントの複数開始時刻を表せないため、曖昧な行を残さず
-- 破棄する。必要な欠測は再 reconcile で再観測される。
DELETE FROM never_scheduled_events;

ALTER TABLE never_scheduled_events
    DROP CONSTRAINT never_scheduled_events_pkey,
    DROP COLUMN program_start_at,
    ADD PRIMARY KEY (site, network_id, service_id, event_id);
