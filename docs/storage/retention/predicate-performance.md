> [storage.md](../../storage.md) §7 の一部。索引から辿る

## 削除述語の性能測定（§7 の一部）

until_encoded の削除可否について、置き場条件を加える前の view と、置き場条件を含む現行の関数の計画と時間を測った。

下記の測定は、`media_assets.rel_path` / `state` を読む引数なしの `until_encoded_deletable_originals` view を使った履歴資料である。この列と view は現行スキーマから削除されているため、測定用 SQL は現行の DB では実行できない。数値は現行スキーマで再測定していない。

### 置き場の条件の無い view の計画と時間

TS の計測済み判定を JOIN から副問い合わせに変えると、50% の合成データで 2,939 ms が 44 ms になった。

PostgreSQL 16 で、原本 2 万、現行サイズの計測済み 1.9 万、encoded 1.8 万、thumbnail 1.94 万、seek tiles 1.88 万の合成データを測った。`until_encoded` の割合ごとに、prepared statement の 6 回目（generic plan）を `LIMIT 5000` で実行した。

| クエリ（JOIN → 副問い合わせ、ms） | 100% | 50% | 20% |
|---|---:|---:|---:|
| `ListUntilEncodedOriginalsToDelete` | 83 → 64 | 2,939 → 44 | 24 → 22 |
| `ListMediaAssetsPendingDelete` | 101 → 104 | 4,242 → 63 | 33 → 28 |

50% の JOIN は `Rows Removed by Join Filter` が最大 1 億 469 万行に達した。副問い合わせの形で二乗を避けられるのは PostgreSQL 16 の planner での実測であり、契約ではない。本番データでの計画と時間は未測定である。

### 置き場の条件を足した関数の計画と時間

置き場の条件を足した関数は、部分索引があれば、producer の 1 パスが 100% と 50% で view より 16 ms ほど長く、20% の差は実行間のずれの内だった。

#### 測定条件

当時のスキーマを持つ専用 DB で測った。測定 SQL は、論理資産に物理パスと状態の列があった時点の view と表を使い、当時まだ無かったコピー表、置き場を引数に取る関数、コピー表の候補索引を足す。

[測定用 SQL](../../../scripts/measure-until-encoded-store-predicate.sql) は、測定時の view、表、索引、`trash_deletable_recordings`、`removed_encoded_assets` を使い、表はすべて本表で、データを入れた後に `VACUUM (ANALYZE)` した。

- PostgreSQL 16.2（macOS の Postgres.app、1 台）
- recordings 2 万。1.95 万が生きている録画で、500 がごみ箱の録画
- 原本 2 万（`deleting` が 1,000）、encoded（h264）1.8 万、thumbnail 1.94 万、seek tiles 1.88 万、現行サイズの TS 計測 1.9 万
- `deleting` の原本 1,000 件は、250 件が encoded を持つ生きた録画、250 件が encoded を持たない生きた録画、500 件がごみ箱の録画である。否定形の 2 クエリは、これを 20 件（述語を満たす 10 件と満たさない 10 件）に絞った条件でも測った。外した版の要求は空である
- `encode_profiles` は 1 要素（h264）。2 要素（h264 と h265）は 50% の producer だけ測った。h265 の encoded は 1.7 万件で、tokyo にだけコピーがある
- CM 検出は 4,000 件で有効。検出済みが 2,000 件、`stage = 'match'` の `failed` が 1,000 件、`stage = 'adopt'` の `failed` が 1,000 件で、残りは試行が無い
- 作り直し要求は 360 件。未充足が 178 件で、残りは同じ区間の凍結 cut を持つ encoded がある
- コピー 9.7 万行（2 要素の測定では、これに h265 の 1.7 万行を足した）。`state` は `active` 8.5 万、`deleted` 1.2 万、`deleting` 540 で、`pending` は無い。置き場は `tokyo` と `takamatsu`。encoded 1 資産あたり 1〜2 コピーで、encoded 以外は `tokyo` に 1 コピー。`deleted` は両置き場に 6,000 行ずつ
- 関数の引数は `'tokyo'`。1 回目から 5 回目までは custom plan、6〜8 回目を `EXPLAIN (ANALYZE, BUFFERS)` で測り、時間は 3 回の中央値（ms）である。`pg_prepared_statements` は全 statement で `custom_plans = 5`、`generic_plans = 3` を示した
- 本物の view を引いている証拠として、全 statement の計画に `encoded_asset_rebuild_requests` と `recording_cm_attempts` へのアクセスが出る

関数の形は 4 つ測った。結果の集合は同じで、スクリプトが `EXCEPT` で確かめる。

