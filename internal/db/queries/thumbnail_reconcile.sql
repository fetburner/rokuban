-- thumbnail の desired（active original）− observed（active thumbnail）を
-- 埋める候補。全件パスと recording_id を絞った即時パスが同じ述語を使う。
--
-- missing_media_assets に載っている原本は delete_reconcile が実体無しを確認した
-- ものなので、ファイルが戻るまで定期パスからは除外する。マーカーが stale に
-- なった場合は delete_reconcile が消し、次のパスで再び候補になる。
-- name: ListMissingThumbnailRecordings :many
SELECT o.recording_id
FROM media_assets o
JOIN active_media_asset_copies(o.id, sqlc.arg('original_store')::text) original_copy ON true
JOIN recordings r ON r.id = o.recording_id
WHERE o.recording_id > sqlc.arg('after_recording_id')::bigint
  AND (sqlc.narg('recording_id')::bigint IS NULL OR o.recording_id = sqlc.narg('recording_id')::bigint)
  AND o.kind = 'original'
  AND r.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM media_assets t
    WHERE t.recording_id = o.recording_id
      AND t.kind = 'thumbnail'
      AND EXISTS (
          SELECT 1 FROM active_media_asset_copies(t.id, sqlc.arg('derived_store')::text)
      )
  )
  AND NOT EXISTS (
    SELECT 1 FROM missing_media_assets m
    WHERE m.media_asset_id = o.id AND m.store = sqlc.arg('original_store')
  )
ORDER BY o.recording_id
LIMIT sqlc.arg('row_limit');

