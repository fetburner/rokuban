# managed PostgreSQL での `btree_gist` の確認

チャプター区間の排他制約を作る migration が、migration Job のロールで通るかを確かめる手順。判断（なぜ止まるか、存在確認が証明にならない理由）は [operations/database.md](../operations/database.md) §managed PostgreSQL の btree_gist にある。

<details><summary>根拠</summary>

- 区間の排他制約: `EXCLUDE`
- 存在確認が証明にならない理由: `IF NOT EXISTS`

</details>

## 接続

接続は、provider のコンソールで特定した direct endpoint に migration ロールで行う。接続 URI や password を、コマンド引数・shell history・ログ・issue に貼らない。

特定するのは、本番相当の project、branch、read-write endpoint と、migration Job が使う `db.user` である。pooler を避けて direct endpoint に接続し、password は `psql -W` の prompt で入力する。

```sh
psql -W "host=ENDPOINT_HOST port=5432 dbname=DATABASE_NAME user=MIGRATION_ROLE sslmode=require"
```

## 読み取りだけの確認

migration を流す前に、権限・拡張の導入状態・区間の制約の適用状態を、読み取りだけの SQL で確かめる。

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

migration と同じ文を同じ migration ロールで実行して、拡張を作る権限があるかを確かめる。拡張が未導入なら追加され、導入済みなら何も変更しない。Down は拡張を削除しない。

```sql
CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;
```

導入済みの環境で新規作成権限まで確かめたいときは、拡張を DROP せず、同じ権限の role を用意した使い捨ての managed database で試す。

## 結果の共有

結果には、provider と project / branch / endpoint の識別子、role での成否、migration の適用状態だけを記録する。接続情報や password は含めない。
