> [runbook.md](../runbook.md) の一部。索引から辿る。

## 開発時のテスト

`docker compose up -d postgres` で立てた postgres をテストに使える
（`scripts/init-test-db.sh` が `rokuban_test` を作る）。

```sh
export ROKUBAN_TEST_DATABASE_URL="postgres://rokuban:<password>@localhost:5432/rokuban_test?sslmode=disable"
go test ./...
```

**この URL が指す DB は直接使われない。** `testutil.SetupDB` はここから
**パッケージごとの DB 名を導出**（`rokuban_test_api` 等）し、プロセスごとに 1 回だけ
DROP → CREATE → マイグレーションしてから、各テストは TRUNCATE で空にする。

- パッケージが DB を共有しないので `go test ./...` の並行実行で踏み合わない
  （以前は advisory lock で直列化していたが、待たされた側が `lock_timeout` で
  落ちて CI が flaky になっていた）
- マイグレーションはテストごとではなくプロセスごとに 1 回なので速い
- **派生 DB は実行後も残る**（次回の実行が DROP して作り直す）。失敗時の事後調査に使える。
  掃除したいときは:

```sh
psql -h localhost -d postgres -tAc \
  "select datname from pg_database where datname like 'rokuban_test\_%'" |
  xargs -I{} psql -h localhost -d postgres -c 'DROP DATABASE {}'
```

ホストで既に PostgreSQL が 5432 を使っている場合は `.env` の `POSTGRES_PORT` を
変えて、URL 側も合わせる。

実機（mirakc）や実録画データに依存するテストは `test/integration/` に置く。
環境依存性が大きいため **追跡対象外**（`.gitignore`）で、各自のローカルにだけ置く。

```sh
MIRAKC_URL=http://192.168.1.10:40772 \
ROKUBAN_TEST_TS_FILE=/path/to/clean.m2ts \
  go test ./test/integration/ -v
```
