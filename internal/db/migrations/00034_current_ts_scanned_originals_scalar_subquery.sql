-- +goose Up

-- 計測済み判定を、主キー media_asset_id で引くスカラー副問い合わせにする。
-- 集合は変わらない。media_asset_ts_scans は media_asset_id が主キーなので副問い合わせは
-- 高々 1 行を返し、scan が無い原本は NULL との比較で不一致になる。
-- JOIN + 異なる表の列どうしの等号だと、計測済みの集合が入れ子ループの内側で
-- Materialize され、候補 × 計測済みで二乗になることがある。
CREATE OR REPLACE VIEW current_ts_scanned_originals AS
SELECT a.id AS media_asset_id
FROM media_assets a
WHERE a.kind = 'original'
  AND a.size_bytes = (
      SELECT s.scanned_size_bytes
      FROM media_asset_ts_scans s
      WHERE s.media_asset_id = a.id
  );

-- sqlc は CREATE OR REPLACE VIEW で view を作り直すので、doc コメントを付け直す。
COMMENT ON VIEW current_ts_scanned_originals IS
    'CurrentTsScannedOriginal は、計測サイズが現在のコミット済みサイズと一致する原本を表す。state は削除済み原本の drop summary を保つため条件に含めず、active 限定が必要な呼び出し側で絞る。';

-- +goose Down

CREATE OR REPLACE VIEW current_ts_scanned_originals AS
SELECT a.id AS media_asset_id
FROM media_assets a
JOIN media_asset_ts_scans s ON s.media_asset_id = a.id
WHERE a.kind = 'original'
  AND s.scanned_size_bytes = a.size_bytes;
