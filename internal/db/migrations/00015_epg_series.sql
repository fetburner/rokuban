-- +goose Up

-- epg_programs 側の自動キー（series_key）は生成列にする。recordings と同じ形で、
-- 書くループが無いので導出が事実を上書きする経路が無い（不変条件 9）。
--
-- series_key の本体を差し替えても、列を作り直す手順は要らない。STORED 生成列は
-- UPDATE でも再計算され、EPG 同期は UpsertEpgProgram の ON CONFLICT DO UPDATE で
-- 全行を書き直すので、次の同期で値が入れ替わる。
ALTER TABLE public.epg_programs
    ADD COLUMN series_key text GENERATED ALWAYS AS (public.series_key(name)) STORED;

CREATE INDEX epg_programs_series_key_idx ON public.epg_programs (series_key);

-- epg_program_series は EPG 番組の実効シリーズ。ハブの「次回」の読み手は
-- ここだけを見る（recording_series の EPG 側の対）。
--
-- **EPG 側はルールの当たりを表に持たない。** 10 分ごとに全行を書き直す同期に
-- 再評価の契機をもう 1 つ足すことになるためで（docs/data/series.md §8
-- 「評価結果の持ち方」）、勝者はここで label_rule_winner を呼んで求める。
-- 録画側が当たりの衛星表を持つのは、73,000 行の全件評価を読むたびに払えない
-- からである。EPG は同期が 10 分ごとに全量を書き直す射影で、読む側の窓も
-- 数日分に閉じるので、読みながら引く。
--
-- 値が両方向で同じ空間になることが「次回」の前提である（このビューの値と
-- recording_series の値を等号で比べる）。
--
-- **2 本の UNION ALL に割るのは、`COALESCE(勝者の値, 自動キー)` の 1 式では
-- `value = $1` をどちらの枝にも押し下げられないからである。** 1 式にすると
-- プランナは epg_programs の全行で label_rule_winner を評価してから値を比べる。
-- 136,053 行・分類ルール 51 本・sqlc / pgx 経由で実測 1.30 s（Seq Scan の
-- Filter で 135,089 行が落ちる）。2 本に割ると `value = $1` が両方の枝に入り、
--
--   - 自動キーの枝: epg_programs_series_key_idx（btree）で引く
--   - ルールの枝: 当たったルールの keyword で epg_programs_name_trgm（
--     normalize_search_text(name) の GIN）を引いてから勝者を確かめる
--
-- という形になる。同じ 136,053 行で 15 ms。EPG 側の候補が 45,351 件になる棚
-- （分類ルールが EPG の 1/3 を 1 つの棚に併合した病理的な場合）を起点にすると
-- 740 ms で、返す応答そのものが 8.8 MB になる。1 件あたり約 15 µs は行ごとの
-- label_rule_winner である。**費用は「返す行数 × ルール本」に比例する。**
--
-- ルールの枝の LIKE は label_rule_winner の照合より**広い側にだけ**ずれてよい。
-- 狭い側にずれると、勝者が居るのに枝から漏れる番組が出る（結果が減るだけで
-- 例外は出ない）。今日は同じ normalize_search_text と like_escape で同じ式に
-- してあるので、片方を変えたらもう片方も同じ PR で変える
-- （internal/db/epg_series_test.go が狭い側のずれを検出する）。
CREATE VIEW public.epg_program_series AS
SELECT p.site, p.program_id, lr.value_key AS value
FROM public.epg_programs p
JOIN public.label_rules lr
  ON public.normalize_search_text(p.name) LIKE ('%' || lr.keyword_key || '%') ESCAPE '\'
 AND public.label_rule_winner(p.name) = lr.id
UNION ALL
SELECT p.site, p.program_id, p.series_key
FROM public.epg_programs p
WHERE public.label_rule_winner(p.name) IS NULL;

-- +goose Down

DROP VIEW public.epg_program_series;
DROP INDEX public.epg_programs_series_key_idx;
ALTER TABLE public.epg_programs DROP COLUMN series_key;
