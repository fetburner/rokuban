-- +goose Up

-- カット版の派生物が実際に適用した区間の凍結。
--
-- 派生値ではなく**二度と再取得できない事実**である（不変条件 9）。チャプターは
-- ユーザーが後から直せるので、現在のタイムラインから再計算すると「その
-- ファイルがどう作られたか」を復元できない。この表があることで、現在の
-- タイムラインと突き合わせて「編集前の内容です」を判定できる。
--
-- 主キーが media_asset_id なのは、行の寿命が派生物の行と同時（不変条件 12）。
-- 録画の行と同時に生まれて同時に死ぬのは media_assets の側である。
--
-- keep_ranges の単位は原本の最初の映像フレームを 0 とする ms の半開区間
-- （recording_chapter_spans と同じ）。境界はフレーム境界へ量子化済みで、
-- 昇順・非交差・非隣接に正規化して入れる（値どうしの一致比較をするため）。
CREATE TABLE media_asset_cuts (
    media_asset_id bigint PRIMARY KEY REFERENCES media_assets (id) ON DELETE CASCADE,
    -- 空の keep_ranges は「全部カット」= カット版は作れない。意味を持たない行を
    -- 作らない（不変条件 10）ので表現不可能にする。
    keep_ranges    int8multirange NOT NULL CHECK (NOT isempty(keep_ranges))
);

-- +goose Down

DROP TABLE media_asset_cuts;
