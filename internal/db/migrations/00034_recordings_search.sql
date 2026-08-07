-- +goose Up

-- M3-24: 録画一覧の絞り込み + キーセットページング（issue #136）。
--
-- normalize_search_text（全角/半角 + 大小文字の揺れ吸収）自体は 00007_epg_search.sql
-- で定義済み。ここでは同じ関数を recordings.title / recordings.description にも
-- 式 GIN で張るだけ（00007 と同形。docs/data.md §5「全角/半角の揺れは immutable
-- 関数 + 式インデックスで吸収する」）。
--
-- 録画検索は rulequery / /search（EPG 検索）とエンジンを共有しない（recordings は
-- 放送の事実を自前の列に凍結した永続資産で、列の形も問いの軸も epg_programs と
-- 違う。docs/data.md §5「録画検索は rulequery を共有しない」）。共有するのは
-- normalize_search_text という正規化方言だけ。

CREATE INDEX recordings_title_trgm
  ON recordings USING gin (normalize_search_text(title) gin_trgm_ops);

CREATE INDEX recordings_description_trgm
  ON recordings USING gin (normalize_search_text(description) gin_trgm_ops);

-- genres（jsonb）から genre_lv1（ジャンル大分類の重なり判定用 smallint[]）を導出する。
--
-- 生成列にして writer に一切書かせない。recordings に行を作る経路は
-- internal/watcher/watcher.go に 2 箇所（CreateRecording / CreateFailedRecording）、
-- internal/catalog/rescue.go、将来の rokuban import epgstation（#72）と複数あり、
-- 1 経路でも genre_lv1 を書き忘れるとその録画が黙って検索に出なくなる。genres から
-- 一意に決まる値なので、書き手を DB 自身の 1 人に固定する（不変条件 9: 導出値と
-- 不可逆な事実を混同しないための「導出できるなら列にしない」の裏側 --- ここでは
-- genres という不可逆な事実の別表現であり、毎パス作り直される値ではないので列に
-- してよいが、書き手は複数経路のアプリコードではなく DB でなければならない）。
--
-- jsonb_array_elements は集合返しなので生成式に直接書けず、IMMUTABLE な SQL 関数に
-- 包む。genres が配列でない/lv1 が数値でない行（mirakc の値をそのまま入れる
-- marshalJSONOrNull 経由なので型を仮定できない）でも INSERT を落とさないよう、
-- jsonb_typeof と正規表現で守る --- ここで落ちると録画の記録そのものが失敗する
-- （不可逆な事実の喪失）。
--
-- 罠 1（PR #187 レビュー M1）: `jsonb_typeof(g) = 'array'` を
-- `jsonb_array_elements(g)` と同じ WHERE に並べるのはプラン依存 --- Postgres が
-- 述語の評価順序を保証しないため、非配列 g でも jsonb_array_elements が先に
-- 評価されてエラーになりうる（実測でこの形は落ちなかったが、表現として閉じて
-- いない）。CASE で FROM 句自体を空配列に閉じ込め、jsonb_array_elements には
-- 常に配列しか渡らないようにする。
--
-- 罠 2（PR #187 レビュー M1）: `(e->>'lv1') ~ '^[0-9]+$'` は桁数を見ないため
-- `40000` のような smallint 範囲外の整数も正規表現を通過し、直後の
-- `::smallint` キャストで INSERT ごと失敗する（= 録画行が作られない不可逆な
-- 事実の喪失）。genre_lv1 のドメインは rule_genres.genre_lv1（00006_rules.sql、
-- `CHECK (genre_lv1 BETWEEN 0 AND 15)`）で 0..15 と決まっているので、
-- smallint へキャストする前に `::numeric` で範囲を確認して閉じる
-- （numeric はオーバーフローしないので、桁数がいくつでも安全に比較できる）。
-- 範囲外の lv1 はエラーにせず黙って無視する --- ここでの目的は「検索用に使える
-- ジャンル大分類だけを集める」ことで、genres 自体（不可逆な事実）は別に無傷で
-- 保持されるため、真実を捨てるわけではない。
CREATE FUNCTION genre_lv1_of(g jsonb) RETURNS smallint[]
IMMUTABLE LANGUAGE sql AS $$
  SELECT coalesce(
    (SELECT array_agg(DISTINCT (e->>'lv1')::smallint ORDER BY (e->>'lv1')::smallint)
     FROM jsonb_array_elements(
       CASE WHEN jsonb_typeof(g) = 'array' THEN g ELSE '[]'::jsonb END
     ) e
     WHERE (e->>'lv1') ~ '^[0-9]+$'
       AND (e->>'lv1')::numeric BETWEEN 0 AND 15),
    '{}')::smallint[]
$$;

-- STORED なので既存行は ALTER TABLE の書き換え時に自動計算される。関数を後から
-- 変えても既存行は再計算されないので、式を変えるなら同じマイグレーションで
-- 再計算（generated column は直接 UPDATE できないため DROP COLUMN → ADD COLUMN
-- のやり直しになる）を書くこと。
--
-- 運用ノート（PR #187 レビュー O6）: この ALTER TABLE は全行を書き換えるため
-- ACCESS EXCLUSIVE ロックで recordings への読み書きを止める。直後の 3 本の
-- CREATE INDEX も非 CONCURRENTLY なので同様に書き込みをブロックする。
-- 既存のマイグレーション（00020 等）と同じ流儀であり、録画テーブルの行数が
-- 世帯スケール（docs/data.md §5「規模感」）である前提では許容範囲。
ALTER TABLE recordings ADD COLUMN genre_lv1 smallint[]
  GENERATED ALWAYS AS (genre_lv1_of(genres)) STORED;

CREATE INDEX recordings_genre_lv1_gin ON recordings USING gin (genre_lv1);

-- キーセットページング用の複合インデックス。同一 program_start_at の録画
-- （同時刻開始の別チャンネル）は普通に発生するため、program_start_at 単独では
-- ページ跨ぎで重複・欠落が出る。(program_start_at, id) の複合で割ることで
-- tie-breaker の id が安定した全順序を保証する。
--
-- 00002_schema_v1.sql の単独インデックス（recordings_program_start_at_idx）は
-- この複合インデックスの先頭列と同じ並びなので、単独インデックスでしか
-- 応えられるクエリが無くなる（複合インデックスの先頭列プレフィックスで代替可能）。
-- 書き込みコストだけが残るので落とす。
DROP INDEX IF EXISTS recordings_program_start_at_idx;

CREATE INDEX recordings_program_start_at_id_idx
  ON recordings (program_start_at DESC, id DESC);

-- +goose Down

CREATE INDEX recordings_program_start_at_idx ON recordings (program_start_at DESC);
DROP INDEX IF EXISTS recordings_program_start_at_id_idx;

DROP INDEX IF EXISTS recordings_genre_lv1_gin;
ALTER TABLE recordings DROP COLUMN IF EXISTS genre_lv1;
DROP FUNCTION IF EXISTS genre_lv1_of(jsonb);

DROP INDEX IF EXISTS recordings_description_trgm;
DROP INDEX IF EXISTS recordings_title_trgm;
