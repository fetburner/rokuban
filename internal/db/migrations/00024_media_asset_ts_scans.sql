-- +goose Up

CREATE TABLE media_asset_ts_scans (
    media_asset_id     bigint PRIMARY KEY REFERENCES media_assets(id) ON DELETE CASCADE,
    scanned_size_bytes bigint NOT NULL CHECK (scanned_size_bytes >= 0)
);

-- Existing drop_stats are evidence that ingest already scanned the original at
-- the size currently recorded by media_assets. Avoid rereading those files after
-- deployment; originals without statistics remain candidates for ts_scan.
INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes)
SELECT DISTINCT a.id, a.size_bytes
FROM media_assets a
JOIN drop_stats d ON d.media_asset_id = a.id
WHERE a.kind = 'original';

-- +goose Down

DROP TABLE media_asset_ts_scans;
