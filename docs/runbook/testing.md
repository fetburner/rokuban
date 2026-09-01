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
長い猶予（数十秒〜）を跨ぐ側は実バイナリでしか測れない。

作るものは「ヘッダーだけ即返して**ボディを遅らせる** mirakc」である。ボディを
遅らせるのは、mirakc クライアントの `ResponseHeaderTimeout`（30 秒）が先に
効いてしまうためである。全体の上限は `http.Client.Timeout` の 60 秒なので、
40 秒のジョブはこの形でしか作れない。

遅延モック（`python3 /tmp/mock-mirakc.py` で立てる。`/api/services` のボディだけを
遅らせ、それ以外は空リストを即返す）:

```python
import http.server, time
DELAY = 40
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"[]"
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers(); self.wfile.flush()
        if self.path == "/api/services":
            time.sleep(DELAY)
        self.wfile.write(body)
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer(("127.0.0.1", 40799), H).serve_forever()
```

config は使い捨ての DB とこのモックを指すものを書く。**`storage.media_dir` と
`db.password` を省くと `migrate up` が起動時検査で落ちる**（password は空文字も
拒否されるので、trust 認証でもダミーが要る）。

```sh
DB=rokuban_softstop
createdb -h localhost "$DB"
cat > /tmp/softstop-config.yml <<EOF
server:
  listen: "127.0.0.1:40773"
  allowed_hosts: []
db:
  host: localhost
  port: 5432
  user: $USER
  password: unused
  database: $DB
  sslmode: disable
mirakcs:
  - site: home
    url: http://127.0.0.1:40799
storage:
  media_dir: /tmp/softstop-media
worker:
  periodic_jobs: false
EOF
```

**`bash` で回す**（`zsh` では `time wait` が何も出力しない）:

```bash
DB=rokuban_softstop CFG=/tmp/softstop-config.yml
go build -o /tmp/rokuban ./cmd/rokuban
/tmp/rokuban migrate up --config $CFG
/tmp/rokuban enqueue epg-sync --site home --config $CFG
/tmp/rokuban server --roles worker --sites home --queues=epg \
  --soft-stop-timeout=60s --config $CFG & PID=$!
# **ジョブが実行中になるまで待つ。** 待たずに撃つと claim 前に畳んで終わり、
# 「完走した」も「打ち切られた」も観測できない（空虚な緑になる）
until psql -h localhost -d $DB -tAc \
  "select 1 from river_job where state = 'running'" | grep -q 1; do sleep 1; done
kill -TERM $PID; time wait $PID
# **kind で絞る。** epg_sync は完走時に ruler_pass を投入するので、絞らないと
# その available 行が「打ち切られた」ように見える
psql -h localhost -d $DB -tAc \
  "select state, attempt, errors::text from river_job where kind = 'epg_sync'"
```

実測（2026-08-28。`--soft-stop-timeout` を 60s と 5s で 1 回ずつ）。
**既定（フラグ省略 = 5 秒）でも 1 回測ること。** 既定は「何も設定しなかった人が
SIGKILL されない」ことを根拠に選んである。Docker の既定猶予 10 秒・k8s の
既定猶予 30 秒に収まっている必要がある（実測 5.09 秒）:

| 猶予 | プロセスの終了 | `river_job` |
|---|---|---|
| 60s | SIGTERM の **約 40 秒後**（ジョブの完走を待った）・exit 0 | `completed` |
| 5s | SIGTERM の **5.0 秒後**（猶予切れでエスカレート）・exit 0 | `available` / `attempt=1` / `error="… stop initiated"` |

既定で測るときは `--soft-stop-timeout` を argv から外すだけでよい。

60s の側が「約」なのは、`DELAY` がジョブの要求時刻から測られるのに対し、上の
待ちが 1 秒刻みのポーリングだからである（その遅れぶん手前で終わる。実測 39.0 秒）。
この側は**プロセス側の待ちが固定値ではないこと**も同時に見ている（かつての
`Stop(30 秒)` のままなら 30 秒で先に抜け、ジョブは `running` のまま残る）。

**2 発目の SIGTERM で強制終了できること**も同じ形で確かめられる。上の
`kill -TERM $PID` の直後にもう一度撃つと、drain の途中でもプロセスが落ちる
（実測: 2 発目の直後に exit 143）。1 発目のあとシグナルの登録を外していないと、
猶予のあいだ Ctrl-C も `kill -TERM` も効かなくなる。

**mock では検出できない前提は `internal/mirakc/conformance` が判定する。**
実物の mirakc コンテナに対して回す（Docker が要る）。
コマンドは `go test -tags conformance ./internal/mirakc/conformance/...`。
reconciler の `overrides.contentPath` の既存 schedule への反映も対象である。
これは mirakc が `GET /api/recording/schedules` で `options.contentPath` を
POST した値のまま返すことに依存する。
`/events` の再送・録画中の Range 挙動も同様で、テストの mock ではなくこちらが権威になる。
症状と観測手段は [troubleshooting.md](troubleshooting.md) の該当項目。

## mirakc の版を上げる手順

**「版を上げる」は常に「`main-debian` のより新しい digest に pin を差し替える」を意味する。**
rokuban はどの版の mirakc イメージも自身で出荷しない。
mirakc は運用者が用意するものである。
だから番号付きリリースへ「戻す」判断は無い。
`main` を pin し続けるのが恒常の形である。

1. `internal/mirakc/conformance/helpers_test.go` の `mirakcImage` / `mirakcVersion` を
   新しい版に変える
2. `go test -tags conformance ./internal/mirakc/conformance/...` を回す
3. 落ちた項目があれば、まず **mirakc 側の変更**か**rokuban の前提の誤り**かを切り分ける。
   `docker run --entrypoint mirakc-arib <image> <subcommand> --help` で該当ツールの
   オプション・出力形式が変わっていないか見る、`docker logs` でコンテナ内の
   `mirakc` 自体のエラーを見る、の 2 つがまず効く
4. mirakc 側の変更なら、対応する docs（delegation.md / reconciler.md / ingest.md /
   watcher.md）の記述を実際の挙動に合わせて直す。rokuban 側の前提が誤っていたなら
   実装を直す
5. `internal/mirakc/conformance/` 配下のテストが引用している mirakc の版の文言
   （`mirakc 4.0.0-dev.0 相当` 等）も同じ PR で書き換える

**この digest はいずれ取れなくなる。**
出どころの `main-debian` タグはビルドのたびに上書きされる可動タグだからである。
古い digest は Docker Hub 側でタグ無し（untagged）manifest として prune されうる。
そうなると CI は「テストが落ちた」ではなく「pull できない」で赤くなる。
**これは conformance の回帰ではなく pin の失効である。**
まず `docker pull mirakc/mirakc:main-debian` で最新の digest を取り直す。
digest は `docker inspect --format '{{index .RepoDigests 0}}' mirakc/mirakc:main-debian`
で確認できる。
確認できたら上記の手順で pin を差し替える。
