> [data.md](../data.md) §5 の一部。索引から辿る

## 5. 検索とルール評価の統一

### 規模感: 世帯スケールでは検索性能は問題にならない

- **録画ライブラリ**: 毎日 20 番組 x 10 年 = 約 7.3 万行。pg_trgm GIN 付きの部分一致はこの 100 倍でもミリ秒台
- **EPG プロジェクション**: 8 日分 x 全サービスで数万〜20 万行。**録画数と違い増えない**（ローリングウィンドウ）ので規模は永久に有界
- **ルール評価**: 数十〜数百ルール x EPG 差分数千番組。バッチで秒未満

「録画が増えると検索が遅くなる」は世帯スケールでは実質起きない。

### ルール評価も Postgres で行う（検索とエンジン統一）

ルールとは実質「保存された検索」。UI 検索と ruler 評価が別エンジン（Go RE2 と Postgres）だと「検索では出るのにルールにマッチしない」という説明不能な不整合が生まれるため、両方 Postgres に寄せる。

- 正規表現方言は **POSIX ARE** の 1 つに統一
- pg_trgm の GIN インデックスは LIKE だけでなく**正規表現マッチ（`~`）も加速**するので、regex ルールもインデックスに乗る
- Postgres の regex は UTF-8 を文字単位で扱うため、EPGStation#562（サロゲートペア等）は再現しない
- **ARE は先読み `(?=)` も後読み `(?<=)` も使える**（PostgreSQL 16.2 で測定・確認）。非互換になるのは JS の名前付きキャプチャ `(?<name>...)` のような POSIX ARE に無い拡張構文（測定: `SELECT '' ~ '(?<name>foo)'` は構文エラー）。`rokuban import epgstation` 時に各ルールの正規表現を Postgres で検証し、非互換は警告してユーザーに修正を促す（差分テストの allowlist にも追加）

### 全角/半角の揺れは immutable 関数 + 式インデックスで吸収する

「ＮＨＫ」というルールで「NHK」と書かれた番組にマッチさせる正規化は必要（EPGStation の `halfWidthKeyword` 系カラムが担っていた実問題）。ただし正規化済みの**重複カラムをアプリコードで維持する方式は採らない**。immutable な正規化関数を SQL で定義し、`epg_programs` に式 GIN インデックスを張る。

- 列ではなくインデックスなので、同期漏れ（アプリが正規化列の更新を忘れる）が構造的に起きない
- 検索 UI と ruler 評価が同じ式を通るので、両者のマッチ結果は定義から一致する

### PGroonga は最初から入れない

pg_trgm（標準 contrib、運用コストゼロ）で始め、形態素解析ベースの検索が欲しくなったら段階的に PGroonga に伸ばす。EPGStation が持っていた半角変換カラム群のような自前検索基盤は作らない。

### 録画検索は rulequery を共有しない

`GET /api/recordings` の絞り込み（[api.md](../api.md) 「録画一覧: 絞り込み + キーセットページング」）は `/search`（EPG 検索）や ruler 評価と同じ `internal/rulequery` に相乗りしない。`recordings` を `internal/rulequery.Compile` の第 2 のターゲットにはしない。理由は 3 つ:

1. **EPG は消えるが録画は残る。** `recordings` は放送の事実を自前の列（`title` / `description` / `genres` / `channel_type` / `service_name`）に凍結した永続資産（§6「録画した番組の情報は予約〜ingest 時点で録画行に非正規化スナップショット」、[projections.md](projections.md)）。`epg_programs` に JOIN して検索すると、EPG のローリングウィンドウから外れた古い録画では条件が**沈黙して 0 件**になる（絞り込みとして最悪の壊れ方）
2. **列の形が違う。** `title` vs `name`、`genres jsonb`（`recordings`）vs `genre_lv1 smallint[]`（`recordings` 側は生成列。`epg_programs.genre_lv1` は worker の River 定期ジョブ `epg_sync` が書く普通の列で、書き手が違う）、`channel_type` は `recordings` が直持ちで JOIN が要らない。1 つのコンパイラに 2 つのテーブルを食わせると、列名マップの分だけ「片方でしか通らない条件」が生まれる
3. **問いが違う。** 録画検索の主役は EPG に存在しない軸（`status` / `source` / `rule_id` / ごみ箱）で、逆にルール条件が持つ曜日ビットマスクや negate 付き正規表現の複数 AND は録画検索には過剰

共有するのは**キーワードの正規化方言だけ**（`internal/rulequery.KeywordClause`。`compileTextMatch` と録画検索の両方が呼ぶ）。同じ語で `/search` と録画一覧の当たり方が変わるとユーザーに説明できないため、`normalize_search_text` を通す・通さないの判定はここだけ揃える。テーブルや列名のマッピングは持たない --- 呼び出し側が列名（`p.name` / `r.title` 等）を渡す。

### 性能上の実注意点（検索ではない）

1. **EPG テーブルの churn / bloat**: 1 日に何度も大量 upsert されるため、遅くなるとしたら検索でなく書き込みと autovacuum の追従。バッチ upsert、GIN fastupdate、テーブル別 autovacuum チューニングで対処（運用ノート）
2. **番組表グリッドのペイロード**: 検索性能でなく転送量と描画の問題。決定済みの仮想化（TanStack Virtual）+ API の時間窓・サービス絞り込みで対処

## 経緯と失敗事例

- 全角/半角正規化の immutable 関数 + 式インデックスは M2 で導入
- 「録画検索は rulequery を共有しない」の決定は M3-24