-- thumbnail が既にあり、CM 検出またはユーザー所有のチャプターがある録画のうち、
-- CM 判定により位置を選び直す候補窓。SQL は候補を窓に収めるだけで、区間内外の
-- 判定は chapters.Derive を通す Go の thumbnailNeedsReselect に任せる。
-- ThumbnailWorker も after_recording_id = id-1, row_limit = 1 で同じクエリを引き、
-- reconcile が投入しワーカーが skip する往復の入口を 1 本にしている。
-- 入力は active かつ missing_media_assets に無いものだけを返す。cut 版の尺と
-- UnmapMs 用区間は media_asset_cuts.keep_ranges の凍結値から作り、ffprobe は呼ばない。
-- name: ListThumbnailReselectCandidates :many
WITH planning AS (
    SELECT
        r.id AS recording_id,
        (r.deleted_at IS NOT NULL)::boolean AS trashed,
        r.program_duration_ms,
        EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id) AS detected,
        EXISTS (SELECT 1 FROM recording_chapter_ownership o WHERE o.recording_id = r.id) AS owned,
        COALESCE((
            SELECT jsonb_agg(jsonb_build_object('startMs', lower(cr.cm_range), 'endMs', upper(cr.cm_range))
                             ORDER BY lower(cr.cm_range))
            FROM recording_cm_detections d
            CROSS JOIN LATERAL unnest(d.cm_ranges) AS cr(cm_range)
            WHERE d.recording_id = r.id
        ), '[]'::jsonb)::jsonb AS cm_ranges,
        COALESCE((
            SELECT jsonb_agg(jsonb_build_object('startMs', lower(s.span), 'endMs', upper(s.span),
                                                'label', s.label, 'cut', s.cut)
                             ORDER BY lower(s.span))
            FROM recording_chapter_spans s
            WHERE s.recording_id = r.id
        ), '[]'::jsonb)::jsonb AS user_spans,
        COALESCE(original.id, 0)::bigint AS original_media_asset_id,
        COALESCE(original.store, '')::text AS original_store,
        COALESCE(original.rel_path, '')::text AS original_rel_path,
        COALESCE(encoded.assets, '[]'::jsonb)::jsonb AS encoded_assets,
        thumbnail.id AS thumbnail_media_asset_id,
        thumbnail.rel_path AS thumbnail_rel_path,
        COALESCE(thumbnail.derived_generation, 0)::bigint AS thumbnail_generation,
        thumbnail.seek_ms
    FROM recordings r
    LEFT JOIN LATERAL (
        SELECT a.id, copy.rel_path, copy.store
        FROM media_assets a
        JOIN active_media_asset_copies(a.id, sqlc.arg('original_store')::text) copy ON true
        WHERE a.recording_id = r.id
          AND a.kind = 'original'
          AND NOT EXISTS (
              SELECT 1 FROM missing_media_assets m
              WHERE m.media_asset_id = a.id AND m.store = sqlc.arg('original_store')
          )
        LIMIT 1
    ) original ON true
    LEFT JOIN LATERAL (
        SELECT jsonb_agg(
            jsonb_build_object(
                'mediaAssetId', a.id,
                'store', copy.store,
                'profile', a.profile,
                'relPath', copy.rel_path,
                'cut', c.media_asset_id IS NOT NULL,
                'keepRanges', COALESCE(frozen.ranges, '[]'::jsonb)
            ) ORDER BY a.profile
        ) AS assets
        FROM media_assets a
        JOIN active_media_asset_copies(a.id, sqlc.arg('derived_store')::text) copy ON true
        LEFT JOIN media_asset_cuts c ON c.media_asset_id = a.id
        LEFT JOIN LATERAL (
            SELECT jsonb_agg(jsonb_build_object('startMs', lower(k), 'endMs', upper(k))
                             ORDER BY lower(k)) AS ranges
            FROM unnest(c.keep_ranges) AS k
        ) frozen ON true
        WHERE a.recording_id = r.id
          AND a.kind = 'encoded'
          AND NOT EXISTS (
              SELECT 1 FROM missing_media_assets m
              WHERE m.media_asset_id = a.id AND m.store = sqlc.arg('derived_store')
          )
    ) encoded ON true
    LEFT JOIN LATERAL (
        SELECT a.id, copy.rel_path, a.derived_generation, thumbnail_seek.seek_ms
        FROM media_assets a
        JOIN active_media_asset_copies(a.id, sqlc.arg('derived_store')::text) copy ON true
        LEFT JOIN media_asset_thumbnail_seeks thumbnail_seek ON thumbnail_seek.media_asset_id = a.id
        WHERE a.recording_id = r.id
          AND a.kind = 'thumbnail'
        LIMIT 1
    ) thumbnail ON true
    WHERE r.purged_at IS NULL
      AND (sqlc.narg('recording_id')::bigint IS NULL OR r.id = sqlc.narg('recording_id')::bigint)
)
SELECT *
FROM planning
WHERE recording_id > sqlc.arg('after_recording_id')::bigint
  AND NOT trashed
  AND thumbnail_media_asset_id IS NOT NULL
  AND (detected OR owned)
ORDER BY recording_id
LIMIT sqlc.arg('row_limit');


-- seek_tiles の desired（active original）− observed（active seek_tiles）を
-- 定期的に埋めるための候補。poster と同じ形（同じ窓・同じ missing_media_assets の
-- 除外）だが、再開位置は呼び出し側が種類ごとに別に持つ。
-- name: ListMissingSeekTilesRecordings :many
SELECT o.recording_id
FROM media_assets o
JOIN active_media_asset_copies(o.id, sqlc.arg('original_store')::text) original_copy ON true
JOIN recordings r ON r.id = o.recording_id
WHERE o.recording_id > sqlc.arg('after_recording_id')::bigint
  AND (sqlc.narg('recording_id')::bigint IS NULL OR o.recording_id = sqlc.narg('recording_id')::bigint)
  AND o.kind = 'original'
  AND r.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM media_assets s
    WHERE s.recording_id = o.recording_id
      AND s.kind = 'seek_tiles'
      AND EXISTS (
          SELECT 1 FROM active_media_asset_copies(s.id, sqlc.arg('derived_store')::text)
      )
  )
  AND NOT EXISTS (
    SELECT 1 FROM missing_media_assets m
    WHERE m.media_asset_id = o.id AND m.store = sqlc.arg('original_store')
  )
ORDER BY o.recording_id
LIMIT sqlc.arg('row_limit');
