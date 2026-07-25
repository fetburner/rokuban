-- name: ListRules :many
SELECT * FROM rules
ORDER BY priority DESC, id ASC;

-- name: GetRule :one
SELECT * FROM rules WHERE id = $1;

-- name: CreateRule :one
INSERT INTO rules (
    name, description, enabled, priority,
    is_free, duration_min_ms, duration_max_ms, period_start_at, period_end_at,
    dedupe_enabled, dedupe_threshold, dedupe_window,
    keep_original, encode_profiles, filename_template, metadata
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8, $9,
    $10, $11, $12,
    $13, $14, $15, $16
)
RETURNING *;

-- name: UpdateRule :one
UPDATE rules SET
    name              = sqlc.arg(name),
    description       = sqlc.arg(description),
    enabled           = sqlc.arg(enabled),
    priority          = sqlc.arg(priority),
    is_free           = sqlc.narg(is_free),
    duration_min_ms   = sqlc.narg(duration_min_ms),
    duration_max_ms   = sqlc.narg(duration_max_ms),
    period_start_at   = sqlc.narg(period_start_at),
    period_end_at     = sqlc.narg(period_end_at),
    dedupe_enabled    = sqlc.arg(dedupe_enabled),
    dedupe_threshold  = sqlc.narg(dedupe_threshold),
    dedupe_window     = sqlc.narg(dedupe_window),
    keep_original     = sqlc.arg(keep_original),
    encode_profiles   = sqlc.arg(encode_profiles),
    filename_template = sqlc.arg(filename_template),
    metadata          = sqlc.arg(metadata),
    updated_at        = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteRule :execrows
DELETE FROM rules WHERE id = $1;

-- name: ListRuleTextMatches :many
SELECT * FROM rule_text_matches
WHERE rule_id = $1
ORDER BY seq;

-- name: InsertRuleTextMatch :exec
INSERT INTO rule_text_matches (rule_id, seq, target, mode, value, case_sensitive, negate)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: DeleteRuleTextMatches :exec
DELETE FROM rule_text_matches WHERE rule_id = $1;

-- name: ListRuleServices :many
SELECT * FROM rule_services
WHERE rule_id = $1
ORDER BY network_id, service_id;

-- name: InsertRuleService :exec
INSERT INTO rule_services (rule_id, network_id, service_id)
VALUES ($1, $2, $3);

-- name: DeleteRuleServices :exec
DELETE FROM rule_services WHERE rule_id = $1;

-- name: ListRuleChannelTypes :many
SELECT * FROM rule_channel_types
WHERE rule_id = $1
ORDER BY channel_type;

-- name: InsertRuleChannelType :exec
INSERT INTO rule_channel_types (rule_id, channel_type)
VALUES ($1, $2);

-- name: DeleteRuleChannelTypes :exec
DELETE FROM rule_channel_types WHERE rule_id = $1;

-- name: ListRuleGenres :many
SELECT * FROM rule_genres
WHERE rule_id = $1
ORDER BY genre_lv1;

-- name: InsertRuleGenre :exec
INSERT INTO rule_genres (rule_id, genre_lv1)
VALUES ($1, $2);

-- name: DeleteRuleGenres :exec
DELETE FROM rule_genres WHERE rule_id = $1;

-- name: ListRuleTimes :many
SELECT * FROM rule_times
WHERE rule_id = $1
ORDER BY seq;

-- name: InsertRuleTime :exec
INSERT INTO rule_times (rule_id, seq, weekdays, start_sec, end_sec)
VALUES ($1, $2, $3, $4, $5);

-- name: DeleteRuleTimes :exec
DELETE FROM rule_times WHERE rule_id = $1;

-- name: ListRuleSites :many
SELECT * FROM rule_sites
WHERE rule_id = $1
ORDER BY site;

-- name: InsertRuleSite :exec
INSERT INTO rule_sites (rule_id, site)
VALUES ($1, $2);

-- name: DeleteRuleSites :exec
DELETE FROM rule_sites WHERE rule_id = $1;

-- name: CountReservationsByRuleID :one
SELECT count(*)::bigint AS count
FROM reservations
WHERE rule_id = sqlc.arg(rule_id);

-- name: DeleteReservationsByRuleWithoutOverrides :execrows
-- ルール削除時: overrides のない導出予約を物理削除する
DELETE FROM reservations
WHERE rule_id = sqlc.arg(rule_id)
  AND overrides = '{}'::jsonb;

-- name: DetachReservationsByRule :execrows
-- ルール削除時: overrides 付き予約を detached 化して rule_id を外す
UPDATE reservations
SET state      = 'detached',
    source     = 'manual',
    rule_id    = NULL,
    updated_at = now()
WHERE rule_id = sqlc.arg(rule_id);

-- name: ValidateRegexPattern :exec
-- POSIX ARE としてコンパイルできるか検証する。不正パターンはエラーになる。
SELECT '' ~ $1;
