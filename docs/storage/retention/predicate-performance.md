> [storage.md](../../storage.md) §7 の一部。索引から辿る

## 削除述語の性能測定（§7 の一部）

現 view と置き場条件を加えた関数の計画と時間を、合成データで測った。

### 現 view の計画と時間

以前の測定では、副問い合わせにより 50% 条件の実行時間が大きく短縮した。

PostgreSQL 16 で、原本 2 万、現行サイズの計測済み 1.9 万、encoded 1.8 万、thumbnail 1.94 万、seek tiles 1.88 万の合成データを測った。`until_encoded` の割合ごとに、prepared statement の 6 回目（generic plan）を `LIMIT 5000` で実行した。

| クエリ（JOIN → 副問い合わせ、ms） | 100% | 50% | 20% |
|---|---:|---:|---:|
| `ListUntilEncodedOriginalsToDelete` | 83 → 64 | 2,939 → 44 | 24 → 22 |
| `ListMediaAssetsPendingDelete` | 101 → 104 | 4,242 → 63 | 33 → 28 |

50% の JOIN は `Rows Removed by Join Filter` が最大 1 億 469 万行に達した。副問い合わせの形で二乗を避けられるのは PostgreSQL 16 の planner での実測であり、契約ではない。本番データでの計画と時間は未測定である。

### 置き場を含む until_encoded 判定の性能


置き場名を引数に取る関数でも、汎用プランへ切り替わった後の性能を測った。

[測定用 SQL](../../../scripts/measure-until-encoded-store-predicate.sql) は、原本 2 万、TS 計測 1.9 万、encoded 1.8 万、thumbnail 1.94 万、seek tiles 1.88 万の合成データを作る。encoded のコピーは 2 置き場に 2.7 万行あり、1 資産あたり 1〜2 個、`tokyo` にあるものは 1.35 万件である。CM 検出は無効、作り直し要求は空にした。media_assets は現行 migration の各索引と `(recording_id, kind, profile) UNIQUE NULLS NOT DISTINCT` 制約索引を再現し、候補索引はコピー表だけに追加した。

PostgreSQL 17.11 で、各 `until_encoded` 比率の statement を 5 回実行してから、6〜8 回目を `EXPLAIN ANALYZE` で測った。`pg_prepared_statements` は各測定で `custom_plans = 5`、`generic_plans = 3` を示した。表の時間は 3 回の中央値である。直前の PostgreSQL 16 の表は削除 reconcile の consumer query を測っているため、時間は直接比較しない。

| 述語 | until_encoded | 適格行数 | 計画の要点 | 中央値 (ms) |
|---|---:|---:|---|---:|
| 現 view | 100% | 18,000 | Nested Loop Semi Join | 24.80 |
| 現 view | 50% | 8,978 | Nested Loop Semi Join | 27.76 |
| 現 view | 20% | 3,598 | Nested Loop Semi Join | 23.42 |
| 置き場関数・追加索引なし | 50% | 6,703 | コピー表を Seq Scan、6,710 loops | 5,615.71 |
| 置き場関数・部分索引あり | 100% | 13,500 | コピー表を Index Only Scan | 30.21 |
| 置き場関数・部分索引あり | 50% | 6,703 | コピー表を Index Only Scan | 31.49 |
| 置き場関数・部分索引あり | 20% | 2,682 | コピー表を Index Only Scan | 21.46 |

関数は各 desired profile に active な encoded asset と指定置き場の active copy がそろうことを確認する。encoded/profile の照合には既存の一意制約索引が使われた。追加索引が無いとコピー表を候補ごとに全走査し、50% で約 5.62 秒かかった。`media_asset_copies (media_asset_id, store) WHERE state = 'active'` の部分索引を使うと同じ条件は 31.49 ms になった。次の実装ではこの関数形と部分索引を使う。
