-- catalog エクスポート / rescue 用（M3-9 / issue #71）。
-- 保護対象はルール・録画・media_assets・drop_stats・意図・上書き（と意図の FK 先
-- program_snapshots）。EPG 射影と schedule/record/tuner_sync は再構築可能なので
-- 含めない（docs/storage.md §8）。

-- ---------------------------------------------------------------------------
-- List（export）
-- ---------------------------------------------------------------------------

-- name: CatalogListRules :many
SELECT * FROM rules ORDER BY id;

-- name: CatalogListRuleTextMatches :many
SELECT * FROM rule_text_matches ORDER BY rule_id, seq;

-- name: CatalogListRuleServices :many
SELECT * FROM rule_services ORDER BY rule_id, network_id, service_id;

-- name: CatalogListRuleChannelTypes :many
SELECT * FROM rule_channel_types ORDER BY rule_id, channel_type;

-- name: CatalogListRuleGenres :many
SELECT * FROM rule_genres ORDER BY rule_id, genre_lv1;

-- name: CatalogListRuleTimes :many
SELECT * FROM rule_times ORDER BY rule_id, seq;

-- name: CatalogListRuleSites :many
SELECT * FROM rule_sites ORDER BY rule_id, site;

-- site が NULL なら全件。tombstone（deleted_at IS NOT NULL）も含む。
-- name: CatalogListRecordings :many
SELECT * FROM recordings
WHERE sqlc.narg('site')::text IS NULL OR site = sqlc.narg('site')
ORDER BY id;

-- name: CatalogListMediaAssets :many
SELECT a.*
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE sqlc.narg('site')::text IS NULL OR r.site = sqlc.narg('site')
ORDER BY a.id;

-- name: CatalogListDropStats :many
SELECT d.*
FROM drop_stats d
JOIN media_assets a ON a.id = d.media_asset_id
JOIN recordings r ON r.id = a.recording_id
WHERE sqlc.narg('site')::text IS NULL OR r.site = sqlc.narg('site')
ORDER BY d.media_asset_id, d.pid;

-- 意図・上書きの FK 先。export 対象の (site, program_id) に限定する。
-- name: CatalogListProgramSnapshots :many
SELECT s.*
FROM program_snapshots s
WHERE (sqlc.narg('site')::text IS NULL OR s.site = sqlc.narg('site'))
  AND (
      EXISTS (
          SELECT 1 FROM program_intents i
          WHERE i.site = s.site AND i.program_id = s.program_id
      )
      OR EXISTS (
          SELECT 1 FROM program_overrides o
          WHERE o.site = s.site AND o.program_id = s.program_id
      )
  )
ORDER BY s.site, s.program_id;

-- name: CatalogListProgramIntents :many
SELECT * FROM program_intents
WHERE sqlc.narg('site')::text IS NULL OR site = sqlc.narg('site')
ORDER BY site, program_id;

-- name: CatalogListProgramOverrides :many
SELECT * FROM program_overrides
WHERE sqlc.narg('site')::text IS NULL OR site = sqlc.narg('site')
ORDER BY site, program_id;

-- ---------------------------------------------------------------------------
-- Upsert（rescue）。id を保持するため OVERRIDING SYSTEM VALUE を使う。
-- ---------------------------------------------------------------------------

-- name: CatalogUpsertRule :exec
INSERT INTO rules (
    id, name, description, enabled, priority,
    is_free, duration_min_ms, duration_max_ms, period_start_at, period_end_at,
    dedupe_enabled, dedupe_threshold, dedupe_window,
    keep_original, encode_profiles, filename_template, metadata,
    created_at, updated_at
) OVERRIDING SYSTEM VALUE
VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10,
    $11, $12, $13,
    $14, $15, $16, $17,
    $18, $19
)
ON CONFLICT (id) DO UPDATE SET
    name              = EXCLUDED.name,
    description       = EXCLUDED.description,
    enabled           = EXCLUDED.enabled,
    priority          = EXCLUDED.priority,
    is_free           = EXCLUDED.is_free,
    duration_min_ms   = EXCLUDED.duration_min_ms,
    duration_max_ms   = EXCLUDED.duration_max_ms,
    period_start_at   = EXCLUDED.period_start_at,
    period_end_at     = EXCLUDED.period_end_at,
    dedupe_enabled    = EXCLUDED.dedupe_enabled,
    dedupe_threshold  = EXCLUDED.dedupe_threshold,
    dedupe_window     = EXCLUDED.dedupe_window,
    keep_original     = EXCLUDED.keep_original,
    encode_profiles   = EXCLUDED.encode_profiles,
    filename_template = EXCLUDED.filename_template,
    metadata          = EXCLUDED.metadata,
    created_at        = EXCLUDED.created_at,
    updated_at        = EXCLUDED.updated_at;

