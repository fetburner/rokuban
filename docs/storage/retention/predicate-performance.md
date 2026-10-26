> [storage.md](../../storage.md) §7 の一部。索引から辿る

## 削除述語の性能測定（§7 の一部）

until_encoded の削除可否について、置き場の条件の無い view と、置き場の条件を足した関数の計画と時間を測った。

### 置き場の条件の無い view の計画と時間

TS の計測済み判定を JOIN から副問い合わせに変えると、50% の合成データで 2,939 ms が 44 ms になった。

PostgreSQL 16 で、原本 2 万、現行サイズの計測済み 1.9 万、encoded 1.8 万、thumbnail 1.94 万、seek tiles 1.88 万の合成データを測った。`until_encoded` の割合ごとに、prepared statement の 6 回目（generic plan）を `LIMIT 5000` で実行した。

| クエリ（JOIN → 副問い合わせ、ms） | 100% | 50% | 20% |
|---|---:|---:|---:|
| `ListUntilEncodedOriginalsToDelete` | 83 → 64 | 2,939 → 44 | 24 → 22 |
| `ListMediaAssetsPendingDelete` | 101 → 104 | 4,242 → 63 | 33 → 28 |

50% の JOIN は `Rows Removed by Join Filter` が最大 1 億 469 万行に達した。副問い合わせの形で二乗を避けられるのは PostgreSQL 16 の planner での実測であり、契約ではない。本番データでの計画と時間は未測定である。

### 置き場の条件を足した関数の計画と時間

置き場の条件を足した関数は、部分索引があれば、置き場の条件の無い view と同じ桁の時間で動いた。

#### 測定条件

本番と同じスキーマの専用 DB に、置き場のコピー表だけを足して測った。

[測定用 SQL](../../../scripts/measure-until-encoded-store-predicate.sql) は、本番の migration を当てた専用 DB の上で動く。view、表、索引、`trash_deletable_recordings`、`removed_encoded_assets` は本物で、足すのは `media_asset_copies`、置き場を引数に取る関数、コピー表の候補索引だけである。表はすべて本表で、データを入れた後に `VACUUM (ANALYZE)` した。

- PostgreSQL 16.2（macOS の Postgres.app、1 台）
- recordings 2 万。1.95 万が生きている録画で、500 がごみ箱の録画
- 原本 2 万（`deleting` が 1,000）、encoded（h264）1.8 万、thumbnail 1.94 万、seek tiles 1.88 万、現行サイズの TS 計測 1.9 万
- `encode_profiles` は 1 要素（h264）。2 要素（h264 と h265）は 50% の producer だけ測った。h265 の encoded は 1.7 万件で、tokyo にだけコピーがある
- CM 検出は 4,000 件で有効。検出済みが 2,000 件、`stage = 'match'` の `failed` が 1,000 件、`stage = 'adopt'` の `failed` が 1,000 件で、残りは試行が無い
- 作り直し要求は 360 件。未充足が 178 件で、残りは同じ区間の凍結 cut を持つ encoded がある
- コピー 9.7 万行（2 要素の測定では、これに h265 の 1.7 万行を足した）。`state` は `active` 8.5 万、`deleted` 1.2 万、`deleting` 540 で、`pending` は無い。置き場は `tokyo` と `takamatsu`。encoded 1 資産あたり 1〜2 コピーで、encoded 以外は `tokyo` に 1 コピー。`deleted` は両置き場に 6,000 行ずつ
- 関数の引数は `'tokyo'`。1 回目から 5 回目までは custom plan、6〜8 回目を `EXPLAIN (ANALYZE, BUFFERS)` で測り、時間は 3 回の中央値（ms）である。`pg_prepared_statements` は全 statement で `custom_plans = 5`、`generic_plans = 3` を示した
- 本物の view を引いている証拠として、全 statement の計画に `encoded_asset_rebuild_requests` と `recording_cm_attempts` へのアクセスが出る

関数の形は 3 つ測った。結果の集合は同じで、スクリプトが `EXCEPT` で確かめる。

- join 形: `unencoded_desired_profiles` を参照せず、profile ごとの `NOT EXISTS` の中で encoded とコピーを JOIN する
- view 参照形: view と同じ `unencoded_desired_profiles` の参照を残し、desired の置き場にコピーがあることの確認を `NOT EXISTS` で足す
- count 形: desired の profile の数と、desired の置き場に active なコピーがある encoded の profile の数を、候補ごとの相関スカラー副問い合わせで比べる

コピー表の索引は 4 通り測った。

- 索引なし: `(store, rel_path)` の一意索引と主キーだけ
- 部分: `(media_asset_id, store) WHERE state = 'active'`
- 全体: `(media_asset_id, store, state)`
- store 先頭の部分: `(store, media_asset_id) WHERE state = 'active'`

#### producer の結果

置き場の条件を足すと、100% と 50% で 1 パスが 15〜31 ms 長くなり、20% では 4 ms 短くなった。

producer は `SELECT * FROM <述語> LIMIT 5000` である。20% の行数は、view が 3,433、関数が 2,580 だった。それ以外は `LIMIT` の 5,000 行に達した。

