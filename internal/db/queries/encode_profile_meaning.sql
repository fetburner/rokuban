-- ListEncodedMeaningEvidence は、プロファイル名が過去に持った「成果物の意味」の
-- 証拠を返す。起動時の検査（internal/worker の CheckEncodeProfileMeaning）が、
-- 今の設定の同名プロファイルと突き合わせる。
--
--   * active な encoded: rel_path と derived_generation。container は rel_path、
--     cut は derived_generation が 0 より大きいかで読む。generation は catalog に
--     含めるため、rescue（media_asset_cuts を持たない）後も意味が残る。
--   * 作り直し要求の行: rel_path は空文字。api は、押された時点の設定で cut の
--     プロファイルにだけ要求を書く。container は分からない
--
-- 証拠が無い名前は返らない（過去の意味が存在しない）。
-- name: ListEncodedMeaningEvidence :many
SELECT a.profile::text AS profile, a.rel_path::text AS rel_path,
       COALESCE(a.derived_generation, 0)::bigint AS derived_generation
FROM media_assets a
WHERE a.kind = 'encoded'
  AND a.state = 'active'
UNION ALL
SELECT q.profile::text, ''::text, 0::bigint
FROM encoded_asset_rebuild_requests q;