-- name: CatalogDeleteRuleTextMatches :exec
DELETE FROM rule_text_matches WHERE rule_id = $1;

-- name: CatalogDeleteRuleServices :exec
DELETE FROM rule_services WHERE rule_id = $1;

-- name: CatalogDeleteRuleChannelTypes :exec
DELETE FROM rule_channel_types WHERE rule_id = $1;

-- name: CatalogDeleteRuleGenres :exec
DELETE FROM rule_genres WHERE rule_id = $1;

-- name: CatalogDeleteRuleTimes :exec
DELETE FROM rule_times WHERE rule_id = $1;

-- name: CatalogDeleteRuleSites :exec
DELETE FROM rule_sites WHERE rule_id = $1;

-- name: CatalogInsertRuleTextMatch :exec
INSERT INTO rule_text_matches (rule_id, seq, target, mode, value, case_sensitive, negate)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (rule_id, seq) DO UPDATE SET
    target         = EXCLUDED.target,
    mode           = EXCLUDED.mode,
    value          = EXCLUDED.value,
    case_sensitive = EXCLUDED.case_sensitive,
    negate         = EXCLUDED.negate;

-- name: CatalogInsertRuleService :exec
INSERT INTO rule_services (rule_id, network_id, service_id)
VALUES ($1, $2, $3)
ON CONFLICT (rule_id, network_id, service_id) DO NOTHING;

-- name: CatalogInsertRuleChannelType :exec
INSERT INTO rule_channel_types (rule_id, channel_type)
VALUES ($1, $2)
ON CONFLICT (rule_id, channel_type) DO NOTHING;

-- name: CatalogInsertRuleGenre :exec
INSERT INTO rule_genres (rule_id, genre_lv1)
VALUES ($1, $2)
ON CONFLICT (rule_id, genre_lv1) DO NOTHING;

-- name: CatalogInsertRuleTime :exec
INSERT INTO rule_times (rule_id, seq, weekdays, start_sec, end_sec)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (rule_id, seq) DO UPDATE SET
    weekdays  = EXCLUDED.weekdays,
    start_sec = EXCLUDED.start_sec,
    end_sec   = EXCLUDED.end_sec;

-- name: CatalogInsertRuleSite :exec
INSERT INTO rule_sites (rule_id, site)
VALUES ($1, $2)
ON CONFLICT (rule_id, site) DO NOTHING;

-- event_id / service_name は issue #98 で追加（00025）。never-scheduled 行
-- （recordings）の識別・表示名に使うので、他のチャンネル識別列と同じく
-- catalog の往復（export/rescue）で失ってはならない。
-- name: CatalogUpsertProgramSnapshot :exec
INSERT INTO program_snapshots (
    site, program_id, title, start_at, duration_ms,
    network_id, service_id, channel_type, channel, event_id, service_name, updated_at
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10, $11, $12
)
ON CONFLICT (site, program_id) DO UPDATE SET
    title        = EXCLUDED.title,
    start_at     = EXCLUDED.start_at,
    duration_ms  = EXCLUDED.duration_ms,
    network_id   = EXCLUDED.network_id,
    service_id   = EXCLUDED.service_id,
    channel_type = EXCLUDED.channel_type,
    channel      = EXCLUDED.channel,
    event_id     = EXCLUDED.event_id,
    service_name = EXCLUDED.service_name,
    updated_at   = EXCLUDED.updated_at;

