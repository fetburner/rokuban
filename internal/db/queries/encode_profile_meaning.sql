-- ListActiveEncodedMeanings は、active な encoded が持つ「成果物の意味」を
-- (profile, cut, container) の重複なしで返す。起動時の検査（internal/worker の
-- CheckEncodeProfileMeaning）が、今の設定の同名プロファイルと突き合わせる。
--
--   * cut = media_asset_cuts の行の有無（カット版だけが凍結区間を持つ）
--   * container = rel_path の拡張子（EncodedRelPath が必ず `.{container}` で終える）
--
-- active な encoded が無い名前は返らない（過去の意味が存在しない）。
-- name: ListActiveEncodedMeanings :many
SELECT DISTINCT
  a.profile::text AS profile,
  (c.media_asset_id IS NOT NULL)::boolean AS cut,
  COALESCE(substring(a.rel_path FROM '\.([^./]+)$'), '')::text AS container
FROM media_assets a
LEFT JOIN media_asset_cuts c ON c.media_asset_id = a.id
WHERE a.kind = 'encoded'
  AND a.state = 'active'
ORDER BY 1, 2, 3;
