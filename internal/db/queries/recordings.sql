-- name: CreateRecording :one
INSERT INTO recordings (
    reservation_id, rule_id, source, site,
    network_id, service_id, event_id, service_name,
    channel_type, channel, title, description,
    extended, genres, is_free,
    program_start_at, program_duration_ms,
    status, started_at, ended_at
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8,
    $9, $10, $11, $12,
    $13, $14, $15,
    $16, $17,
    $18, $19, $20
) RETURNING id;

-- name: UpdateRecordingStatus :exec
-- status は 'finished' / 'failed' / 'canceled' に達したら降格させない
-- （out-of-order な 'recording' イベントが後から来ても上書きしない。
-- 'canceled' は録画が再開しない取消なので他の 2 つと同じ終端として扱う。
-- issue #130）。
UPDATE recordings SET
    status     = CASE WHEN status IN ('finished', 'failed', 'canceled') THEN status ELSE sqlc.arg('new_status') END,
    started_at = COALESCE(started_at, sqlc.arg('started_at')),
    ended_at   = CASE WHEN sqlc.narg('ended_at')::timestamptz IS NOT NULL THEN sqlc.narg('ended_at') ELSE ended_at END,
    updated_at = now()
WHERE id = sqlc.arg('id');

-- name: CreateFailedRecording :exec
INSERT INTO recordings (
    reservation_id, rule_id, source, site,
    network_id, service_id, event_id, service_name,
    channel_type, channel, title, description,
    extended, genres, is_free,
    program_start_at, program_duration_ms,
    status, quality_events
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8,
    $9, $10, $11, $12,
    $13, $14, $15,
    $16, $17,
    'failed', $18
)
ON CONFLICT (site, network_id, service_id, event_id) WHERE deleted_at IS NULL
DO UPDATE SET
    quality_events = recordings.quality_events || EXCLUDED.quality_events,
    updated_at = now();

-- 録画一覧。原本のサイズと PID 別 drop_stats の合計、
-- 再生可能な encoded プロファイル名を同梱する。
-- PID 別の内訳は行数が多く一覧では使わないので ListRecordingDropStats で別に取る。
-- name: ListRecordings :many
SELECT
    r.*,
    a.size_bytes                        AS original_size_bytes,
    COALESCE(d.packets, 0)::bigint      AS drop_packets,
    COALESCE(d.drops, 0)::bigint        AS drop_drops,
    COALESCE(d.errors, 0)::bigint       AS drop_errors,
    COALESCE(d.scrambled, 0)::bigint    AS drop_scrambled,
    -- ブラウザ再生用。desired（r.encode_profiles）ではなく observed（active encoded）。
    -- sqlc は array_agg の型を推論しきれないことがあるので text[] に明示キャストする。
    (
        SELECT coalesce(array_agg(e.profile ORDER BY e.profile), '{}')::text[]
        FROM media_assets e
        WHERE e.recording_id = r.id
          AND e.kind = 'encoded'
          AND e.state = 'active'
          AND e.profile IS NOT NULL
    ) AS available_encoded_profiles
FROM recordings r
LEFT JOIN media_assets a
    ON a.recording_id = r.id AND a.kind = 'original' AND a.state <> 'deleted'
LEFT JOIN LATERAL (
    SELECT sum(packets) AS packets, sum(drops) AS drops,
           sum(errors) AS errors, sum(scrambled) AS scrambled
    FROM drop_stats
    WHERE media_asset_id = a.id
) d ON true
WHERE r.site = $1 AND r.deleted_at IS NULL
ORDER BY r.program_start_at DESC, r.id DESC;

-- name: ListRecordingDropStats :many
SELECT d.pid, d.packets, d.drops, d.errors, d.scrambled, d.pid_type
FROM drop_stats d
JOIN media_assets a ON a.id = d.media_asset_id
WHERE a.recording_id = $1 AND a.kind = 'original' AND a.state <> 'deleted'
ORDER BY d.pid;

-- name: AppendQualityEvents :exec
UPDATE recordings
SET quality_events = quality_events || sqlc.arg('events')::jsonb,
    updated_at = now()
WHERE id = sqlc.arg('id');

-- ingest が原本 media_asset のコミットと同じ tx で焼く「この録画の望ましい
-- 最終状態」（M3-14、issue #103）。凍結する理由・瞬間・冪等性の詳細は
-- internal/worker/ingest.go の resolveAndSnapshotEncodePolicy の doc コメント参照。
-- 予約行が無い録画（手動で mirakc に起こされた録画等）は呼び出し側がこのクエリを
-- 呼ばないので、列は CREATE TABLE の既定値（'always' / '{}'）のまま残る。
-- name: SnapshotRecordingEncodePolicy :exec
UPDATE recordings SET
    keep_original   = sqlc.arg('keep_original'),
    encode_profiles = sqlc.arg('encode_profiles')::text[],
    updated_at      = now()
WHERE id = sqlc.arg('id');