| 述語 | 100% | 50% | 20% | コピー表の引き方（候補ごと） | 最上位の結合 |
|---|---:|---:|---:|---|---|
| 置き場の条件の無い view | 42.8 | 37.7 | 33.4 | 引かない | Nested Loop Semi Join |
| join 形・部分索引 | 73.6 | 52.3 | 29.1 | Index Only Scan（部分索引） | Nested Loop Semi Join |
| join 形・全体索引 | 79.0 | 54.6 | 29.7 | Index Only Scan（全体索引） | Nested Loop Semi Join |
| join 形・store 先頭の部分索引 | 78.3 | 54.4 | 29.7 | Index Only Scan（store 先頭） | Nested Loop Semi Join |
| join 形・索引なし | 未測定 | 20,226.0 | 未測定 | Seq Scan（8,479 loops） | Nested Loop Semi Join |
| view 参照形・部分索引 | 87.1 | 67.4 | 36.0 | Index Only Scan（部分索引） | Nested Loop Semi Join |
| count 形・部分索引 | 55.1 | 59.9 | 31.3 | Index Only Scan（部分索引） | Nested Loop Semi Join（50% は Nested Loop） |

最上位の結合の見積もりは、view が 1 行だった。join 形は 100% で 4 行、50% で 2 行、20% で 1 行だった。実際はどちらも `LIMIT` の 5,000 行か 20% の行数で、見積もりは大きく外れている。

`encode_profiles` が 2 要素で 50% のとき、view は 44.0 ms、join 形・部分索引は 63.2 ms だった。

同じスクリプトを書き換えながら 4 回実行し、join 形・部分索引の 50% は 52.3〜62.9 ms だった。実行間のずれは最大 10.6 ms（52.3 と 62.9）である。

#### 消費クエリの結果

join 形の増分は、100% の入口クエリで 39.6 ms、50% で 5.0 ms で、20% では短くなった。

`delete_reconcile.sql` の 3 クエリを、述語だけ差し替えて測った。`ListMediaAssetsPendingDelete` のごみ箱腕と外した版の腕は、本物の関数と view である。`deleting` の原本は 1,000 件で、250 件が encoded を持つ生きた録画、250 件が encoded を持たない生きた録画、500 件がごみ箱の録画である。外した版の要求は空である。

| クエリ | 述語 | 100% | 50% | 20% |
|---|---|---:|---:|---:|
| `ListUntilEncodedOriginalsToDelete` | 置き場の条件の無い view | 87.8 | 56.8 | 33.8 |
| | join 形・部分索引 | 127.4 | 61.8 | 29.1 |
| | view 参照形・部分索引 | 126.8 | 75.7 | 35.8 |
| | count 形・部分索引 | 141.6 | 71.4 | 31.5 |
| `ListMediaAssetsPendingDelete` | 置き場の条件の無い view | 95.2 | 59.6 | 34.4 |
| | join 形・部分索引 | 110.4 | 61.3 | 30.0 |
| | view 参照形・部分索引 | 128.4 | 76.3 | 36.3 |
| | count 形・部分索引 | 153.1 | 71.8 | 32.8 |
| `ListUnqualifiedDeletingAssets` | 置き場の条件の無い view | 8.9 | 240.0 | 106.2 |
| | join 形・部分索引 | 10.2 | 188.1 | 86.3 |
| | view 参照形・部分索引 | 397.7 | 202.8 | 99.1 |
| | count 形・部分索引 | 416.3 | 205.3 | 90.4 |

`ListUnqualifiedDeletingAssets` は、述語を置き場の条件の無い view にしたままでも 50% で 240.0 ms、20% で 106.2 ms かかる。これは置き場の条件を足す前からある。join 形はこの 2 点で view より短く、100% では 10.2 ms で view と同程度だった。view 参照形と count 形は、100% で 397.7 ms と 416.3 ms だった。この 2 形で 100% が長い原因は未調査である。

#### 読み取れること

索引が無いと全走査になり、関数の形の差は否定形の消費クエリで大きく出た。

- 索引が無いと、候補ごとにコピー表を全走査する。50% の producer は 20,226.0 ms で、部分索引ありの 52.3 ms の約 390 倍だった。
- 3 通りの索引は、producer の時間が 100% で 73.6〜79.0 ms、50% で 52.3〜54.6 ms、20% で 29.1〜29.7 ms に収まった。索引の形による差は最大 5.4 ms で、実行間のずれより小さい。
- 置き場の条件を足した join 形は、100% の producer で view より 30.8 ms、50% で 14.6 ms 遅かった。20% は 4.3 ms 速かった。どれも `LIMIT 5000` の 1 パスの時間である。
- count 形が join 形より producer で速いのは 100% だけ（55.1 ms）である。view 参照形は 3 点とも遅い。どちらも `ListUnqualifiedDeletingAssets` の 100% が join 形の約 40 倍長い。

未測定: 本番データでの計画と時間、`pending` のコピー行を混ぜた場合、置き場が 3 つ以上の場合、PostgreSQL 16.2 以外の版。
