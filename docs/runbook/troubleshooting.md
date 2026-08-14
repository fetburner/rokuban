> [runbook.md](../runbook.md) の一部。索引から辿る。

## 詰まったとき

### `ingest: transfer complete` が出ない

```sh
docker compose exec postgres psql -U rokuban -d rokuban -c \
  "SELECT id, kind, state, attempt, errors FROM river_job WHERE kind='ingest' ORDER BY id DESC LIMIT 3"
```

- `errors` に **500** — mirakc のイメージに `cat` / `dd` が無い（[setup.md](setup.md) の前提を参照）。
  HEAD だけ試すと成功するので騙されやすい
- `errors` に **`context deadline exceeded`** — River の総時間タイムアウト。
  ingest は無効化してあるので、出るなら設定が壊れている
- `state=retryable` のまま進まない — mirakc への到達性か、
  `media_dir` の書き込み権限を確認する

**「転送しているが遅いだけ」と「止まっている」は UI で見分けられる。** 録画一覧・
録画詳細に取り込み状態が出る（「取り込み中 42%」/「取り込み中 1.2 GB（停滞）」/
「取り込み待ち」。[frontend/recordings.md](../frontend/recordings.md)）。DB を直接
見るなら次:

```sh
docker compose exec postgres psql -U rokuban -d rokuban -c \
  "SELECT recording_id, written_bytes, expected_bytes, observed_at FROM recording_ingest_progress"
```

`observed_at` が現在時刻から離れていくなら転送は止まっている（進捗の書き直しは
**最短** 2 秒間隔 --- バイトが流れたときにしか書かないので、極端に遅い回線では
これより粗くなる）。`written_bytes` が 0 に戻るのは異常ではない —— ジョブ再試行は部分
ファイルを truncate してゼロから作り直す（[recording/ingest.md](../recording/ingest.md) §5.3）

### EPG 同期が一度しか走らない

`river_job` の `epg_sync` に完了済みの行が残っていて `unique_key` を占有している。
`UniqueOpts.ByState` の設定を変更した場合に起きる（[データ層](../data.md) の
「River のジョブ一意性の注意」）。

```sh
docker compose exec postgres psql -U rokuban -d rokuban -c \
  "DELETE FROM river_job WHERE kind='epg_sync' AND state='completed'"
```

`rokuban_epg_sync_last_success_timestamp_seconds` が更新され続けているかで監視できる。

### デプロイ直後、旧キューの残骸が `river_job` に残っている

**実測（このリリースノートを書くために実バイナリで確認済み。手順は下記）**:
site 単位のキュー名を修飾する変更（`ingest` → `ingest_<site>` 等。
[運用](../operations.md) の「`worker.queues` に書く名前は論理名」参照）と
`delete_reconcile` / `catalog_export` の `default` → `cleanup` への移設は、
デプロイ前に投入済みだった旧キューの行を新キューへ自動移行しない。

**現在のコード（`UniqueOpts.ByQueue: true`、`internal/worker/worker.go` の
`uniqueByQueue`）では、旧キューの残骸があっても新しいジョブの投入自体は
ブロックされない** --- 一意キーがキュー名を含むため、旧キュー（キューを含まない
鍵）と新キュー（キューを含む鍵）は別のハッシュになり衝突しない。実際に
確認した挙動:

```console
$ rokuban enqueue reconcile-pass --site tokyo   # 旧バイナリで投入（旧キュー "reconciler"）
inserted job "reconcile-pass" (id=1) for site "tokyo"

$ rokuban enqueue reconcile-pass --site tokyo   # 新バイナリで同じ args を再投入
inserted job "reconcile-pass" (id=3) for site "tokyo"   # 別行として作られる。スキップされない
```

```sql
SELECT id, kind, queue, state FROM river_job ORDER BY id;
--  id |      kind      |      queue       |   state
-- ----+----------------+------------------+-----------
--   1 | reconcile_pass | reconciler       | available   ← 旧キューの残骸（誰も引かない）
--   3 | reconcile_pass | reconciler_tokyo | available   ← 新キュー。worker が正しく引く
```

**それでも掃除は推奨する。** 旧キューの残骸（上の `id=1`）はどの worker も
購読しないキューに永久に残り、`state='available'` のまま滞留メトリクス
（River のキュー長ダッシュボード等）を汚し続ける。デプロイ後に 1 回だけ
次を実行する（`pendingJobStates` と同じ 5 状態すべてを対象にする。
`available`/`scheduled`/`retryable` だけでは `pending`/`running` の残骸を
取りこぼす）:

