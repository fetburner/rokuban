> [docs/schema.md](../schema.md)（索引）の分割本文。`rules` 一式（`rules` + 条件の子テーブル 6 つ）。DDL の権威は `internal/db/migrations/00006_rules.sql`（dedupe の値域 CHECK は `00016_dedupe_range_check.sql`）。エンジン側の意味論（評価・重複排除・サイトの扱い）は [録画エンジン](../recording.md) §3・§3.1。

## rules — 録画ルール（永続資産）

ユーザーが書く永続資産。条件とオプションは型付き列 + 子テーブルで持ち、jsonb は `metadata` のみ（§1「型の規律」: 内容でクエリするなら型付き列）。

```sql
CREATE TABLE rules (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name              text    NOT NULL,
    description       text    NOT NULL DEFAULT '',
    enabled           boolean NOT NULL DEFAULT true,
    priority          integer NOT NULL DEFAULT 10,
    -- 条件のうち単一値（NULL = 問わない）
    is_free           boolean,
    duration_min_ms   bigint,
    duration_max_ms   bigint,
    period_start_at   timestamptz,
    period_end_at     timestamptz,
    -- 履歴ベース重複排除
    dedupe_enabled    boolean NOT NULL DEFAULT false,
    dedupe_threshold  real,
    dedupe_window     interval,
    -- base の材料
    keep_original     text    NOT NULL DEFAULT 'always'
                              CHECK (keep_original IN ('always', 'until_encoded')),
    encode_profiles   text[]  NOT NULL DEFAULT '{}'
                              CHECK (array_is_canonical_set(encode_profiles)),
    filename_template text    NOT NULL DEFAULT '',
    metadata          jsonb   NOT NULL DEFAULT '{}',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CHECK (duration_min_ms IS NULL OR duration_max_ms IS NULL
           OR duration_min_ms <= duration_max_ms),
    CHECK (dedupe_enabled = false OR dedupe_threshold IS NOT NULL),
    CHECK (keep_original <> 'until_encoded' OR cardinality(encode_profiles) > 0),
    -- 00016 で追加（値域の最後の砦。一次防御は API 層の validateRuleInput）
    CONSTRAINT rules_dedupe_threshold_range
        CHECK (dedupe_threshold IS NULL
               OR (dedupe_threshold > 0 AND dedupe_threshold <= 1)),
    CONSTRAINT rules_dedupe_window_positive
        CHECK (dedupe_window IS NULL OR dedupe_window > interval '0')
);
```

- **`rules` に site 列は無い。** ルールはサイトに従属しないグローバルな永続資産で、サイトは条件の一次元（下記 `rule_sites`）
- **単一値の条件は NULL = 問わない。** 行の列で表すのは単一値の条件だけで、複数値の条件（テキスト・サービス・チャンネル種別・ジャンル・時間帯・サイト）はすべて子テーブル
- **`dedupe_threshold` の値域は `(0, 1]`、`dedupe_window` は `> 0`**（`00016`）。`similarity()` の値域が [0, 1] なので、0 は恒真（マッチする全番組の録画が黙って止まる）・1 超は恒偽（重複排除が黙って無効化）になる。`dedupe_window <= 0` も恒偽。**0 は「時間窓なし」ではない** —— 窓なしは NULL（条件そのものを外す）。理想は表現不可能にすること（不変条件 10）だが、real / interval という一般の列型では専用ドメイン型を導入しない限り無理なので、CHECK を最後の砦として置く。一次防御は API 層（`internal/api/rules.go` の `validateRuleInput`）の人間可読なエラー
- **`keep_original = 'until_encoded'` は空の `encode_profiles` と両立しない**（CHECK）。エンコードされないまま原本が消える組み合わせを表現不可能にする
- `dedupe_window` が NULL のときの意味（無制限）や評価順は [録画エンジン](../recording.md) §3.1
- SSE ヒント: `rules` の行トリガー `rules_notify` がトピック `rules` で通知する

### array_is_canonical_set — 正規集合チェック（`00006`）

```sql
CREATE FUNCTION array_is_canonical_set(a text[]) RETURNS boolean
IMMUTABLE STRICT LANGUAGE sql AS $$
  SELECT array_position(a, NULL) IS NULL
     AND NOT ('' = ANY(a))
     AND a = (SELECT coalesce(array_agg(DISTINCT x ORDER BY x), '{}') FROM unnest(a) x)
$$;
```

`encode_profiles` の重複・空文字・NULL 要素・非正規順を拒否する。「同じ集合が複数の表現を持つ」状態を表現不可能にする（不変条件 10）。

## 条件の子テーブル 6 つ

すべて `rule_id` の FK が `ON DELETE CASCADE`。**子テーブルに行が無い条件次元は「問わない」**（`rule_sites` なら全サイト。他の条件テーブルも同じ規約）。

