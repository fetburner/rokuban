-- +goose Up

ALTER TABLE media_asset_copies
    ADD COLUMN generation bigint;

UPDATE media_asset_copies copy
SET generation = CASE
    WHEN asset.kind = 'original' THEN COALESCE(asset.ingest_generation, 0)
    ELSE COALESCE(asset.derived_generation, 0)
END
FROM media_assets asset
WHERE asset.id = copy.media_asset_id;

ALTER TABLE media_asset_copies
    ALTER COLUMN generation SET NOT NULL,
    ADD CONSTRAINT media_asset_copies_generation_check CHECK (generation >= 0);

-- 旧 binary は insert / path 更新 / pending から active への遷移で generation を省く。
-- rolling update 中も active copy を読めるよう、論理行から補う。
-- +goose StatementBegin
CREATE FUNCTION set_media_asset_copy_generation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF (TG_OP = 'INSERT' AND NEW.generation IS NULL)
       OR (TG_OP = 'UPDATE'
           AND (NEW.rel_path IS DISTINCT FROM OLD.rel_path
                OR (NEW.state = 'active' AND OLD.state = 'pending'))
           AND NEW.generation IS NOT DISTINCT FROM OLD.generation) THEN
        SELECT CASE WHEN asset.kind = 'original' THEN COALESCE(asset.ingest_generation, 0)
                    ELSE COALESCE(asset.derived_generation, 0) END
        INTO NEW.generation
        FROM media_assets asset
        WHERE asset.id = NEW.media_asset_id;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER media_asset_copies_set_generation
BEFORE INSERT OR UPDATE OF rel_path, state ON media_asset_copies
FOR EACH ROW EXECUTE FUNCTION set_media_asset_copy_generation();

CREATE INDEX media_asset_copies_generation_idx
    ON media_asset_copies (media_asset_id, generation);

CREATE OR REPLACE FUNCTION active_media_asset_copies(p_asset_id bigint, p_store text)
RETURNS SETOF media_asset_copies
LANGUAGE sql STABLE
AS $$
SELECT copy.*
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.media_asset_id = p_asset_id
  AND copy.state = 'active'
  AND copy.generation = CASE WHEN asset.kind = 'original'
                             THEN COALESCE(asset.ingest_generation, 0)
                             ELSE COALESCE(asset.derived_generation, 0) END
  AND (p_store IS NULL OR copy.store = p_store)
$$;

-- +goose Down

DROP TRIGGER media_asset_copies_set_generation ON media_asset_copies;
DROP FUNCTION set_media_asset_copy_generation();

CREATE OR REPLACE FUNCTION active_media_asset_copies(p_asset_id bigint, p_store text)
RETURNS SETOF media_asset_copies
LANGUAGE sql STABLE
AS $$
SELECT copy.*
FROM media_asset_copies copy
WHERE copy.media_asset_id = p_asset_id
  AND copy.state = 'active'
  AND (p_store IS NULL OR copy.store = p_store)
$$;

DROP INDEX media_asset_copies_generation_idx;
ALTER TABLE media_asset_copies
    DROP CONSTRAINT media_asset_copies_generation_check,
    DROP COLUMN generation;
