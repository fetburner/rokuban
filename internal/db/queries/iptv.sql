-- IPTV M3U / XMLTV エクスポート (internal/api/iptv.go)。

-- name: ListIPTVServices :many
SELECT site, network_id, service_id, name, channel, channel_type
FROM epg_services
WHERE site = ANY(@sites::text[])
ORDER BY site, channel_type, remote_control_key_id, network_id, service_id;

-- name: ListIPTVRecordings :many
-- profile が NULL なら原本、指定があればその encoded profile の active アセットを持つ録画。
SELECT r.id, r.site, r.title
FROM recordings r
JOIN media_assets a ON a.recording_id = r.id
WHERE r.site = ANY(@sites::text[])
  AND r.status = 'finished'
  AND r.deleted_at IS NULL
  AND r.superseded_at IS NULL
  AND a.state = 'active'
  AND (
    (sqlc.narg(profile)::text IS NULL AND a.kind = 'original' AND a.profile IS NULL)
    OR (sqlc.narg(profile)::text IS NOT NULL AND a.kind = 'encoded' AND a.profile = sqlc.narg(profile)::text)
  )
ORDER BY r.program_start_at DESC, r.id DESC;

-- name: ListIPTVGuide :many
SELECT s.site, s.network_id, s.service_id, s.name AS service_name, s.channel_type, s.channel,
       p.program_id, p.start_at, p.end_at, p.name AS program_name, p.description
FROM epg_services s
LEFT JOIN epg_programs p
  ON p.site = s.site
 AND p.network_id = s.network_id
 AND p.service_id = s.service_id
 AND p.start_at < @window_end::timestamptz
 AND (p.end_at > @window_start::timestamptz OR p.start_at >= @window_start::timestamptz)
WHERE s.site = ANY(@sites::text[])
ORDER BY s.site, s.network_id, s.service_id, p.start_at, p.program_id;
