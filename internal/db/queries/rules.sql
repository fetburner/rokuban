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

-- name: DeleteReservationsByRuleWithoutIntent :execrows
-- ルール削除時: ユーザーの投資（program_investments view。program_intents の
-- action='record' 行 ∪ program_overrides の行）がない導出予約を物理削除する。
-- 投資がある予約は残す（FK の ON DELETE SET NULL で rule_id が外れ、base が
-- 凍結されたまま実質 manual として動く = detached）。
--
-- program_intents 側は action = 'record' に限定する（#162。ruler.sql の
-- DeleteReservationsBySiteAndProgramIDs と同じ理由づけ）。action を限定しないと
-- intent{skip} だけの予約行が「detached として残る」と数えられるが、直後の
-- ruler パス（DeleteRule が同一 tx でヒントを投入するので数秒後）で
-- program_investments に含まれない行として導出削除され、内訳表示が
-- 数秒で消える行を「detached になった」と数える不整合を生む。
--
-- program_overrides 側は中身を問わず行の存在だけで desired/detached に残る設計
-- （docs/recording/reservation-model.md §4.2「ruler から見た load-bearing な行」）
-- なので、action と無関係に program_investments に含まれる。
DELETE FROM reservations r
WHERE r.rule_id = sqlc.arg(rule_id)
  AND NOT EXISTS (
      SELECT 1 FROM program_investments v
      WHERE v.site = r.site AND v.program_id = r.program_id
  );

-- name: CountReservationsByRuleWithIntent :one
-- ルール削除時の内訳表示用（detached 化される件数）。上の
-- DeleteReservationsByRuleWithoutIntent と対になる条件（program_investments に
-- 含まれる = record 意図または上書きがある）。
SELECT count(*) FROM reservations r
WHERE r.rule_id = sqlc.arg(rule_id)
  AND EXISTS (
      SELECT 1 FROM program_investments v
      WHERE v.site = r.site AND v.program_id = r.program_id
  );
-- name: ValidateRegexPattern :exec
-- POSIX ARE としてコンパイルできるか検証する。不正パターンはエラーになる。
SELECT '' ~ $1;
