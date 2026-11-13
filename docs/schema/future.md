> [docs/schema.md](../schema.md)（索引）の分割本文。節番号は分割前のまま（§10）。

## 10. 後続マイグレーション

**マイグレーションの一覧の権威は、マイグレーションのファイル自体である。** 各表の現行仕様は該当する分割ファイル（[schema.md](../schema.md) の表で引く）にある。ここに完了した一覧は置かない。

<details><summary>根拠</summary>

- マイグレーションのファイル: `internal/db/migrations/`

</details>

関数と view の現行定義は、マイグレーションを grep せず `internal/db/schema.sql` で読む。どちらも `CREATE OR REPLACE` で複数のマイグレーションに散っており、grep すると古い定義に当たる。`schema.sql` は全マイグレーションを適用した後の `pg_dump` 出力で、`scripts/schemadump.sh` が生成する。マイグレーションを足したら再生成してコミットする（しないと CI の `generated-diff` が落ちる）。形の権威はマイグレーションのままである。`schema.sql` は読むための生成物なので、手で直さない。

<details><summary>根拠</summary>

- 生成スクリプト: `scripts/schemadump.sh`
- 生成物: `internal/db/schema.sql`

</details>