```sh
docker compose exec postgres psql -U rokuban -d rokuban -c \
  "DELETE FROM river_job
     WHERE state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
       AND (
         queue IN ('ingest', 'epg', 'reconciler', 'watcher')
         OR (queue = 'default' AND kind IN ('delete_reconcile', 'catalog_export'))
       )"
```

**`worker.queues` に `default` を含めない中央 cleanup worker（例:
`queues: [cleanup]`）を運用する構成では、`default` キューの残骸
（旧 `delete_reconcile` / `catalog_export`）はどの worker も購読しないままに
なる。** `queue = 'default'` の分岐を上の DELETE から省略しないこと ---
「`default` は引き続き購読対象なので掃除不要」という判断は、`default` を
含む `worker.queues` を書いている構成にしか当てはまらない。

上のコマンドを実際に実行して `id=1` の行が消え、`id=3` だけが残ることを
確認済み（このリリースノートの検証手順）。

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
curl -s -X POST http://localhost:40773/api/sites/default/programs/search \
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

- `skip: true` — 除外か重複排除。`dedupMatchRecordingId` / `dedupSimilarity` が
  入っていれば重複排除（相手の録画 ID と類似度まで説明できる）、無ければ
  ユーザーの除外（`program_intents.action = 'skip'`）
- `state: "orphaned"` — 同期対象外（放送済み番組の予約は GC まで残る）
- どちらでもない — `rokuban_reconcile_pending_diff{action="create"}` が減らないなら
  mirakc が作成を拒否している。`reconciler: creating schedule` の ERROR を見る

### `/api/capacity/overages` が常に空

`tuner_sync` の射影が空だと**何も主張しない**設計なので、超過が無いのか判定が
無効なのかを区別できない。空配列を見たら、射影が動いているかを先に確認する。

```sh
curl -s http://localhost:40773/metrics |
  grep -E 'rokuban_tuners_projected|rokuban_tuner_sync_last_success|rokuban_capacity_overages'
# rokuban_tuners_projected{site="default"} 4
# rokuban_tuner_sync_last_success_timestamp_seconds{site="default"} 1.7695e+09
# rokuban_capacity_overages{site="default"} 0
```

```sh
docker compose exec postgres psql -U rokuban -d rokuban -c \
  "SELECT tuner_index, name, types, is_available, is_fault, observed_at FROM tuner_sync ORDER BY tuner_index"
```

- 行が無い / `rokuban_tuners_projected` が 0 — 射影が空。判定は無効
- `tuner sync: mirakc returned no projectable tuners, skipping sweep` — mirakc が
  空を返したのでスイープを見送った（既存の射影は消さない）
- `tuner sync: skipping tuner with unknown channel type` — 未知の種別を持つ
  チューナーを**丸ごと捨てた**。`cap(A)` が 1 本少なくなるので警告が過剰に出る側に
  ずれる（見逃す側にはずれない）
- 行はあるが `is_available = false` / `is_fault = true` が全台 — この場合は**判定する**
  （射影された事実であって我々の無知ではない）

### ドロップ統計で音声 PID が `other` に見える

**`pidType` が `other` の音声 PID**（LATM AAC / `stream_type = 0x11`）は
「誤読しやすい正常」で、ドロップ統計そのものは正しい。`gots` の
`IsAudioContent()` の値域に従っているだけで、自前の `stream_type` 表は
作らない方針。ISDB の GR/BS/CS は 0x0F（MPEG-2 AAC）が主なので実害は
4K/8K に限られる見込みだが、観測したら「分類の限界」であってドロップではない。
1 行で足せるので観測したら報告する。

### 録画が開始直後に失敗する

`recording.failed` の `need-rescheduling` が録画開始の数秒後に出る場合、
チューナーの競合が濃厚。

```sh
curl -s "$MIRAKC_URL/api/tuners" | jq '.[] | {name, isAvailable, users}'
```

EPGStation のライブ視聴・EPG 収集・別チャンネルの録画が掴んでいないか確認する。


## 経緯と失敗事例

- 「デプロイ直後、旧キューの残骸が `river_job` に残っている」の挙動と掃除コマンドは、
  issue #185（M4-13）の検証で実バイナリを使って実測した