```sql
-- テキスト条件（キーワード / 正規表現）。seq は評価順・表示順。
-- target: name / description / extended
-- mode: keyword（部分一致）/ regex（POSIX ARE ~）
CREATE TABLE rule_text_matches (
    rule_id         bigint  NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    seq             integer NOT NULL CHECK (seq >= 0),
    target          text    NOT NULL CHECK (target IN ('name', 'description', 'extended')),
    mode            text    NOT NULL CHECK (mode IN ('keyword', 'regex')),
    value           text    NOT NULL CHECK (value <> ''),
    case_sensitive  boolean NOT NULL DEFAULT false,
    negate          boolean NOT NULL DEFAULT false,
    PRIMARY KEY (rule_id, seq)
);

CREATE TABLE rule_services (
    rule_id    bigint  NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    PRIMARY KEY (rule_id, network_id, service_id)
);

CREATE TABLE rule_channel_types (
    rule_id       bigint NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    channel_type  text   NOT NULL CHECK (channel_type IN ('GR', 'BS', 'CS', 'SKY')),
    PRIMARY KEY (rule_id, channel_type)
);

CREATE TABLE rule_genres (
    rule_id   bigint   NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    genre_lv1 smallint NOT NULL CHECK (genre_lv1 BETWEEN 0 AND 15),
    PRIMARY KEY (rule_id, genre_lv1)
);

-- weekdays: bit0=月 … bit6=日（1..127）。start_sec/end_sec は 0..86400（end は翌日跨ぎ可）
CREATE TABLE rule_times (
    rule_id    bigint  NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    seq        integer NOT NULL CHECK (seq >= 0),
    weekdays   integer NOT NULL CHECK (weekdays BETWEEN 1 AND 127),
    start_sec  integer NOT NULL CHECK (start_sec BETWEEN 0 AND 86400),
    end_sec    integer NOT NULL CHECK (end_sec BETWEEN 0 AND 86400),
    PRIMARY KEY (rule_id, seq)
);

-- 指定なし = 全サイト。site は設定レジストリ由来（FK なし）
CREATE TABLE rule_sites (
    rule_id bigint NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    site    text   NOT NULL CHECK (site <> ''),
    PRIMARY KEY (rule_id, site)
);
```

- **`rule_sites` 未指定 = 全サイト。** 実体化はマッチした全サイトで N 予約（複数録画 → ドロップ統計で選別する運用を一級とする。[録画エンジン](../recording.md) §3.1「サイトの扱い」）。サイト名は安定識別子でリネームは運用作業
- **`rule_sites.site` に FK は張らない。** サイトのレジストリは設定ファイルにあり（§1「サイトスコープ」）、外部に真実があるものは存在を制約できない。**書き込み時のレジストリ照合は `validateRuleInput`（`internal/api/rules.go`）が担う**: 各 site 名をそのプロセスが読んでいる `config.mirakc` / `mirakcs` レジストリ（`GET /api/sites` と同じ一覧）に照合し、未知（タイポ含む）なら 400 で拒否する。空文字列は「未指定 = 全サイト」の個別要素ではなく、挿入時に無視される（従来どおり）。レジストリから site が消えたあとに残る既存行はこの照合の対象外

- **`reservations.rule_id` が持つのは勝者ルールのみ。** 負けたルールは記録しない ---
  `DeleteReservationsByRuleWithoutIntent` / `CountReservationsByRuleWithIntent`
  （`internal/db/queries/rules.sql`）はどちらも `reservations.rule_id`（勝者）で
  引くので、負けたルールを削除も無効化もしても予約は 1 行も変わらない。全マッチの
  逆引きが必要になれば、enabled ルールを `rulequery.MatchProgramIDsForRule` で
  回せば同じ集合が作り直せる（ruler が毎パスやっている計算そのもの）

## 他テーブルへの FK（`00006` で追加）

```sql
ALTER TABLE reservations ADD CONSTRAINT reservations_rule_id_fkey
    FOREIGN KEY (rule_id) REFERENCES rules (id) ON DELETE SET NULL;
ALTER TABLE recordings ADD CONSTRAINT recordings_rule_id_fkey
    FOREIGN KEY (rule_id) REFERENCES rules (id) ON DELETE SET NULL;
```

ルール削除で予約・録画履歴の行は消えず、参照だけ NULL になる（トレーサビリティは失うが履歴は残る）。ルール削除時の予約側の同期削除は [reservations.md](reservations.md) §3 冒頭。

## 経緯と失敗事例

- `rules` 一式は M2-1（issue #3 / #24）の成果物（`00006`）
- **dedupe の値域 CHECK（`00016`）はコードレビューで発覚した欠落。** `00006` の CHECK は
  `dedupe_enabled = false OR dedupe_threshold IS NOT NULL` しか見ておらず、値そのものの
  範囲は API 層にも DB 層にも無かった。恒真トラップ（閾値 0）は M2-5 のサーキット
  ブレーカー（削除しか守らない）にも止められない経路だった。既存の違反行は値を推測して
  丸めず、`dedupe_enabled = false` に倒して無効化した（意図不明の値で重複排除を有効の
  まま残すと「黙って録画が止まる / 黙って無効化される」症状が継続するため。詳細は
  `00016_dedupe_range_check.sql` のコメント）
