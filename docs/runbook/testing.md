> [runbook.md](../runbook.md) の一部。索引から辿る。

## 開発時のテスト

`docker compose up -d postgres` で立てた postgres をテストに使える
（`scripts/init-test-db.sh` が `rokuban_test` を作る）。

```sh
export ROKUBAN_TEST_DATABASE_URL="postgres://rokuban:<password>@localhost:5432/rokuban_test?sslmode=disable"
go test ./...
```

**この URL が指す DB は直接使われない**。`testutil.SetupDB` はここから
**パッケージごとの DB 名を導出**する（`rokuban_test_api` 等）。プロセスごとに 1 回だけ
DROP → CREATE → マイグレーションしてから、各テストは TRUNCATE で空にする。

- パッケージが DB を共有しないので `go test ./...` の並行実行で踏み合わない
  （以前は advisory lock で直列化していた）。待たされた側が `lock_timeout` で
  落ちて CI が flaky になっていた
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

### SIGTERM の drain を実バイナリで確かめる

`--soft-stop-timeout` を触ったときはこれを回す。**テストでは猶予より長く走る
ジョブを実際に走らせられない**（テストの所要が猶予そのものになるため、
`TestServerCmd_SigtermDrainsRunningJob` は猶予を数秒に絞って両方向を見ている）。
既定の 30 秒を跨ぐ側は実バイナリでしか測れない。

作るものは「ヘッダーだけ即返して**ボディを遅らせる** mirakc」である。ボディを
遅らせるのは、mirakc クライアントの `ResponseHeaderTimeout`（30 秒）が先に
効いてしまうためである。全体の上限は `http.Client.Timeout` の 60 秒なので、
40 秒のジョブはこの形でしか作れない。

```sh
go build -o /tmp/rokuban ./cmd/rokuban
# /api/services のボディを 40 秒遅らせる HTTP サーバーを 127.0.0.1:40799 に立てておく
/tmp/rokuban migrate up --config /tmp/softstop-config.yml     # 使い捨ての DB を指す config
/tmp/rokuban enqueue epg-sync --site home --config /tmp/softstop-config.yml
/tmp/rokuban server --roles worker --sites home --queues=epg \
  --soft-stop-timeout=60s --config /tmp/softstop-config.yml &
# mirakc に要求が届いてから（= ジョブが実行中になってから）SIGTERM を撃つ
kill -TERM $!
```

実測（2026-08-28。`--soft-stop-timeout` を 60s と 5s で 1 回ずつ）:

| 猶予 | プロセスの終了 | `river_job` |
|---|---|---|
| 60s | SIGTERM の **40 秒後**（ジョブの完走を待った）・exit 0 | `completed` |
| 5s | SIGTERM の **5 秒後**（猶予切れでエスカレート）・exit 0 | `available` / `attempt=1` / `error="… stop initiated"` |

60s の側は**プロセス側の待ちが固定値ではないこと**も同時に見ている（かつての
`Stop(30 秒)` のままなら 30 秒で先に抜け、ジョブは `running` のまま残る）。

**mock では検出できない前提が未検証のまま残ることがある**。例えば reconciler の
`overrides.contentPath` の既存 schedule への反映を考える。これは mirakc が `GET /api/recording/schedules`
で `options.contentPath` を POST した値のまま返すことに依存する。テストの mock は
入力をそのまま返すのでこの前提が破れていても検出できない。この種の依存を触ったときは
`GET /api/recording/schedules` の実応答を実 mirakc に対して直接確認する
（症状と観測手段は [troubleshooting.md](troubleshooting.md) の該当項目）。
