> [docs/schema.md](../schema.md)（索引）の分割本文。`rules` 一式（`rules` + 条件の子テーブル 6 つ）。DDL の権威は `internal/db/migrations`（`rules` テーブル定義。dedupe の値域 CHECK も同定義内）。エンジン側の意味論（評価・重複排除・サイトの扱い）は [録画エンジン](../recording.md) §3・§3.1。

## rules — 録画ルール（永続資産）

ルールの表は、ユーザーが書く永続資産である。単一値の条件は列で持ち、空は問わないを表す。複数値の条件は子テーブルで持つ。ルールはサイトに従属しないグローバルな資産で、サイトは条件の一次元にすぎない。

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
    -- 値域の最終防御として後から追加（一次防御は API 層の validateRuleInput）
    CONSTRAINT rules_dedupe_threshold_range
        CHECK (dedupe_threshold IS NULL
               OR (dedupe_threshold > 0 AND dedupe_threshold <= 1)),
    CONSTRAINT rules_dedupe_window_positive
        CHECK (dedupe_window IS NULL OR dedupe_window > interval '0')
);
```

<details><summary>根拠</summary>

- jsonb で持つ列: `metadata`
- 原則の出所: §1「型の規律」

</details>

### 列の持ち方

条件とオプションは型付きの列と子テーブルで持ち、jsonb で持つのはメタデータだけである。内容でクエリするなら型付き列にする原則による。

ルールはサイトに従属しないグローバルな永続資産で、**サイト列を持たない。** サイトは条件の一次元として、子テーブルで持つ。

行の列で表す条件は単一値のものだけで、NULL は問わないを意味する。複数値の条件（テキスト・サービス・チャンネル種別・ジャンル・時間帯・サイト）は、すべて子テーブルで持つ。

<details><summary>根拠</summary>

- サイトの子テーブル: `rule_sites`

</details>

### 重複排除の値域

重複排除の閾値は 0 を超え 1 以下、時間窓は正の長さに限る。時間窓なしは NULL で表し、0 は時間窓なしの意味にならない。

類似度の値域が 0 以上 1 以下なので、0 は恒真になり、マッチする全番組の録画が気づかないうちに止まる。1 を超える値は恒偽で、重複排除が気づかないうちに無効化される。時間窓が 0 以下の場合も恒偽である。

**時間窓なしは NULL で表し（条件そのものを外す）、0 は時間窓なしの意味にならない。** 理想は表現不可能にすること（不変条件 10）である。しかし閾値と時間窓の列型は一般の実数と時間間隔で、専用のドメイン型を導入しない限り無理なので、CHECK を最終防御として置く。一次防御は、API 層の人間可読なエラーである。時間窓が NULL のときの意味（無制限）と評価順は [録画エンジン](../recording.md) §3.1 にある。

<details><summary>根拠</summary>

- 値域: `dedupe_threshold` は `(0, 1]`、`dedupe_window` は `> 0`（`dedupe_window <= 0` は恒偽）
- 類似度の値域: `similarity()` が [0, 1]
- 一次防御: `internal/api/rules.go` の `validateRuleInput`
- 最終防御の CHECK: `rules_dedupe_threshold_range` / `rules_dedupe_window_positive`

</details>

### 原本保持とエンコードの組み合わせ

原本保持が「エンコード後に消す」のとき、エンコードプロファイルが空の組み合わせは CHECK で拒否する。エンコードされないまま原本が消える組み合わせを、表現不可能にするためである。

<details><summary>根拠</summary>

- 原本保持の値: `keep_original = 'until_encoded'`
- プロファイルの列: `encode_profiles`（空は `cardinality(encode_profiles) > 0` で拒否）

</details>

### SSE ヒントと予約済みキー

ルールの行トリガーが、トピック rules で SSE ヒントを通知する。

メタデータには、予約済みのキーが 1 つある。EPGStation 側のルール id を持ち、EPGStation からの移行コマンドが冪等キーとして使う。内容でクエリする以上、型付き列にする原則からは外れる。だが 1 用途だけの一過性キーに列を割くほどではないという判断である。他の用途でこのキーを上書きしない。

<details><summary>根拠</summary>

- 行トリガー: `rules_notify`。トピックは `rules`
- 予約済みキー: `metadata.epgstation.ruleId`。移行コマンドは `rokuban import epgstation --rules`
- 引き方: `metadata @> '{"epgstation":{"ruleId": N}}'`

</details>

### array_is_canonical_set — 正規集合チェック

正規集合チェックは、同じ集合が複数の表現を持つ状態を、表現不可能にする関数である。エンコードプロファイルの重複・空文字・NULL 要素・非正規順を拒否する（不変条件 10）。

```sql
CREATE FUNCTION array_is_canonical_set(a text[]) RETURNS boolean
IMMUTABLE STRICT LANGUAGE sql AS $$
  SELECT array_position(a, NULL) IS NULL
     AND NOT ('' = ANY(a))
     AND a = (SELECT coalesce(array_agg(DISTINCT x ORDER BY x), '{}') FROM unnest(a) x)
