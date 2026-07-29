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
`UniqueOpts.ByState` の設定を変更した場合に起きる（[運用](../operations.md) の
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

### ルールを作ったのに予約ができない

上から順に切る。

```sh
# 1. 条件はマッチしているか（ruler と同じコンパイラ）
curl -s -X POST http://localhost:40773/api/programs/search \
  -H 'Content-Type: application/json' -d '<ルールと同じ条件>' | jq length

# 2. ruler パスは走ったか
docker compose logs rokuban | grep 'ruler: pass complete' | tail -3

# 3. ジョブが詰まっていないか
docker compose exec postgres psql -U rokuban -d rokuban -c \
  "SELECT id, kind, state, attempt, errors FROM river_job
     WHERE kind IN ('ruler_pass','reconcile_pass') ORDER BY id DESC LIMIT 5"
```

- 1 が 0 件 — 条件そのものの問題（射影に番組が無いか、条件が厳しすぎる）。検索は
  `enabled` を見ないので、ここが非ゼロでもルールが無効なら予約はできない
- 1 は非ゼロだが `desired` が 0 — ルールが `enabled: false`、または `sites` /
  `periodStartAt` / `periodEndAt` で絞られている
- ログに何も出ない — `worker.periodic_jobs` が false（`rokuban enqueue ruler-pass`）か、
  worker ロールが起動していない
- **サーキットブレーカーは疑わなくてよい。** 止まるのは削除だけで、作成は続く

### 予約はあるのに mirakc に schedule が作られない

```sh
curl -s http://localhost:40773/api/reservations/12 | jq '{state, skip, dedupMatchRecordingId}'
```

- `skip: true` — 除外か重複排除。手順 6 で理由を読む
- `state: "orphaned"` — 同期対象外。番組終了後なら「既知の未解決事項」を先に読む
- どちらでもない — `rokuban_reconcile_pending_diff{action="create"}` が減らないなら
  mirakc が作成を拒否している。`reconciler: creating schedule` の ERROR を見る

### `/api/capacity/overages` が常に空

`tuner_sync` の射影が空だと**何も主張しない**設計なので、超過が無いのか判定が
無効なのかを区別できない。手順 10 の「射影が動いているかを先に確認する」を見る。

### 録画が開始直後に失敗する

`recording.failed` の `need-rescheduling` が録画開始の数秒後に出る場合、
チューナーの競合が濃厚。

```sh
curl -s "$MIRAKC_URL/api/tuners" | jq '.[] | {name, isAvailable, users}'
```

EPGStation のライブ視聴・EPG 収集・別チャンネルの録画が掴んでいないか確認する。

