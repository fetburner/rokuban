-- In-place media registration shared by `rokuban rescue` and M3-10 imports.
-- Bytes are already under storage.media_dir; these queries only publish DB rows.

-- name: GetInPlaceAssetByRelPath :one
SELECT id, recording_id, kind, profile,
       (state <> 'deleted')::boolean AS published
FROM media_assets
WHERE rel_path = $1
-- media_assets_rel_path_idx は state <> 'deleted' の部分索引なので、実ファイルが残る限り
-- deleted 行もそのファイルの所属を示すという理由でここは意図的に述語を外し、索引を使えず
-- seq scan になる。述語を戻すと deleted 行を見落とし、mtime が変わった rescue 再実行で
-- 同じファイルに live な recordings が 2 行できる（issue #662。
-- TestRescueLatest_ReusesRecordingWhenDeletedAssetMtimeChanges が検出する）。実測 (media_assets 3000 行、EXPLAIN (ANALYZE, TIMING OFF)): 旧クエリ
-- (述語あり) Index Scan 0.010 ms → 新クエリ (述語なし) Seq Scan 0.123 ms。Register は
-- asset 1 件ごとに呼ぶので rescue 全体ではアセット数に比例するが、家庭用サーバー規模を
-- 前提に許容する。増えたら非部分索引の追加が上げ幅。
ORDER BY (state <> 'deleted')::boolean DESC, id DESC
LIMIT 1;

-- name: UpsertInPlaceRecording :one
INSERT INTO recordings (
    source, site,
    network_id, service_id, event_id,
    service_name, channel_type, channel,
    title, program_start_at, program_duration_ms,
    status, started_at, ended_at
) VALUES (
    sqlc.arg('source'), sqlc.arg('site'),
    sqlc.arg('network_id'), sqlc.arg('service_id'), sqlc.arg('event_id'),
    sqlc.arg('service_name'), sqlc.arg('channel_type'), sqlc.arg('channel'),
    sqlc.arg('title'), sqlc.arg('program_start_at'), sqlc.arg('program_duration_ms'),
    sqlc.arg('status'), sqlc.narg('started_at'), sqlc.narg('ended_at')
)
-- ON CONFLICT の対象列と述語は recordings_unique_active_event（issue #129 症状 2 で
-- `AND superseded_at IS NULL` を追加済み）と一字一句一致させる
-- 必要がある。in-place 登録が superseded_at を立てることはない（それは watcher の
-- 録画 supersede 専用の概念）が、索引の述語が変わった以上、対象インデックスの
-- 照合のためにここも揃える。program_start_at は放送イベントの永続 identity
-- の一部なので、DO UPDATE で書き換えず、開始時刻が変われば別行を作る。
ON CONFLICT (site, network_id, service_id, event_id, program_start_at)
    WHERE deleted_at IS NULL AND superseded_at IS NULL
DO UPDATE SET
    source              = EXCLUDED.source,
    service_name        = EXCLUDED.service_name,
    channel_type        = EXCLUDED.channel_type,
    channel             = EXCLUDED.channel,
    title               = EXCLUDED.title,
    program_duration_ms = EXCLUDED.program_duration_ms,
    status              = EXCLUDED.status,
    started_at          = COALESCE(recordings.started_at, EXCLUDED.started_at),
    ended_at            = COALESCE(recordings.ended_at, EXCLUDED.ended_at),
    updated_at          = now()
RETURNING id;

-- name: UpsertInPlaceMediaAsset :one
INSERT INTO media_assets (
    recording_id, kind, profile, rel_path, size_bytes, state
) VALUES (
    sqlc.arg('recording_id'), sqlc.arg('kind'), sqlc.narg('profile'),
    sqlc.arg('rel_path'), sqlc.arg('size_bytes'), 'active'
)
ON CONFLICT (recording_id, kind, profile)
DO UPDATE SET
    size_bytes = EXCLUDED.size_bytes,
    state      = 'active',
    deleted_at = NULL,
    updated_at = now()
-- A tuple is immutable once published. A different path here means a synthetic identity
-- collision or conflicting import; fail (RETURNING no row) instead of orphaning the old file.
WHERE media_assets.rel_path = EXCLUDED.rel_path
RETURNING id;
