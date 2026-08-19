-- +goose Up

-- 削除 reconcile が使う「実体が無い active media_asset」の観測記録
-- （issue #343、docs/storage/retention.md §7「孤児回収の逆」）。
--
-- orphan_files の鏡像: orphan_files は「ファイルはあるが media_assets に無い」を
-- 追い、この表は「media_assets に active としてあるがファイルが無い」を追う。
-- 行の存在そのものが「直前のパスでファイルを観測できなかった」という主張
-- （CLAUDE.md 不変条件 10）で、「観測できたが空」を表す行は作らない。
--
-- 「ファイルが無い」は削除の必要条件であって十分条件ではない
-- （マウントが落ちている・空マウントのときは全 active 行がこう見える）ため、
-- この表を根拠に media_assets を自動で消す経路は無い。first_seen をエイジング
-- の起点にする点も orphan_files と同じ --- DB リストアで行ごと失われれば
-- 窓は自動的に開き直る。
--
-- media_asset_id を PK にし media_assets への FK にする（rel_path は複製しない
-- --- 不変条件 9。読み出し側は常に media_assets と JOIN して引く）。対象の
-- media_asset 行が消えたら（deleted 確定、または誤検知で active に戻った後の
-- 掃除）この行も意味を失うので ON DELETE CASCADE で追従させる。
CREATE TABLE missing_media_assets (
    media_asset_id bigint PRIMARY KEY REFERENCES media_assets (id) ON DELETE CASCADE,
    first_seen     timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE missing_media_assets;
