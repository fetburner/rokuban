-- +goose Up
-- メディア削除（旧世代の判定）の view。Postgres の old_generation_media_asset_copies に従う。
-- 他の削除述語（ごみ箱・until_encoded・外した版・配置）は、置き場や方針の引数を取るので、読み手のクエリに
-- 断片として置く（internal/store/mysqlstore/mediadelete.go）。

-- 論理資産の現行世代より古い派生物のコピーのうち、実体の有る現行の active コピーが（どの置き場にでも）あるもの。
-- generation_started_at は現行世代の最初のコピー行の created_at で、呼び出し側が overlap の猶予を判定する。
CREATE VIEW old_generation_media_asset_copies AS
SELECT old_copy.id, old_copy.media_asset_id, old_copy.store, old_copy.rel_path, old_copy.state,
       old_copy.created_at, old_copy.deleted_at, old_copy.generation,
       (SELECT MIN(current_copy.created_at)
        FROM media_asset_copies current_copy
        WHERE current_copy.media_asset_id = old_copy.media_asset_id
          AND current_copy.generation = asset.derived_generation) AS generation_started_at
FROM media_asset_copies old_copy
JOIN media_assets asset ON asset.id = old_copy.media_asset_id
WHERE asset.kind <> 'original'
  AND old_copy.generation < asset.derived_generation
  AND EXISTS (
      SELECT 1 FROM active_present_media_asset_copies current_copy
      WHERE current_copy.media_asset_id = old_copy.media_asset_id
  );

-- +goose Down
DROP VIEW old_generation_media_asset_copies;
