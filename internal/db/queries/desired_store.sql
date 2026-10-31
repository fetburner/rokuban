-- ingest が書き込み先の置き場を選ぶために、規則を年齢 0 で評価する（program_end_at = now()）。
-- older_than の規則は一致せず、site の規則と無条件の規則だけが残る。
-- 規則の評価は desired_media_store 1 か所に置き、Go に写しを作らない。
-- name: GetDesiredOriginalStoreAtIngest :one
-- 一致する規則が無ければ空文字列を返す。
SELECT COALESCE(desired_media_store('original', sqlc.arg('original_rules')::jsonb, sqlc.arg('derived_store')::text,
                                    sqlc.arg('recording_site')::text, now()), '')::text AS store;
