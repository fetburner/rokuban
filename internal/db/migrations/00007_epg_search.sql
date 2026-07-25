-- +goose Up

-- M2-2: 検索 / ruler 共通の正規化と pg_trgm インデックス
-- （docs/data.md §5 — 全角/半角は immutable 関数 + 式インデックス）

-- 全角英数・全角スペースを半角にし、小文字化する。
-- 「ＮＨＫ」ルールが「NHK」番組にマッチする実問題用（EPGStation halfWidthKeyword 相当）。
-- カタカナの全半角まではこの段階では扱わない。
CREATE FUNCTION normalize_search_text(t text) RETURNS text
IMMUTABLE STRICT LANGUAGE sql AS $$
  SELECT lower(
    translate(
      t,
      'ＡＢＣＤＥＦＧＨＩＪＫＬＭＮＯＰＱＲＳＴＵＶＷＸＹＺａｂｃｄｅｆｇｈｉｊｋｌｍｎｏｐｑｒｓｔｕｖｗｘｙｚ０１２３４５６７８９　',
      'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789 '
    )
  )
$$;

-- キーワード部分一致（LIKE '%…%'）を加速。ruler と検索 API が同じ式を使う。
CREATE INDEX epg_programs_name_trgm
  ON epg_programs USING gin (normalize_search_text(name) gin_trgm_ops);

CREATE INDEX epg_programs_description_trgm
  ON epg_programs USING gin (normalize_search_text(description) gin_trgm_ops);

-- +goose Down

DROP INDEX IF EXISTS epg_programs_description_trgm;
DROP INDEX IF EXISTS epg_programs_name_trgm;
DROP FUNCTION IF EXISTS normalize_search_text(text);
