-- +goose Up

-- The logical asset keeps its legacy path and state during the rolling migration.
-- Physical locations are copied into a per-store satellite table.
ALTER TABLE media_assets
    ADD COLUMN derived_generation bigint,
    ADD COLUMN sha256 bytea,
    ADD CONSTRAINT media_assets_derived_generation_check
        CHECK (derived_generation IS NULL OR (kind <> 'original' AND derived_generation >= 0)),
    ADD CONSTRAINT media_assets_sha256_length_check
        CHECK (sha256 IS NULL OR octet_length(sha256) = 32);

-- Parse only a terminal generation suffix. A profile may itself contain strings
-- such as ".g2", and malformed or out-of-range suffixes must not abort migration.
CREATE FUNCTION media_asset_generation_from_rel_path(rel_path text)
RETURNS bigint
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
AS $$
    SELECT CASE
        WHEN generation IS NULL THEN NULL
        WHEN length(generation) < 19 THEN generation::bigint
        WHEN length(generation) = 19
         AND generation COLLATE "C" <= '9223372036854775807' COLLATE "C"
            THEN generation::bigint
        ELSE NULL
    END
    FROM (
        SELECT (regexp_match(rel_path, '\.[gu]([1-9][0-9]*)\.[^./]+$'))[1] AS generation
    ) AS parsed
$$;

-- Existing versioned paths seed the monotonic generation once. Writers use this
-- column after this migration; they do not infer the next generation from rel_path.
UPDATE media_assets
SET derived_generation = CASE
    WHEN kind = 'original' THEN NULL
    ELSE COALESCE(media_asset_generation_from_rel_path(rel_path), 0)
END;

CREATE TABLE media_asset_copies (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    media_asset_id bigint      NOT NULL REFERENCES media_assets (id) ON DELETE CASCADE,
    store          text        NOT NULL DEFAULT '',
    rel_path       text        NOT NULL,
    state          text        NOT NULL CHECK (state IN ('pending', 'active', 'deleting', 'deleted')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    deleted_at     timestamptz,
    CONSTRAINT media_asset_copies_store_check
        CHECK (store = '' OR store ~ '^[a-z0-9-]+$'),
    CONSTRAINT media_asset_copies_asset_store_key UNIQUE (media_asset_id, store)
);

CREATE INDEX media_asset_copies_media_asset_id_idx ON media_asset_copies (media_asset_id);
CREATE UNIQUE INDEX media_asset_copies_store_rel_path_idx
    ON media_asset_copies (store, rel_path) WHERE state <> 'deleted';

-- Empty store is an explicit, unadopted value. storage adopt assigns the existing
-- physical rows after writing the root marker.
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, created_at, deleted_at)
SELECT id, '', rel_path, state, created_at, deleted_at
FROM media_assets;

ALTER TABLE recording_ingest_progress
    ADD COLUMN temp_store text NOT NULL DEFAULT '';
ALTER TABLE orphan_files
    ADD COLUMN store text NOT NULL DEFAULT '';
ALTER TABLE missing_media_assets
    ADD COLUMN store text NOT NULL DEFAULT '';
ALTER TABLE storage_sync
    ADD COLUMN store text NOT NULL DEFAULT '';

ALTER TABLE orphan_files DROP CONSTRAINT orphan_files_pkey;
ALTER TABLE orphan_files ADD CONSTRAINT orphan_files_pkey PRIMARY KEY (store, rel_path);

ALTER TABLE missing_media_assets DROP CONSTRAINT missing_media_assets_pkey;
ALTER TABLE missing_media_assets ADD CONSTRAINT missing_media_assets_pkey PRIMARY KEY (media_asset_id, store);

ALTER TABLE storage_sync DROP CONSTRAINT storage_sync_pkey;
ALTER TABLE storage_sync ADD CONSTRAINT storage_sync_pkey PRIMARY KEY (store, root);

-- Old binaries still target these single-column keys in ON CONFLICT while this
-- add-only migration rolls out. Keep them until a later migration retires the
-- legacy writers; the new composite keys serve current workers.
CREATE UNIQUE INDEX orphan_files_rel_path_legacy_key ON orphan_files (rel_path);
CREATE UNIQUE INDEX missing_media_assets_asset_id_legacy_key ON missing_media_assets (media_asset_id);
CREATE UNIQUE INDEX storage_sync_root_legacy_key ON storage_sync (root);

COMMENT ON TABLE media_asset_copies IS
    'Physical copies of logical media assets; an empty store marks rows that storage adopt has not assigned';
COMMENT ON COLUMN media_assets.derived_generation IS
    'Monotonic generation for derived output paths; NULL for original assets';
COMMENT ON COLUMN media_assets.sha256 IS
    'Observed SHA-256 of the logical asset bytes, stored once for all physical copies';
COMMENT ON COLUMN recording_ingest_progress.temp_store IS
    'Store containing the current ingest temporary file';
COMMENT ON COLUMN orphan_files.store IS
    'Store where the orphan path was observed; empty until storage adopt';
COMMENT ON COLUMN missing_media_assets.store IS
    'Store where the asset copy was missing; empty until storage adopt';
COMMENT ON COLUMN storage_sync.store IS
    'Store whose filesystem root was observed; empty until storage adopt';

-- +goose Down

ALTER TABLE storage_sync DROP CONSTRAINT storage_sync_pkey;
DROP INDEX storage_sync_root_legacy_key;
ALTER TABLE storage_sync DROP COLUMN store;
ALTER TABLE storage_sync ADD CONSTRAINT storage_sync_pkey PRIMARY KEY (root);

ALTER TABLE missing_media_assets DROP CONSTRAINT missing_media_assets_pkey;
DROP INDEX missing_media_assets_asset_id_legacy_key;
ALTER TABLE missing_media_assets DROP COLUMN store;
ALTER TABLE missing_media_assets ADD CONSTRAINT missing_media_assets_pkey PRIMARY KEY (media_asset_id);

ALTER TABLE orphan_files DROP CONSTRAINT orphan_files_pkey;
DROP INDEX orphan_files_rel_path_legacy_key;
ALTER TABLE orphan_files DROP COLUMN store;
ALTER TABLE orphan_files ADD CONSTRAINT orphan_files_pkey PRIMARY KEY (rel_path);

ALTER TABLE recording_ingest_progress DROP COLUMN temp_store;

DROP TABLE media_asset_copies;
ALTER TABLE media_assets
    DROP CONSTRAINT media_assets_sha256_length_check,
    DROP CONSTRAINT media_assets_derived_generation_check,
    DROP COLUMN sha256,
    DROP COLUMN derived_generation;
DROP FUNCTION media_asset_generation_from_rel_path(text);
