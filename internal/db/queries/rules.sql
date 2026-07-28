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

-- name: DeleteReservationsByRuleWithoutIntent :execrows
-- ルール削除時: ユーザーの投資（program_intents / program_overrides のどちらか）
-- がない導出予約を物理削除する。投資がある予約は残す（FK の ON DELETE SET NULL
-- で rule_id が外れ、base が凍結されたまま実質 manual として動く = detached）。
--
-- M2-4 で overrides を program_intents から program_overrides に分離したため、
-- 「ルール由来の予約に PATCH しただけ（program_overrides のみ、program_intents
-- には行なし）」という状態が構造的にありうる（docs/recording.md §4.2「overrides
-- は program_intents とは別の表に置く」）。program_intents だけを見ると
-- この投資を見落として誤って物理削除してしまうため、両方を確認する
-- （§4.3「意図または上書きがある → 削除せず detached で保持」）。
DELETE FROM reservations r
WHERE r.rule_id = sqlc.arg(rule_id)
  AND NOT EXISTS (
      SELECT 1 FROM program_intents i
      WHERE i.site = r.site AND i.program_id = r.program_id
  )
  AND NOT EXISTS (
      SELECT 1 FROM program_overrides o
      WHERE o.site = r.site AND o.program_id = r.program_id
  );

-- name: CountReservationsByRuleWithIntent :one
-- ルール削除時の内訳表示用（detached 化される件数）。上の
-- DeleteReservationsByRuleWithoutIntent と対になる条件（意図または上書きの
-- どちらかがある）。
SELECT count(*) FROM reservations r
WHERE r.rule_id = sqlc.arg(rule_id)
  AND (
      EXISTS (
          SELECT 1 FROM program_intents i
          WHERE i.site = r.site AND i.program_id = r.program_id
      )
      OR EXISTS (
          SELECT 1 FROM program_overrides o
          WHERE o.site = r.site AND o.program_id = r.program_id
      )
  );
-- name: ValidateRegexPattern :exec
-- POSIX ARE としてコンパイルできるか検証する。不正パターンはエラーになる。
SELECT '' ~ $1;
