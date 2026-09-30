# managed PostgreSQL での `btree_gist` の確認

チャプター区間の `EXCLUDE` 制約を作る migration が、migration Job のロールで通るかを確かめる手順。
判断（なぜ止まるか、`IF NOT EXISTS` が証明にならない理由）は [operations/database.md](../operations/database.md) §managed PostgreSQL の `btree_gist`。

## 接続

provider の公式コンソールで、本番相当の project、branch、read-write endpoint と migration Job が使う `db.user` を特定する。
pooler を避けて direct endpoint に接続し、password は `psql -W` の prompt で入力する。
接続 URI や password をコマンド引数、shell history、ログ、issue に貼らない。

```sh
psql -W "host=ENDPOINT_HOST port=5432 dbname=DATABASE_NAME user=MIGRATION_ROLE sslmode=require"
```

## 読み取りだけの確認

```sql
SELECT current_user,
       current_database(),
       has_database_privilege(current_user, current_database(), 'CREATE') AS can_create;

SELECT e.extversion, n.nspname AS schema_name
FROM pg_extension AS e
JOIN pg_namespace AS n ON n.oid = e.extnamespace
WHERE e.extname = 'btree_gist';

-- true なら区間の EXCLUDE 制約を作る migration は適用済み
SELECT EXISTS (
  SELECT 1 FROM pg_constraint
  WHERE conrelid = to_regclass('public.recording_chapter_spans') AND contype = 'x'
) AS chapter_migration_applied;
```

## 作成権限の確認

同じ migration role で migration と同じ文を実行する。拡張が未導入なら追加され、導入済みなら何も変更しない。
Down は拡張を削除しない。

```sql
CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;
```

導入済みの環境で新規作成権限まで確かめたいときは、拡張を DROP せず、同じ権限の role を用意した使い捨ての managed database で試す。

## 結果の共有

provider と project / branch / endpoint の識別子、role での成否、migration の適用状態だけを記録する。接続情報や password は含めない。