$$;
```

<details><summary>根拠</summary>

- 拒否の対象の列: `encode_profiles`

</details>

## 条件の子テーブル 6 つ

条件の子テーブルは 6 つあり、**行が無い条件次元は「問わない」を意味する。** サイトなら全サイトで、他の条件も同じ規約である。サイト名には外部キーを張らず、書き込み時にレジストリと照合し、保存済みの名前は照合を免除する。どの子テーブルも、ルールの削除に連動して消える。

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

<details><summary>根拠</summary>

- 連動して消える設定: `rule_id` の FK が `ON DELETE CASCADE`
- 全サイトの例: `rule_sites`

</details>

### サイト条件の意味

サイトを指定しないルールは、マッチした全サイトで N 予約を実体化する。複数録画をドロップ統計で選別する運用を、基本の運用として扱う（[録画エンジン](../recording.md) §3.1「サイトの扱い」）。サイト名は安定識別子で、リネームは運用作業である。

<details><summary>根拠</summary>

- 未指定 = 全サイト: `rule_sites`

</details>

### サイト名の存在制約と書き込み時の照合

サイト名の列には外部キーを張らない。サイトのレジストリは設定ファイルにあり（§1「サイトスコープ」）、外部に権威があるものは存在を制約できないからである。**存在制約の代わりに、書き込み時の照合を置く。**

各サイト名を、そのプロセスが読んでいるレジストリ（`GET /api/sites` と同じ一覧）に照合し、未知の名前（タイポを含む）なら 400 を返す。空文字列も 400 で、「未指定 = 全サイト」を意味する個別の要素としては扱わない。絞り込みの意図が気づかないうちに全サイトへ反転するのを防ぐためである。全サイトを意図するなら、サイト条件の全体を省略するか、空配列にする。

<details><summary>根拠</summary>

- 外部キーを張らない列: `rule_sites.site`
- 照合の実装: `validateRuleSites`（`internal/api/rules.go`）
- 照合先: `config.mirakcs` レジストリ
- 載せ直す条件: `sites`（全サイトを意図するなら省略か空配列）

</details>

### 保存済みの名前を照合から外す

照合は、保存済みの名前に及ばせない。更新では、そのルールに既に保存されているサイト名は、レジストリに無くても通す。子表は全置換なので、更新ではサイト条件を毎回載せ直す必要があり、クライアントは GET で得た値をそのまま送る（web の編集フォームもそうしている）。

**保存済みの名前まで照合すると、サイトをリネーム・削除すると、そのサイトを持つ全ルールが保存できなくなる。** 名前や優先度を直すだけの編集も含む。レジストリから消えたサイトの掃除は、書き込み時照合の仕事ではない。照合が狙うタイポは、定義上「保存済みに無い新しい名前」なので、免除しても検出力は落ちない。

<details><summary>根拠</summary>

- 載せ直しの保全: `web/src/lib/program-search.test.ts` の preserve テスト
- 保存済みの消えたサイト名を載せ直す PATCH は通り、新しい未知の名前を足す PATCH は 400 になること（両方向）: `TestRuleSites_RegistryDriftAllowsRoundTripUpdate`

</details>

### 免除の範囲

免除の範囲はルールの同一性であり、作成には及ばない（未解決）。既存ルールを下敷きにするフォークは、サイトがレジストリから消えていると拒否される。

免除集合は、そのルールのサイト条件だからである（フォークは[フロントエンド](../frontend/search.md) §「UI が持たない次元は勝手に埋めない」）。レジストリからサイトが消えた後も、そのサイトを持つルールの編集（`PATCH`）は通る。フォーク（`POST`）だけが 400 になる。サイト条件は条件 UI に出していないので、UI 内で外すこともできない。復旧手段は、設定を戻すか API を直叩きするかの 2 つだけである。免除が守りたい範囲は、本当は「クライアントが GET で読んで載せ直した名前」であって、「そのルール」ではない。

**未解決: 免除を作成側にも届かせる（複製元を受け取る）か、フォーク時に web が「レジストリに無いサイト」を見せて明示的に外させるか。** どちらも API / UI の形を決める判断なので、書き込み時照合と同じ PR では決めない。気づかないうちに落とす選択だけは取らない。絞り込みが無音で全サイトへ反転するためである。

<details><summary>根拠</summary>

- 免除集合: そのルールの `rule_sites`
- 条件 UI に出していないサイト条件: `sites`
- フォークが 400 `unknown site` になり、編集は通ること: `TestRuleSites_RegistryDriftRejectsFork`

</details>

### 空文字列の拒否の置き場

空文字列の拒否は、スキーマの最小長の制約にしない。このリポジトリには OpenAPI のリクエスト検証ミドルウェアが無く、スキーマに書いた制約を強制する主体が居ない。強制できるのは書き込み時照合だけで、保存済みの名前の免除もスキーマでは表現できない。

そこで、同じフィールドの検証権威を 2 つに割らず、ハンドラ 1 箇所に集める。OpenAPI の description は、400 になる条件の記述に留める。

<details><summary>根拠</summary>

- 使わない制約: `items.minLength: 1`
- 検証ミドルウェアが無いこと: `kin-openapi` 系の依存は無し、`minLength` は `openapi.yaml` 全体で 0 件
- 検証の権威: `validateRuleSites`

</details>

### 勝者ルールだけを記録する

予約が持つルールの参照は、勝者ルールだけである。負けたルールは記録しない。ルールの削除は、予約を 2 つのクエリで引き、どちらも勝者の参照で引く。そのため、負けたルールを削除しても無効化しても、予約は 1 行も変わらない。

全マッチの逆引きが必要になれば、有効なルールを順に回せば同じ集合が作り直せる。ruler が毎パスやっている計算そのものである。

<details><summary>根拠</summary>

- 勝者ルールを持つ列: `reservations.rule_id`
- 2 つのクエリ（`internal/db/queries/rules.sql`）: `DeleteReservationsByRuleWithoutIntent` / `CountReservationsByRuleWithIntent`
- 逆引き: enabled ルールを `rulequery.MatchProgramIDsForRule` で回す

</details>

## 他テーブルへの FK

ルールの削除で、予約と録画履歴の行は消えず、参照だけが NULL になる。トレーサビリティは失うが、履歴は残る。ルール削除時の予約側の同期削除は [reservations.md](reservations.md) §3 冒頭にある。

```sql
ALTER TABLE reservations ADD CONSTRAINT reservations_rule_id_fkey
    FOREIGN KEY (rule_id) REFERENCES rules (id) ON DELETE SET NULL;
ALTER TABLE recordings ADD CONSTRAINT recordings_rule_id_fkey
    FOREIGN KEY (rule_id) REFERENCES rules (id) ON DELETE SET NULL;
```
