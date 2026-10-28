-- +goose Up

-- A logical asset without a copy cannot be adopted after its legacy path and
-- lifecycle columns are gone. Require the compatibility release's storage adopt
-- to finish before this migration commits.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM media_assets a
        WHERE NOT EXISTS (
            SELECT 1 FROM media_asset_copies c WHERE c.media_asset_id = a.id
        )
    ) THEN
        RAISE EXCEPTION
            'media_assets without a copy remain; run storage adopt with the compatibility release before applying this migration';
    END IF;
END
$$;
-- +goose StatementEnd

-- These relations are the compatibility shape used by binaries predating the
-- copy-table readers. Current deletion queries use the store-aware function and
-- only need the logical identity from removed_encoded_assets.
DROP VIEW until_encoded_deletable_originals;
DROP VIEW removed_encoded_assets;

CREATE VIEW removed_encoded_assets AS
SELECT a.id AS asset_id,
       a.recording_id
FROM media_assets a
JOIN encoded_asset_removal_requests q
    ON q.recording_id = a.recording_id AND q.profile = a.profile
JOIN recording_encode_policy p ON p.recording_id = a.recording_id
WHERE a.kind = 'encoded'
  AND NOT (a.profile = ANY (p.encode_profiles))
  AND EXISTS (
      SELECT 1
      FROM media_assets other
      WHERE other.recording_id = a.recording_id
        AND (
            (other.kind = 'original' AND EXISTS (
                SELECT 1 FROM media_asset_copies c
                WHERE c.media_asset_id = other.id AND c.state = 'active'
            ))
            OR (
                other.kind = 'encoded'
                AND NOT EXISTS (
                    SELECT 1 FROM encoded_asset_removal_requests oq
                    WHERE oq.recording_id = other.recording_id
                      AND oq.profile = other.profile
                )
                AND EXISTS (
                    SELECT 1 FROM media_asset_copies c
                    WHERE c.media_asset_id = other.id AND c.state = 'active'
                )
            )
        )
  );

DROP INDEX media_assets_kind_state_idx;
DROP INDEX media_assets_rel_path_idx;

ALTER TABLE media_assets
    DROP COLUMN rel_path,
    DROP COLUMN state,
    DROP COLUMN deleted_at;

-- +goose Down

-- The old schema can represent at most one physical copy per logical asset.
-- Refuse a lossy rollback if the copy table now has multiple locations or states
-- that the legacy lifecycle cannot represent.
DROP VIEW removed_encoded_assets;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM media_assets a
        LEFT JOIN media_asset_copies c ON c.media_asset_id = a.id
        GROUP BY a.id
        HAVING count(c.id) <> 1
    ) THEN
        RAISE EXCEPTION
            'cannot restore media_assets copy columns while an asset has zero or multiple copies';
    END IF;
    IF EXISTS (
        SELECT 1 FROM media_asset_copies WHERE state = 'pending'
    ) THEN
        RAISE EXCEPTION
            'cannot restore media_assets copy columns while a copy is pending';
    END IF;
    IF EXISTS (
        SELECT rel_path
        FROM media_asset_copies
        WHERE state <> 'deleted'
        GROUP BY rel_path
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION
            'cannot restore the legacy global rel_path index while active copies share a path';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE media_assets
    ADD COLUMN rel_path text,
    ADD COLUMN state text NOT NULL DEFAULT 'active',
    ADD COLUMN deleted_at timestamptz;

UPDATE media_assets a
SET rel_path = c.rel_path,
    state = c.state,
    deleted_at = c.deleted_at
FROM media_asset_copies c
WHERE c.media_asset_id = a.id;

ALTER TABLE media_assets
    ALTER COLUMN rel_path SET NOT NULL,
    ADD CONSTRAINT media_assets_state_check
        CHECK (state IN ('active', 'deleting', 'deleted'));

CREATE INDEX media_assets_kind_state_idx ON media_assets (kind, state);
CREATE UNIQUE INDEX media_assets_rel_path_idx
    ON media_assets (rel_path) WHERE state <> 'deleted';

CREATE VIEW until_encoded_deletable_originals AS
SELECT DISTINCT a.id AS asset_id,
       a.recording_id,
       a.rel_path,
       a.size_bytes,
       a.state
FROM (SELECT DISTINCT store FROM media_asset_copies) s
CROSS JOIN LATERAL until_encoded_deletable_originals(s.store) d
JOIN media_assets a ON a.id = d.asset_id;

CREATE VIEW removed_encoded_assets AS
SELECT a.id AS asset_id,
       a.recording_id,
       a.rel_path,
       a.size_bytes,
       a.state
FROM media_assets a
JOIN encoded_asset_removal_requests q
    ON q.recording_id = a.recording_id AND q.profile = a.profile
JOIN recording_encode_policy p ON p.recording_id = a.recording_id
WHERE a.kind = 'encoded'
  AND NOT (a.profile = ANY (p.encode_profiles))
  AND EXISTS (
      SELECT 1
      FROM media_assets other
      WHERE other.recording_id = a.recording_id
        AND (
            (other.kind = 'original' AND EXISTS (
                SELECT 1 FROM media_asset_copies c
                WHERE c.media_asset_id = other.id AND c.state = 'active'
            ))
            OR (
                other.kind = 'encoded'
                AND NOT EXISTS (
                    SELECT 1 FROM encoded_asset_removal_requests oq
                    WHERE oq.recording_id = other.recording_id
                      AND oq.profile = other.profile
                )
                AND EXISTS (
                    SELECT 1 FROM media_asset_copies c
                    WHERE c.media_asset_id = other.id AND c.state = 'active'
                )
            )
        )
  );
