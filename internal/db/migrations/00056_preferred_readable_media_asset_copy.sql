-- +goose Up

-- Serving readers select their copy here so that the streamer query and the contract
-- tests share one definition. p_readable_stores NULL means every store is readable.
-- p_preferred_store NULL means no preference, so the store name order decides.
CREATE FUNCTION preferred_readable_media_asset_copy(
    p_asset_id bigint,
    p_preferred_store text,
    p_readable_stores text[]
)
RETURNS SETOF media_asset_copies
LANGUAGE sql STABLE
AS $$
SELECT c.*
FROM active_media_asset_copies(p_asset_id, NULL::text) c
WHERE p_readable_stores IS NULL OR c.store = ANY(p_readable_stores)
ORDER BY (c.store = p_preferred_store) DESC, c.store
LIMIT 1
$$;

COMMENT ON FUNCTION preferred_readable_media_asset_copy(bigint, text, text[]) IS
    'preferred_readable_media_asset_copy は、読める置き場の active コピーから p_preferred_store を優先し、無ければ置き場名順で 1 件選ぶ。p_readable_stores が NULL なら全置き場が読める扱い。読めないコピーを選ばない。';

-- +goose Down
DROP FUNCTION preferred_readable_media_asset_copy(bigint, text, text[]);
