# シャドー運用 runbook（M1）

既存の mirakc（EPGStation と共有）に Rokuban をぶら下げ、**実放送を 1 本録って
再生できるところまで**を確認する手順。

M1 のスコープは「録れる」まで。ルール録画・エンコード・保持ポリシーは入っていない
（[移行計画](https://github.com/fetburner/rokuban/issues/6) の M2 以降）。

## 前提

- mirakc が動いていて `GET /api/version` が返る
- mirakc の `recording.basedir` と `recording.records-dir` が設定されている
  （どちらも未設定だと `/api/recording/*` が 404 になる）
- mirakc のイメージに **`cat` と `dd` が含まれている**

最後の項目は見落としやすい。`FROM scratch` + magicpak のように必要なバイナリだけを
含むイメージだと、`GET /api/recording/records/{id}/stream` が **500** を返して
ingest が完走しない。HEAD はファイルのメタデータだけで応答できるので**成功する**ため、
「HEAD は通るのに GET が 500」という切り分けにくい形で出る。

## 起動

```sh
cp .env.example .env
$EDITOR .env          # MIRAKC_URL と POSTGRES_PASSWORD は必須
docker compose up -d
docker compose logs -f rokuban
```

`http://localhost:40773` で UI が開く。

正常なら起動から 30 秒ほどで EPG の全量同期が 1 回走る。

```
INFO epg sync complete services_projected=19 programs_fetched=7139 programs_projected=2680 ...
```

`programs_projected` が 0 のままなら mirakc 側の EPG がまだ空か、
`update-schedules` ジョブが一度も走っていない（既定は 1 日 2 回）。

## 実放送を 1 本録る

### 1. 番組を選んで予約する

UI の「番組」タブから予約ボタンを押す。API で直接やるなら:

```sh
# 現在放送中で残りがある番組を探す（mirakc から直接）
curl -s "$MIRAKC_URL/api/programs" | jq -r --argjson now "$(date +%s000)" '
  .[] | select(.name != null)
      | select(.startAt <= $now and $now < .startAt + .duration)
      | select(.startAt + .duration - $now > 300000)
      | "\(.id)\t\(.serviceId)\t\(.name)"' | head

# 予約する（startAt は RFC3339、durationMs はミリ秒）
curl -s -X POST http://localhost:40773/api/reservations \
  -H 'Content-Type: application/json' \
  -d '{"programId":319215325618427,"title":"テスト番組",
       "startAt":"2026-07-25T16:00:00+09:00","durationMs":1800000}'
```

### 2. reconciler が mirakc に schedule を作るのを待つ

reconciler は 30 秒間隔。ログに出る。

```
INFO reconciler: created schedule reservation_id=1 program_id=319215325618427 state=scheduled content_path=20260725/160000_..._53256.m2ts
```

mirakc 側にも tag 付きで入っていることを確認する。

```sh
curl -s "$MIRAKC_URL/api/recording/schedules" | jq '.[] | {program: .program.id, state, tags}'
# → tags に "rokuban:reservation=1" が入っている
```

### 3. 録画 → ingest を待つ

番組が終わると mirakc が record を `finished` にし、watcher がそれを観測して
ingest ジョブを投入する。

```
INFO ingest: transfer complete bytes=687486296 drops=0 errors=0 scrambled=0
```

`ingest: transfer complete` が出ない場合は「詰まったとき」を参照。

### 4. 結果を確認する

```sh
# 録画一覧（ドロップ統計込み）
curl -s http://localhost:40773/api/recordings | jq '.[] | {title, status, sizeBytes, dropSummary}'

# PID 別の内訳
curl -s http://localhost:40773/api/recordings/1/drop-stats | jq

# エッジの record は ingest コミット後に消えているはず
curl -s "$MIRAKC_URL/api/recording/records" | jq 'length'   # → 0
```

### 5. VLC で再生する

```sh
vlc http://localhost:40773/api/recordings/1/file
```

シークして飛べることを確認する。Range 配信なので `http.ServeContent` が
206 と `Content-Range` を返す。

## 出口基準チェックリスト

M1 の出口基準は「実放送を手動予約で 1 本録画し、ingest 済みファイルを VLC で
再生でき、ドロップ統計が UI で見える」。

- [ ] `docker compose up -d` で起動し `/healthz` が 200 を返す
- [ ] UI の番組リストに実際の番組が並ぶ（日付・サービスで絞り込める）
- [ ] UI から予約でき、予約タブに現れる
- [ ] mirakc の `/api/recording/schedules` に `rokuban:reservation=<id>` tag 付きで入る
- [ ] 番組終了後、`ingest: transfer complete` がログに出る
- [ ] `media_assets` 行ができ、ファイルの実サイズが `size_bytes` と一致する
- [ ] mirakc 側のエッジ record が消えている（コミット後に削除）
- [ ] UI の録画一覧に状態とドロップ統計が出る（行を開くと PID 別の内訳）
- [ ] **VLC でシークしながら再生できる**
- [ ] UI から予約を取消すと、mirakc 側の schedule も消える
- [ ] `/metrics` が scrape でき、`rokuban_ingest_bytes_total` などに値が入る

確認用の SQL:

```sh
docker compose exec postgres psql -U rokuban -d rokuban -c \
  "SELECT r.title, r.status, a.rel_path, a.size_bytes,
          (SELECT sum(drops) FROM drop_stats d WHERE d.media_asset_id = a.id) AS drops
     FROM recordings r LEFT JOIN media_assets a ON a.recording_id = r.id"
```

## EPGStation との並走

同じ mirakc を共有してよい。**チューナーの調停は mirakc が行う**ので、Rokuban が
EPGStation の録画を奪うことはない。ただし物理的な制約は残る。

### 同一チャンネルなら競合しない

mirakc は同じ物理チャンネルのストリームを複数の購読者で共有する。EPGStation が
GR/27 をライブ視聴していて Rokuban が GR/27 を録画する場合、チューナーは 1 本で足りる。

### 別チャンネルはチューナー数で競合する

チューナーが 1 本しかない環境では、EPGStation の録画と Rokuban の録画が別チャンネルに
なった時点でどちらかが録れない。負けた側は `recording.failed` の
`need-rescheduling` になる（`rokuban_recordings_failed_total{reason="need-rescheduling"}`）。

調停は `priority` で行う。Rokuban の既定は 10。EPGStation 側の優先度と揃えるか、
**並走中はチャンネルが重ならない番組で試す**のが安全。

### EPG 収集もチューナーを使う

mirakc の `update-schedules` ジョブ（既定 08:21 / 20:21、timeout 10 分）は
物理チャンネルごとにチューニングして EPG を集める。この時間帯に録画を入れると
競合しやすい。

逆にこのジョブが特定チャンネルで失敗すると、そのチャンネルの番組が
`/api/programs` から返らなくなる。Rokuban は**番組を返さなかったチャンネルの
プロジェクションを消さない**ようにしてあるが、
`rokuban_epg_channels_without_programs` が 0 以外で続くなら mirakc 側の
収集失敗を疑う。

### 二重録画に注意

Rokuban と EPGStation の両方に同じ番組の予約が入っていると、**同じ番組を 2 回録る**
（tag が違うので互いに相手の schedule を消さない。reconciler は
`rokuban:reservation=` tag のない schedule を触らない）。ディスクとチューナーを
二重に消費するので、シャドー運用中は片方だけに予約を入れる。

## 詰まったとき

### `ingest: transfer complete` が出ない

```sh
docker compose exec postgres psql -U rokuban -d rokuban -c \
  "SELECT id, kind, state, attempt, errors FROM river_job WHERE kind='ingest' ORDER BY id DESC LIMIT 3"
```

- `errors` に **500** — mirakc のイメージに `cat` / `dd` が無い（前述）。
  HEAD だけ試すと成功するので騙されやすい
- `errors` に **`context deadline exceeded`** — River の総時間タイムアウト。
  ingest は無効化してあるので、出るなら設定が壊れている
- `state=retryable` のまま進まない — mirakc への到達性か、
  `media_dir` の書き込み権限を確認する

### EPG 同期が一度しか走らない

`river_job` の `epg_sync` に完了済みの行が残っていて `unique_key` を占有している。
`UniqueOpts.ByState` の設定を変更した場合に起きる（[運用](operations.md) の
「River のジョブ一意性の注意」）。

```sh
docker compose exec postgres psql -U rokuban -d rokuban -c \
  "DELETE FROM river_job WHERE kind='epg_sync' AND state='completed'"
```

`rokuban_epg_sync_last_success_timestamp_seconds` が更新され続けているかで監視できる。

### 番組リストが空

```sh
curl -s "$MIRAKC_URL/api/programs" | jq 'length'
```

0 なら mirakc 側の問題。mirakc は再起動直後に EPG キャッシュを読み込み終えて
おらず空を返すことがある。Rokuban は空レスポンスでプロジェクションを消さない
ようにしてあるので、mirakc の EPG が復帰すれば次の同期で戻る。

### 録画が開始直後に失敗する

`recording.failed` の `need-rescheduling` が録画開始の数秒後に出る場合、
チューナーの競合が濃厚。

```sh
curl -s "$MIRAKC_URL/api/tuners" | jq '.[] | {name, isAvailable, users}'
```

EPGStation のライブ視聴・EPG 収集・別チャンネルの録画が掴んでいないか確認する。

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