-- name: CatalogUpsertProgramIntent :exec
INSERT INTO program_intents (
    site, program_id, action, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (site, program_id) DO UPDATE SET
    action     = EXCLUDED.action,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at;

-- name: CatalogUpsertProgramOverride :exec
INSERT INTO program_overrides (
    site, program_id, overrides, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (site, program_id) DO UPDATE SET
    overrides  = EXCLUDED.overrides,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at;

-- reservation_id は reservations を export しないので常に NULL で入れる。
-- name: CatalogUpsertRecording :exec
INSERT INTO recordings (
    id, reservation_id, rule_id, source, site,
    network_id, service_id, event_id, service_name,
    channel_type, channel, title, description,
    extended, genres, is_free,
    program_start_at, program_duration_ms,
    status, started_at, ended_at,
    keep_original, encode_profiles, quality_events,
    deleted_at, purge_after, superseded_at, purged_at, created_at, updated_at
) OVERRIDING SYSTEM VALUE
VALUES (
    $1, NULL, $2, $3, $4,
    $5, $6, $7, $8,
    $9, $10, $11, $12,
    $13, $14, $15,
    $16, $17,
    $18, $19, $20,
    $21, $22, $23,
    $24, $25, $26, $27, $28, $29
)
ON CONFLICT (id) DO UPDATE SET
    reservation_id      = NULL,
    rule_id             = EXCLUDED.rule_id,
    source              = EXCLUDED.source,
    site                = EXCLUDED.site,
    network_id          = EXCLUDED.network_id,
    service_id          = EXCLUDED.service_id,
    event_id            = EXCLUDED.event_id,
    service_name        = EXCLUDED.service_name,
    channel_type        = EXCLUDED.channel_type,
    channel             = EXCLUDED.channel,
    title               = EXCLUDED.title,
    description         = EXCLUDED.description,
    extended            = EXCLUDED.extended,
    genres              = EXCLUDED.genres,
    is_free             = EXCLUDED.is_free,
    program_start_at    = EXCLUDED.program_start_at,
    program_duration_ms = EXCLUDED.program_duration_ms,
    status              = EXCLUDED.status,
    started_at          = EXCLUDED.started_at,
    ended_at            = EXCLUDED.ended_at,
    keep_original       = EXCLUDED.keep_original,
    encode_profiles     = EXCLUDED.encode_profiles,
    quality_events      = EXCLUDED.quality_events,
    deleted_at          = EXCLUDED.deleted_at,
    purge_after         = EXCLUDED.purge_after,
    -- superseded_at を落とすと、復旧時に superseded 行が live に戻って
    -- recordings_unique_active_event に衝突する（issue #129 症状 2）。
    superseded_at       = EXCLUDED.superseded_at,
    -- purged_at を落とすと、purge 済みの tombstone がごみ箱ビューに再び
    -- 出てしまう（issue #135。ListTrashRecordings は purged_at IS NULL を
    -- 要求する）。
    purged_at           = EXCLUDED.purged_at,
    created_at          = EXCLUDED.created_at,
    updated_at          = EXCLUDED.updated_at;

-- name: CatalogUpsertMediaAsset :exec
INSERT INTO media_assets (
    id, recording_id, kind, profile, rel_path, size_bytes,
    state, deleted_at, created_at, updated_at
) OVERRIDING SYSTEM VALUE
VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10
)
ON CONFLICT (id) DO UPDATE SET
    recording_id = EXCLUDED.recording_id,
    kind         = EXCLUDED.kind,
    profile      = EXCLUDED.profile,
    rel_path     = EXCLUDED.rel_path,
    size_bytes   = EXCLUDED.size_bytes,
    state        = EXCLUDED.state,
    deleted_at   = EXCLUDED.deleted_at,
    created_at   = EXCLUDED.created_at,
    updated_at   = EXCLUDED.updated_at;

-- name: CatalogUpsertDropStat :exec
INSERT INTO drop_stats (
    media_asset_id, pid, packets, drops, errors, scrambled, pid_type
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (media_asset_id, pid) DO UPDATE SET
    packets   = EXCLUDED.packets,
    drops     = EXCLUDED.drops,
    errors    = EXCLUDED.errors,
    scrambled = EXCLUDED.scrambled,
    pid_type  = EXCLUDED.pid_type;

-- IDENTITY 列のシーケンスを max(id) に揃える（再 insert で衝突しないように）。
-- name: CatalogResetRulesIDSeq :exec
SELECT setval(
    pg_get_serial_sequence('rules', 'id'),
    GREATEST(COALESCE((SELECT MAX(id) FROM rules), 1), 1)
);

-- name: CatalogResetRecordingsIDSeq :exec
SELECT setval(
    pg_get_serial_sequence('recordings', 'id'),
    GREATEST(COALESCE((SELECT MAX(id) FROM recordings), 1), 1)
);

-- name: CatalogResetMediaAssetsIDSeq :exec
SELECT setval(
    pg_get_serial_sequence('media_assets', 'id'),
    GREATEST(COALESCE((SELECT MAX(id) FROM media_assets), 1), 1)
);
