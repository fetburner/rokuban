-- ListEncodedMeaningEvidence は、プロファイル名が過去に持った「成果物の意味」の
-- 証拠を返す。起動時の検査（internal/worker の CheckEncodeProfileMeaning）が、
-- 今の設定の同名プロファイルと突き合わせる。
--
--   * active な encoded: rel_path。container は拡張子、cut は profile 名の直後の
--     `.g{n}` / `.u{n}` で読む。derived_generation は削除後の再生成でも増えるため、
--     cut の意味判定には使わない。パスの印は rescue（media_asset_cuts を持たない）後も残る。
--   * 作り直し要求の行: rel_path は空文字。api は、押された時点の設定で cut の
--     プロファイルにだけ要求を書く。container は分からない
--
-- 証拠が無い名前は返らない（過去の意味が存在しない）。
-- name: ListEncodedMeaningEvidence :many
SELECT active.profile, active.rel_path
FROM (
    SELECT DISTINCT ON (a.profile) a.profile::text AS profile, c.rel_path::text AS rel_path
    FROM media_assets a
    JOIN media_asset_copies c
      ON c.media_asset_id = a.id AND c.state = 'active'
    WHERE a.kind = 'encoded'
      AND (sqlc.narg('store_name')::text IS NULL OR c.store = sqlc.narg('store_name'))
    ORDER BY a.profile, c.store = sqlc.narg('store_name') DESC, c.store
) active
UNION ALL
SELECT q.profile::text, ''::text
FROM encoded_asset_rebuild_requests q;
