> [docs/schema.md](../schema.md)（索引）の分割本文。節番号は分割前のまま（§10）。

## 10. 後続マイグレーション

**マイグレーションの一覧の権威は、マイグレーションのファイル自体である。** 各表の現行仕様は該当する分割ファイル（[schema.md](../schema.md) の表で引く）にある。ここに完了した一覧は置かない。

関数と view の現行定義は、マイグレーションを grep せず `internal/db/schema.sql` で読む。関数は `CREATE OR REPLACE` で複数のマイグレーションに散っており、grep すると古い定義に当たる。このファイルは全マイグレーションを適用した後の `pg_dump` 出力で、`scripts/schemadump.sh` が生成する。手で直さない。

<details><summary>根拠</summary>

- マイグレーションのファイル: `internal/db/migrations/`

</details>