- 合成形: view と同じ `unencoded_desired_profiles` の参照を残し、「desired の active な encoded のうち、desired の置き場に active なコピーが無いものが無い」とだけ足す。不足の判定は共有する view に任せる
- view 参照形: `unencoded_desired_profiles` の参照を残し、コピーの確認を profile ごとの `NOT EXISTS` の中で encoded とコピーを JOIN して足す。不足の判定を二重に持つ
- join 形: `unencoded_desired_profiles` を参照せず、profile ごとの `NOT EXISTS` の中で encoded とコピーを JOIN する。不足の判定を写す
- count 形: desired の profile の数と、desired の置き場に active なコピーがある encoded の profile の数を、候補ごとの相関スカラー副問い合わせで比べる

コピー表の索引は 4 通り測った。大きさは `pg_relation_size` で、コピー 9.7 万行の時点の値である。

- 索引なし: `(store, rel_path)` の一意索引と主キーだけ
- 部分: `(media_asset_id, store) WHERE state = 'active'`（2,818,048 bytes）
- 全体: `(media_asset_id, store, state)`（4,136,960 bytes）
- store 先頭の部分: `(store, media_asset_id) WHERE state = 'active'`（2,818,048 bytes）

#### producer の結果

置き場の条件を足すと、100% と 50% で 1 パスが 16 ms ほど長くなり、20% の差は実行間のずれの内だった。

producer は `SELECT * FROM <述語> LIMIT 5000` である。20% の行数は、view が 3,433、関数が 2,580 だった。それ以外は `LIMIT` の 5,000 行に達した。

| 述語 | 100% | 50% | 20% | コピー表の引き方 |
|---|---:|---:|---:|---|
| 置き場の条件の無い view | 43.5 | 38.6 | 33.5 | 引かない |
| 合成形・部分索引 | 60.0 | 54.6 | 38.2 | Index Only Scan（部分索引） |
| 合成形・全体索引 | 60.4 | 55.6 | 39.0 | Index Only Scan（全体索引） |
| 合成形・store 先頭の部分索引 | 59.3 | 56.0 | 39.0 | Index Only Scan（store 先頭） |
| 合成形・索引なし | 64.2 | 59.5 | 26,292.7 | 100% と 50% は Seq Scan 1 回、20% は候補ごとの Seq Scan（3,433 loops） |
| join 形・部分索引 | 72.7 | 52.5 | 29.2 | Index Only Scan（部分索引） |
| join 形・索引なし | 未測定 | 20,248.6 | 未測定 | 候補ごとの Seq Scan（8,479 loops） |
| view 参照形・部分索引 | 84.4 | 66.1 | 35.7 | Index Only Scan（部分索引） |
| count 形・部分索引 | 54.8 | 55.0 | 31.5 | Index Only Scan（部分索引） |

最上位の結合の見積もりは、view と合成形が 1 行だった。join 形は 100% で 4 行、50% で 2 行、20% で 1 行だった。実際は `LIMIT` の 5,000 行か 20% の行数で、見積もりは大きく外れている。この外れは、「否定形の計画の切り替わり」で述べる計画の切り替わりに結びつく。

`encode_profiles` が 2 要素で 50% のとき、view は 43.7 ms、合成形・部分索引は 62.9 ms だった。

#### 消費クエリの結果

入口の消費クエリでも、合成形の増分は 100% で 27.4 ms、50% で 12.5 ms だった。20% の差は、producer で測った実行間のずれより小さい。

`delete_reconcile.sql` の 3 クエリを、述語だけ差し替えて測った。入口の `ListUntilEncodedOriginalsToDelete` は次のとおりである。

| 述語 | 100% | 50% | 20% |
|---|---:|---:|---:|
| 置き場の条件の無い view | 85.0 | 54.6 | 34.4 |
| 合成形・部分索引 | 112.4 | 67.1 | 38.6 |
| 合成形・索引なし | 113.0 | 67.5 | 25,849.3 |
| join 形・部分索引 | 118.8 | 61.4 | 29.6 |
| view 参照形・部分索引 | 121.3 | 90.0 | 36.0 |
| count 形・部分索引 | 136.8 | 71.8 | 31.9 |

`ListMediaAssetsPendingDelete` と `ListUnqualifiedDeletingAssets` は、`deleting` の原本を引く。ごみ箱腕と外した版の腕は、本物の関数と view である。次の表は `deleting` が 1,000 件の条件で、各セルは時間（ms）と、計画のなかの `Rows Removed by Join Filter` の最大値（括弧内）である。

