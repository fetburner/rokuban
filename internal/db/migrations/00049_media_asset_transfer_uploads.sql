-- +goose Up

-- upload_store と upload_state は、pending コピーの再開できるアップロードの送信状態である。
-- 書き手は転送 worker だけで、lease を持つ試行が slice の終わりに保存する。
-- 進捗の行と寿命が同じなので同じ表に置く。upload_store は送信先の置き場名で、Abort のときにどの実装を使うかを指す。
-- upload_state は置き場の実装が marshal した不透明なバイト列で、この表は中身を解釈しない。
-- 両方 NULL（送信状態が無い）か、両方非 NULL（送信状態がある）のどちらかだけを表現できる。
-- verifying_source は、原本の不一致を照合している最中であることを表す。
-- 真の間、offset_bytes / hash_state は転送元の読み直しの途中経過で、temp には書かない（コピーの途中経過とは意味が違う）。
-- 照合は派生物の送信状態と同じ行に共存しない（原本だけが照合する）。
ALTER TABLE media_asset_copy_transfer_progress
    ADD COLUMN upload_store text,
    ADD COLUMN upload_state bytea,
    ADD COLUMN verifying_source boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT media_asset_copy_transfer_progress_upload_pair
        CHECK ((upload_store IS NULL) = (upload_state IS NULL)),
    ADD CONSTRAINT media_asset_copy_transfer_progress_verifying_exclusive
        CHECK (NOT (verifying_source AND upload_state IS NOT NULL));

-- media_asset_transfer_abandoned_uploads は、もう使わない送信状態の掃除待ちである。
-- 書き手は転送 worker（送信状態を置き換えるとき・進捗の行が消えるとき）で、掃除役は置き場ごとの Abort を担う worker である。
-- 行の寿命は掃除が Abort を済ませて行を消すまでで、コピーや進捗の行より長生きするので FK を張らない。
-- 行の存在そのものが主張である: 行がある = その送信状態はもう使わず、置き場が残骸なしと答えるまで Abort する。
CREATE TABLE media_asset_transfer_abandoned_uploads (
    id           bigserial PRIMARY KEY,
    store        text NOT NULL,
    upload_state bytea NOT NULL,
    abandoned_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX media_asset_transfer_abandoned_uploads_store_idx
    ON media_asset_transfer_abandoned_uploads (store, id);

COMMENT ON TABLE media_asset_transfer_abandoned_uploads IS
    'Upload states no longer used by any transfer; a row exists until the store has aborted that upload.';

-- 進捗の行が消えるとき（コピーが active / deleted になる、回収される、コピーの CASCADE 削除）に、
-- 送信状態を掃除待ちに移す。送信状態が無ければ何も作らない。
-- +goose StatementBegin
CREATE FUNCTION media_asset_transfer_progress_abandon_upload() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.upload_state IS NOT NULL THEN
        INSERT INTO media_asset_transfer_abandoned_uploads (store, upload_state)
        VALUES (OLD.upload_store, OLD.upload_state);
    END IF;
    RETURN OLD;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER media_asset_copy_transfer_progress_abandon_upload
    BEFORE DELETE ON media_asset_copy_transfer_progress
    FOR EACH ROW EXECUTE FUNCTION media_asset_transfer_progress_abandon_upload();

-- +goose Down

DROP TRIGGER media_asset_copy_transfer_progress_abandon_upload ON media_asset_copy_transfer_progress;
DROP FUNCTION media_asset_transfer_progress_abandon_upload();
DROP TABLE media_asset_transfer_abandoned_uploads;
ALTER TABLE media_asset_copy_transfer_progress
    DROP CONSTRAINT media_asset_copy_transfer_progress_verifying_exclusive,
    DROP CONSTRAINT media_asset_copy_transfer_progress_upload_pair,
    DROP COLUMN verifying_source,
    DROP COLUMN upload_state,
    DROP COLUMN upload_store;
