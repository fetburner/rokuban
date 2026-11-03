-- +goose Up

-- Prefer the desired derived store only when this streamer can read it; otherwise
-- select the first active copy from the stores mounted or configured here.
CREATE FUNCTION preferred_streamer_media_asset_copy(
    p_asset_id bigint,
    p_preferred_store text,
    p_readable_stores text[]
)
RETURNS SETOF media_asset_copies
LANGUAGE sql STABLE
AS $$
SELECT c.*
FROM active_media_asset_copies(p_asset_id, NULL::text) c
WHERE c.store = ANY(p_readable_stores)
ORDER BY (c.store = p_preferred_store) DESC, c.store
LIMIT 1
$$;

COMMENT ON FUNCTION preferred_streamer_media_asset_copy(bigint, text, text[]) IS
    'preferred_streamer_media_asset_copy は、streamer が読める active コピーから storage.derived を優先し、無ければ置き場名順で 1 件選ぶ。読めないコピーを選んで配信を失敗させない。';

-- +goose Down
DROP FUNCTION preferred_streamer_media_asset_copy(bigint, text, text[]);