| クエリ | 述語 | 100% | 50% | 20% |
|---|---|---:|---:|---:|
| `ListMediaAssetsPendingDelete` | 置き場の条件の無い view | 88.9（975） | 57.2（466） | 34.0（165） |
| | 合成形・部分索引 | 110.2（975） | 68.9（466） | 39.2（165） |
| | 合成形・索引なし | 112.5（975） | 68.1（466） | 26,316.5（165） |
| | join 形・部分索引 | 96.5（6,540） | 61.9（3,271） | 29.7（1,287） |
| | view 参照形・部分索引 | 125.3（6,540） | 76.5（3,271） | 37.0（1,287） |
| | count 形・部分索引 | 138.6（6,540） | 70.9（3,271） | 32.7（1,287） |
| `ListUnqualifiedDeletingAssets` | 置き場の条件の無い view | 8.9（0） | 234.9（4,185,063） | 106.3（1,620,613） |
| | 合成形・部分索引 | 9.3（0） | 203.2（3,143,690） | 95.4（1,236,248） |
| | 合成形・索引なし | 328.7（5,151,755） | 194.7（2,908,212） | 26,332.5（1,236,248） |
| | join 形・部分索引 | 8.6（1） | 186.7（2,924,092） | 86.2（1,236,238） |
| | view 参照形・部分索引 | 392.2（6,342,673） | 203.6（2,924,092） | 93.0（1,236,238） |
| | count 形・部分索引 | 405.7（6,342,673） | 206.0（3,143,690） | 90.6（1,287,627） |

`deleting` を 20 件に絞った条件は、100% と 50% で測った。`ListMediaAssetsPendingDelete` は count 形を除いて 0.5 ms 前後だった。count 形は 140.0 ms（100%）と 73.6 ms（50%）で、`deleting` の件数に関わらず長い。`ListUnqualifiedDeletingAssets` は次のとおりである。

| 述語（部分索引） | 100% | 50% |
|---|---:|---:|
| 置き場の条件の無い view | 6.1（10,000） | 6.3（10,000） |
| 合成形 | 6.1（6,000） | 5.9（7,500） |
| join 形 | 5.8（6,000） | 6.0（7,500） |
| view 参照形 | 6.2（10,000） | 6.0（10,000） |
| count 形 | 5.9（6,000） | 6.0（7,500） |

#### 否定形の計画の切り替わり

否定形の時間は、述語の書き方より、削除中の原本の件数に左右された。

`ListUnqualifiedDeletingAssets` は、`deleting` が 1,000 件のとき、計画が 2 通りに分かれた。

- 候補ごとに `Index Cond: (id = a.id)` で述語を引く計画。100% の view と合成形（捨てた行は 0）である。join 形も 100% で同じ時間（8.6 ms、捨てた行は 1）だった
- 述語の全集合を Materialize し、Nested Loop Anti Join の Join Filter で突き合わせる計画。捨てた行は 100 万〜600 万行になる。100% の view 参照形と count 形、50% と 20% の全形がこちらである。置き場の条件の無い view でも、50% で 234.9 ms、20% で 106.3 ms かかる

述語単独の最上位の見積もりは 1〜4 行で、実際は数千行だった。計画を読むと、この見積もりのために、全集合を Materialize しても安いと見積もられている。`deleting` を 20 件に絞ると、全形が 5.8〜6.3 ms に揃った。捨てた行は最大 10,000 行にとどまる。どちらの計画に落ちるかを決めるのは、述語の書き方ではなく `deleting` の件数である。

合成形の 100% は候補ごとに引く側に落ちた。ただし同じ形でも、索引なしでは 100% が 328.7 ms の Join Filter 側に落ちた。

#### 実行間のずれ

最終版のスクリプトを 2 回通しで実行した。合成形・部分索引の producer は、100% が 60.0 と 59.0、50% が 54.6 と 55.0、20% が 38.2 と 47.5 ms だった。表の値は 1 回目である。20% の差 9.3 ms が最大で、数 ms の差は読み取れない。

#### 読み取れること

索引が無いと 20% で 26 秒かかり、索引があれば合成形の増分は数十 ms にとどまった。

- 索引が無いと、候補ごとにコピー表を全走査する計画に落ちることがある。合成形は 100% と 50% では Seq Scan 1 回の計画で 64.2 ms と 59.5 ms だったが、20% は 3,433 回の Seq Scan で 26,292.7 ms になった。join 形は 50% で 20,248.6 ms だった。
- 3 通りの索引は、合成形の producer で差が最大 1.1 ms（100%）、1.4 ms（50%）、0.8 ms（20%）に収まった。実行間のずれより小さい。部分索引は全体索引より 1,318,912 bytes 小さい。
- 合成形は、100% の producer で view より 16.5 ms、50% で 16.0 ms 長かった。どれも `LIMIT 5000` の 1 パスの時間である。20% の差は実行間のずれの内だった。
- join 形と合成形の producer の差は、100% の 12.7 ms（join 形が長い）だけが実行間のずれより大きかった。
- `ListUnqualifiedDeletingAssets` の 100% で view 参照形と count 形が長いのも、「否定形の計画の切り替わり」で述べた計画の切り替わりによる。

未測定: 本番の `deleting` の行数（否定形の計画を決める変数）、本番データでの計画と時間、`pending` のコピー行を混ぜた場合、置き場が 3 つ以上の場合、PostgreSQL 16.2 以外の版。計画の切り替わりを起こす見積もりの原因は、計画の読解にとどまり、統計の差し替えなどで確かめていない。
