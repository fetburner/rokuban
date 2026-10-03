-- +goose Up

-- サムネイル JPEG が原本時間軸のどの位置から切り出されたかという事実。
-- サムネイル worker が書き、media_assets 自体の状態機械とは寿命が同じなので
-- media_asset_id を主キーにした衛星表に置く（不変条件 9 / 12 / 13）。
-- 行の不在はこの表ができる前のサムネイルで位置が不明なことを表す。
CREATE TABLE media_asset_thumbnail_seeks (
    media_asset_id bigint PRIMARY KEY REFERENCES media_assets (id) ON DELETE CASCADE,
    seek_ms        bigint NOT NULL CHECK (seek_ms >= 0)
);

-- +goose Down

DROP TABLE media_asset_thumbnail_seeks;
