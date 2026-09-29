-- +goose Up

-- series_key は normalize(..., NFKC) を使う。これは server_encoding が UTF8 でない
-- データベースでは実行時に落ちる（生成列なので INSERT ごとに落ちる）。本番の
-- encoding は未確認なので、マイグレーションの時点で落とす。docs/data/series.md
-- §8「2 層: 分類ルール → 自動キー」。
--
-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('server_encoding') <> 'UTF8' THEN
        RAISE EXCEPTION 'series_key requires server_encoding = UTF8, got % (normalize(..., NFKC) is not available)',
            current_setting('server_encoding');
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION public.series_key(t text) RETURNS text
    LANGUAGE sql IMMUTABLE STRICT
    AS $$
  WITH a AS (
    SELECT normalize(t, NFKC) AS t
  ), b AS (
    SELECT regexp_replace(regexp_replace(t, '\[[^]]*\]', '', 'g'), '【[^】]*】', '', 'g') AS t FROM a
  ), c AS (
    SELECT substring(regexp_replace(t, '^[▽◇★☆〜「『◆■●○◎※#|]+', '')
                     FROM '^[^▽◇★☆〜「『』」◆■●○◎※#|]*') AS t FROM b
  ), d AS (
    SELECT regexp_replace(t, '第[0-9]+[話回週]|#[0-9]+|\([0-9]+\)|[0-9]+話', '', 'g') AS t FROM c
  ), e AS (
    -- 枠名（定数）を 1 語飛ばしてから最初の空白で切る。NFKC が全角空白を
    -- ASCII 空白へ畳むので、ここに来る時点で区切りは半角空白である。
    SELECT substring(regexp_replace(t, '^(アニメ|TVアニメ|日5) +', '') FROM '^[^ ]*') AS t FROM d
  )
  SELECT CASE WHEN regexp_replace(t, '[^[:alnum:]]', '', 'g') = '' THEN NULL
              ELSE nullif(btrim(t, ' 　、,・.。:：;；!！?？'), '') END FROM e
$$;
-- +goose StatementEnd

