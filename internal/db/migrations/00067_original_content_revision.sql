-- +goose Up
-- 内容のリビジョンは局所の値とし、catalog 復元でも古い値を戻さず進める。
ALTER TABLE media_assets ADD COLUMN content_revision bigint NOT NULL DEFAULT 0;
COMMENT ON COLUMN media_assets.content_revision IS
    'ContentRevision は、内容を差し替える書き手が進め、TS 解析の保存と計測記録の照合に使う局所のリビジョンである。catalog 文書には含めない。';
ALTER TABLE media_asset_ts_scans ADD COLUMN content_revision bigint NOT NULL DEFAULT 0;
CREATE OR REPLACE VIEW current_ts_scanned_originals AS
SELECT a.id AS media_asset_id FROM media_assets a
WHERE a.kind = 'original'
  AND a.size_bytes = (SELECT s.scanned_size_bytes FROM media_asset_ts_scans s
      WHERE s.media_asset_id = a.id AND s.content_revision = a.content_revision);
COMMENT ON VIEW current_ts_scanned_originals IS
    'CurrentTsScannedOriginal は、内容のリビジョンと計測サイズが現在の原本に一致する計測記録を持つ原本を表す。内容の書き手は同じ文で計測記録を取り消す。コピーの state は削除済み原本の drop summary を保つため条件に含めず、active 限定が必要な呼び出し側で絞る。';

-- +goose Down
CREATE OR REPLACE VIEW current_ts_scanned_originals AS
SELECT a.id AS media_asset_id FROM media_assets a
WHERE a.kind = 'original'
  AND a.size_bytes = (SELECT s.scanned_size_bytes FROM media_asset_ts_scans s WHERE s.media_asset_id = a.id);
ALTER TABLE media_asset_ts_scans DROP COLUMN content_revision;
ALTER TABLE media_assets DROP COLUMN content_revision;
COMMENT ON VIEW current_ts_scanned_originals IS
    'CurrentTsScannedOriginal は、計測サイズが現在のコミット済みサイズと一致する原本を表す。state は削除済み原本の drop summary を保つため条件に含めず、active 限定が必要な呼び出し側で絞る。';
