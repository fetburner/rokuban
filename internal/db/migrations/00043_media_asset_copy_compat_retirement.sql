-- +goose Up

-- rel_path / state / deleted_at を落とすと、コピーの無い論理資産には物理パスを復元する元が
-- 残らない。直前の版の storage adopt でコピーを作り終えるまで、この migration は進めない。
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
            'media_assets without a copy remain; they cannot be recovered once media_assets.rel_path is dropped';
    END IF;
END
$$;
-- +goose StatementEnd

-- 現行の読み手は置き場つきの関数を使い、removed_encoded_assets からは論理 ID だけを読む。
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
                SELECT 1 FROM active_media_asset_copies(other.id, NULL)
            ))
            OR (
                other.kind = 'encoded'
                AND NOT EXISTS (
                    SELECT 1 FROM encoded_asset_removal_requests oq
                    WHERE oq.recording_id = other.recording_id
                      AND oq.profile = other.profile
                )
                AND EXISTS (
                    SELECT 1 FROM active_media_asset_copies(other.id, NULL)
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

-- 旧列は論理資産ごとに 1 つのパスと状態しか持てない。現行世代のコピーのうち 1 つを代表に選んで写す。
-- 代表は active、deleting、deleted の順に、同順位なら置き場名と id の小さい方。state は現行世代の
-- コピー全体から決め、active があれば active、無くて deleting があれば deleting、それ以外は deleted。
-- 旧世代のコピーは旧列に写せないので、ファイルだけが残る。
-- 現行世代のコピーが無い論理資産と、pending のコピーは旧列で表せないので、復元を拒否する。
DROP VIEW removed_encoded_assets;

CREATE TEMP TABLE media_asset_legacy_columns AS
SELECT a.id AS media_asset_id,
       rep.rel_path,
       CASE
           WHEN EXISTS (SELECT 1 FROM active_media_asset_copies(a.id, NULL)) THEN 'active'
           WHEN EXISTS (
               SELECT 1 FROM media_asset_copies c
               WHERE c.media_asset_id = a.id
                 AND c.generation = COALESCE(a.derived_generation, 0)
                 AND c.state = 'deleting'
           ) THEN 'deleting'
           ELSE 'deleted'
       END AS state,
       rep.deleted_at
FROM media_assets a
JOIN LATERAL (
    SELECT c.rel_path, c.deleted_at
    FROM media_asset_copies c
    WHERE c.media_asset_id = a.id
      AND c.generation = COALESCE(a.derived_generation, 0)
    ORDER BY (c.state = 'active') DESC, (c.state = 'deleting') DESC, c.store, c.id
    LIMIT 1
) rep ON true;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM media_assets a
        WHERE NOT EXISTS (SELECT 1 FROM media_asset_legacy_columns l WHERE l.media_asset_id = a.id)
    ) THEN
        RAISE EXCEPTION
            'cannot restore media_assets copy columns while an asset has no copy of its current generation';
    END IF;
    IF EXISTS (
        SELECT 1 FROM media_asset_copies WHERE state = 'pending'
    ) THEN
        RAISE EXCEPTION
            'cannot restore media_assets copy columns while a copy is pending';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM media_asset_legacy_columns
        WHERE state <> 'deleted'
        GROUP BY rel_path
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION
            'cannot restore the global rel_path index while live assets share a path';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE media_assets
    ADD COLUMN rel_path text,
    ADD COLUMN state text NOT NULL DEFAULT 'active',
    ADD COLUMN deleted_at timestamptz;

UPDATE media_assets a
SET rel_path = l.rel_path,
    state = l.state,
    deleted_at = CASE WHEN l.state = 'deleted' THEN l.deleted_at END
FROM media_asset_legacy_columns l
WHERE l.media_asset_id = a.id;

DROP TABLE media_asset_legacy_columns;

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
                SELECT 1 FROM active_media_asset_copies(other.id, NULL)
            ))
            OR (
                other.kind = 'encoded'
                AND NOT EXISTS (
                    SELECT 1 FROM encoded_asset_removal_requests oq
                    WHERE oq.recording_id = other.recording_id
                      AND oq.profile = other.profile
                )
                AND EXISTS (
                    SELECT 1 FROM active_media_asset_copies(other.id, NULL)
                )
            )
        )
  );
