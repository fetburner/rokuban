-- +goose Up

-- M7-5: ストレージ観測の射影（issue #238）。
--
-- worker が storage.media_dir / storage.scratch_dir を定期的に statfs 相当で
-- 観測し、結果をここに投影する。tuner_sync / epg_services と同じ**使い捨て
-- プロジェクション**で、真実は常にファイルシステム側にある。毎パス全量を
-- 作り直せる観測値であり、二度と取れない事実ではない（不変条件 9: 導出値と
-- 不可逆な事実を同じ列に載せない --- total/used/available bytes は「今この
-- 瞬間の statfs 結果」であって、積み上げるログではないので全行 upsert で
-- 常に最新観測だけを持つ）。
--
-- 存在理由は不変条件 1（api ロールはファイルシステムにも mirakc にも依存しない）。
-- ファイルシステムを持つのは worker だけなので、observed 側の書き手は worker の
-- 観測ループ 1 人（不変条件 12: 1 表 = 1 書き手 = 1 寿命）。api はこの表だけを
-- 読んで GET /api/storage を組み立てる。
--
-- root は Rokuban 自身が直接 statfs する 2 つのローカルパスに限る --- mirakc の
-- recording.basedir（録画バッファ。docs/storage/contract.md §5 の 2 階層のうち
-- Rokuban からは観測しない側）は対象外。CHECK で config キー名（storage.media_dir /
-- storage.scratch_dir）と 1:1 に保つことで、キーが増えたときに表の想定漏れを
-- コンパイル時ではなく INSERT 時に検出できるようにする。
CREATE TABLE storage_sync (
    -- config キー名（media_dir / scratch_dir）と 1:1。site 列は持たない ---
    -- アーカイブは docs/storage/contract.md §5「rel_path の名前空間」の通り
    -- site 列を持たない単一のストレージであり、tuner_sync のように mirakc
    -- インスタンス単位で分かれるものではない。
    root            text        NOT NULL CHECK (root IN ('media', 'scratch')),
    -- 観測対象の絶対パス（config の値そのもの）。運用者が「どこを見ているか」を
    -- API レスポンスだけで確認できるようにするための添え物で、判定には使わない。
    path            text        NOT NULL,
    total_bytes     bigint      NOT NULL,
    used_bytes      bigint      NOT NULL,
    available_bytes bigint      NOT NULL,
    observed_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (root)
);

-- +goose Down

DROP TABLE IF EXISTS storage_sync;
