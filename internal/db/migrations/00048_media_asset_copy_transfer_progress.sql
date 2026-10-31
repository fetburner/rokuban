-- +goose Up

-- media_asset_copy_transfer_progress は、pending の転送コピー 1 本の、原本転送の slice 間の進捗を持つ。
-- 書き手は転送 worker だけで、コピーが pending でなくなったら行を消す（不変条件 12: 寿命はコピーと同じ pending の期間）。
--
-- lease は、今の slice の持ち主と期限である。期限は DB の now() と比べる。
-- handed_off は、前の slice が offset までの temp を書き終えて保存したことを表す。
-- 保存と同時に lease を切るので、次の slice が保存済みの状態を引き継げる。
-- generation は temp 名に入る書き手の世代で、きれいな引き継ぎ以外では上がる。
CREATE TABLE media_asset_copy_transfer_progress (
    copy_id          bigint PRIMARY KEY REFERENCES media_asset_copies (id) ON DELETE CASCADE,
    lease_owner      uuid NOT NULL,
    lease_expires_at timestamptz NOT NULL,
    generation       bigint NOT NULL CHECK (generation > 0),
    handed_off       boolean NOT NULL DEFAULT false,
    offset_bytes     bigint NOT NULL DEFAULT 0 CHECK (offset_bytes >= 0),
    hash_state       bytea
);

-- +goose Down

DROP TABLE media_asset_copy_transfer_progress;