-- like_escape は LIKE の特殊文字（\ % _）を文字どおりに照合するための前置。
-- KeywordClause（internal/rulequery）と同じ規則で、4 経路（ruler・EPG 検索・
-- 録画一覧・分類ルール）が同じ関数を通る。**トリガーと全件再評価のジョブは Go を
-- 通らない**ので、分類ルールの当たりは SQL 側でエスケープできることが前提になる。
--
-- +goose StatementBegin
CREATE FUNCTION public.like_escape(s text) RETURNS text
    LANGUAGE sql IMMUTABLE STRICT
    AS $$
  SELECT replace(replace(replace(s, '\', '\\'), '%', '\%'), '_', '\_')
$$;
-- +goose StatementEnd

-- label_rules は分類ルール 1 本 = キーワード 1 つ + 棚の値 1 つ。当たった順は
-- priority DESC, id ASC で決める（同順位で勝者が不定だと棚が評価のたびに
-- 入れ替わる。docs/data/series.md §8）。
--
-- value_key は値の正規化結果を持つ生成列。集計のたびに録画ごとに正規化すると、
-- 生成列で消したコストが値の側から戻る。
--
-- CHECK (value_key IS NOT NULL) は「正規化で NULL になる値」= 何も主張しない
-- ルールを表現不可能にする（不変条件 10）。
CREATE TABLE public.label_rules (
    id bigint NOT NULL PRIMARY KEY,
    key text NOT NULL,
    value text NOT NULL,
    keyword text NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    value_key text GENERATED ALWAYS AS (public.series_key(value)) STORED,
    -- keyword_key はキーワード側の正規化（normalize_search_text(like_escape(...))、
    -- KeywordClause と同じ方言）。label_rule_winner は録画 1 行ごとに呼ばれるので、
    -- ここで持たないと (録画行 × ルール本) 回の正規化が走る（73,000 行 × 50 本の
    -- 実測で 16.0 s → 0.95 s）。
    keyword_key text GENERATED ALWAYS AS (public.normalize_search_text(public.like_escape(keyword))) STORED,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT label_rules_key_check CHECK ((key = 'series'::text)),
    CONSTRAINT label_rules_value_key_check CHECK ((value_key IS NOT NULL)),
    CONSTRAINT label_rules_keyword_check CHECK ((btrim(keyword) <> ''::text))
);

ALTER TABLE public.label_rules ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.label_rules_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

-- label_rule_hits は録画ごとに勝ったルールの id を持つ衛星表。行の存在 = その
-- ルールが勝った。当たらない録画には行を作らない（不変条件 10）。
--
-- 値ではなく id を持つので、ルールの値を変えた変更は JOIN で即座に反映される。
-- 削除の CASCADE は安全網でしかない --- 勝者が消えた録画の次点はこの表から
-- 分からないので、削除も全件の再評価が要る。
--
-- **宛先にしない**（不変条件 9）。この行を指す API・上書き操作を作らない。
CREATE TABLE public.label_rule_hits (
    recording_id bigint NOT NULL,
    label_rule_id bigint NOT NULL
);

ALTER TABLE public.label_rule_hits
    ADD CONSTRAINT label_rule_hits_pkey PRIMARY KEY (recording_id);

ALTER TABLE public.label_rule_hits
    ADD CONSTRAINT label_rule_hits_recording_id_fkey
        FOREIGN KEY (recording_id) REFERENCES public.recordings (id) ON DELETE CASCADE;

ALTER TABLE public.label_rule_hits
    ADD CONSTRAINT label_rule_hits_label_rule_id_fkey
        FOREIGN KEY (label_rule_id) REFERENCES public.label_rules (id) ON DELETE CASCADE;

-- 書き手が 2 人（recordings のトリガーと全件再評価のジョブ）いるので、
-- 評価の関数を 1 つに寄せる。Go 側に複製しない。
--
-- **`norm` を MATERIALIZED にするのは必須**。インラインにすると
-- `normalize_search_text(title)` がルール本ぶん評価され、(録画行 × ルール本) 回に
-- なる（73,000 行 × 50 本の実測で 16.0 s → 0.95 s。docs/data/series.md §8）。
--
-- +goose StatementBegin
CREATE FUNCTION public.label_rule_winner(title text) RETURNS bigint
    LANGUAGE sql STABLE
    AS $$
  WITH norm AS MATERIALIZED (
    SELECT public.normalize_search_text(title) AS t
  )
  SELECT lr.id
  FROM public.label_rules lr, norm n
  WHERE n.t LIKE ('%' || lr.keyword_key || '%') ESCAPE '\'
  ORDER BY lr.priority DESC, lr.id ASC
  LIMIT 1
$$;
-- +goose StatementEnd

-- 録画の series_key（自動キー）は生成列にする。書くループが無いので、導出が
-- 事実を上書きする経路が無い（不変条件 9）。genre_lv1 と同じ形。
--
-- **series_key の本体を変えるマイグレーションは、この列を作り直さなければ
-- ならない。** 生成列は INSERT のときにだけ書かれるので、関数を差し替えても
-- 既存行は古い値のまま残る（REINDEX では直らない）。列を落とすには、先に
-- recording_series のビューを落とす必要がある。順序は
-- DROP VIEW → DROP COLUMN → ADD COLUMN → CREATE INDEX → CREATE VIEW。
ALTER TABLE public.recordings
    ADD COLUMN series_key text GENERATED ALWAYS AS (public.series_key(title)) STORED;

CREATE INDEX recordings_series_key_idx ON public.recordings (series_key);

-- EPG 側も同じ式を当てる。ハブの「次回」はこの列と label_rules から引く。
ALTER TABLE public.epg_programs
    ADD COLUMN series_key text GENERATED ALWAYS AS (public.series_key(name)) STORED;

CREATE INDEX epg_programs_series_key_idx ON public.epg_programs (series_key);

-- 録画 1 行の評価。録画を作る経路は複数あるので Go 側で呼ぶと 1 つ漏れる。
-- 「既存の当たりを消す → 勝者がいれば入れる」の 2 段にする。upsert だけだと、
-- 当たらなくなった録画に古い当たりが残る。
--
-- +goose StatementBegin
CREATE FUNCTION public.recordings_label_rule_sync() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    winner bigint;
BEGIN
    DELETE FROM public.label_rule_hits WHERE recording_id = NEW.id;
    winner := public.label_rule_winner(NEW.title);
    IF winner IS NOT NULL THEN
        INSERT INTO public.label_rule_hits (recording_id, label_rule_id)
        VALUES (NEW.id, winner);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER recordings_label_rule_sync
    AFTER INSERT OR UPDATE OF title ON public.recordings
    FOR EACH ROW EXECUTE FUNCTION public.recordings_label_rule_sync();

-- recording_series は実効シリーズ（分類ルールが当たればその値、当たらなければ
-- 自動キー）。棚とハブの読み手はここだけを見る。
CREATE VIEW public.recording_series AS
SELECT r.id AS recording_id,
       COALESCE(lr.value_key, r.series_key) AS value
FROM public.recordings r
LEFT JOIN public.label_rule_hits h ON h.recording_id = r.id
LEFT JOIN public.label_rules lr ON lr.id = h.label_rule_id;

-- 分類ルールの変更は録画の棚を変えるので recordings トピックへ通知する
-- （media_assets_notify と同じ）。keyword を変えた直後は古い棚が一度返り、
-- 全件再評価の完了後に正しい棚になる。
--
-- label_rule_hits には通知トリガーを付けない。全件再評価で行数ぶんの通知が
-- 出てしまう（変化があったときだけジョブが 1 回送る）。
CREATE TRIGGER label_rules_notify
    AFTER INSERT OR DELETE OR UPDATE ON public.label_rules
    FOR EACH ROW EXECUTE FUNCTION public.rokuban_notify('recordings');

-- +goose Down

DROP TRIGGER label_rules_notify ON public.label_rules;
DROP VIEW public.recording_series;
DROP TRIGGER recordings_label_rule_sync ON public.recordings;
DROP FUNCTION public.recordings_label_rule_sync();
DROP INDEX public.epg_programs_series_key_idx;
ALTER TABLE public.epg_programs DROP COLUMN series_key;
DROP INDEX public.recordings_series_key_idx;
ALTER TABLE public.recordings DROP COLUMN series_key;
DROP FUNCTION public.label_rule_winner(text);
DROP TABLE public.label_rule_hits;
DROP TABLE public.label_rules;
DROP FUNCTION public.like_escape(text);
DROP FUNCTION public.series_key(text);
