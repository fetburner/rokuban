-- In-place media registration shared by `rokuban rescue` and M3-10 imports.
-- Bytes are already under storage.media_dir; these queries only publish DB rows.

-- name: GetInPlaceAssetByRelPath :one
SELECT a.id, a.recording_id, a.kind, a.profile, c.state, c.generation,
       (r.deleted_at IS NOT NULL OR r.purged_at IS NOT NULL)::boolean AS recording_trashed
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
JOIN recordings r ON r.id = a.recording_id
WHERE c.rel_path = sqlc.arg('rel_path')
  AND c.store = sqlc.arg('store_name')
-- media_asset_copies_store_rel_path_idx は state <> 'deleted' の部分索引なので、実ファイルが残る限り
-- deleted copy もそのファイルの所属を示すという理由でここは意図的に述語を外し、索引を使えず
-- seq scan になる。述語を戻すと deleted 行を見落とし、mtime が変わった rescue 再実行で
-- 同じファイルに live な recordings が 2 行できる（issue #662。
-- TestRescueLatest_ReusesRecordingWhenDeletedAssetMtimeChanges が検出する）。実測 (media_assets 3000 行、EXPLAIN (ANALYZE, TIMING OFF)): 旧クエリ
-- (述語あり) Index Scan 0.010 ms → 新クエリ (述語なし) Seq Scan 0.123 ms。Register は
-- asset 1 件ごとに呼ぶので rescue 全体ではアセット数に比例するが、家庭用サーバー規模を
-- 前提に許容する。増えたら非部分索引の追加が上げ幅。
ORDER BY (c.state <> 'deleted')::boolean DESC, a.id DESC
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

-- in-place 登録の論理行。規則はこの 1 文だけに置く（Go は 0 行を ErrConflict に写すだけ）。
-- 録画がごみ箱か消去済みなら、資産の有無にかかわらず 0 行になる（INSERT ... SELECT の WHERE）。
-- 論理資産が無ければ新規に作る。あれば、次の 3 つを満たすときだけ rel_path / size_bytes を差し替える。
-- 1. この置き場に、その資産の生きたコピー（active / deleting / pending）が無い。
--    あれば同じ置き場に 2 つ目の現行ファイルを足すことになる。deleting は unlink される予定のファイルである。
-- 2. パスから読む世代が現行の世代（derived_generation。original は NULL = 0）と等しい。
--    古い世代で論理行を巻き戻すと、現行のコピーが旧世代として削除される。
-- 3. 録画がごみ箱にない（上記）。
-- derived_generation は変えず、state / deleted_at は書かない（コピー行を書いた後に
-- ReconcileMediaAssetStateFromCopies が決める）。並行する 2 本の登録は
-- media_asset_copies_asset_store_generation_idx が直列化する。
-- name: UpsertInPlaceMediaAsset :one
INSERT INTO media_assets (
    recording_id, kind, profile, rel_path, size_bytes, state, derived_generation
)
SELECT
    r.id, sqlc.arg('kind')::text, sqlc.narg('profile')::text,
    sqlc.arg('rel_path')::text, sqlc.arg('size_bytes')::bigint, 'active',
    CASE WHEN sqlc.arg('kind')::text = 'original' THEN NULL
         ELSE COALESCE(media_asset_generation_from_rel_path(sqlc.arg('rel_path')::text), 0)
    END
FROM recordings r
WHERE r.id = sqlc.arg('recording_id')
  AND r.deleted_at IS NULL
  AND r.purged_at IS NULL
ON CONFLICT (recording_id, kind, profile)
DO UPDATE SET
    rel_path   = EXCLUDED.rel_path,
    size_bytes = EXCLUDED.size_bytes,
    sha256     = NULL,
    updated_at = now()
WHERE NOT EXISTS (
        SELECT 1 FROM media_asset_copies live
        WHERE live.media_asset_id = media_assets.id
          AND live.store = sqlc.arg('store_name')
          AND live.state <> 'deleted'
    )
  AND COALESCE(EXCLUDED.derived_generation, 0) = COALESCE(media_assets.derived_generation, 0)
RETURNING id,
    CASE WHEN kind = 'original' THEN 0
         ELSE COALESCE(media_asset_generation_from_rel_path(sqlc.arg('rel_path')::text), 0)
    END::bigint AS generation;
