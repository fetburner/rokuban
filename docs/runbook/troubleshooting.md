> [runbook.md](../runbook.md) の一部。索引から辿る。

## 詰まったとき

### `media_dir` に書けない（`permission denied`）

**録画が 1 件も無くても、起動直後に `river_job` にエラーが出る**のがこの症状の
最初の形。`catalog_export` は起動時に 1 回走る定期ジョブなので、`media_dir` に
書けない構成ではこれが最初に落ちる（ingest・サムネイル生成・エンコードも同じ
理由で落ちるが、そちらは録画ができるまで走らない）。`/healthz` は通り続けるので、
ジョブ側を見ないと気付けない。

```sh
docker compose exec postgres psql -U rokuban -d rokuban -c \
  "SELECT id, kind, state, attempt, errors FROM river_job
     WHERE state IN ('retryable', 'discarded') ORDER BY id DESC LIMIT 5"
```

`catalog_export` が `state=retryable` で次のエラーを持っていればこれ。

```text
creating catalog dir: mkdir /mnt/media/catalog: permission denied
```

実測: 所有権を焼く前のイメージで `docker-compose.test.yml` を起動したときの
`river_job` 行。このとき `/healthz` は `{"status":"ok"}` を返し続けた。

イメージは `/mnt/media` を実行ユーザー所有で焼いてある（uid/gid `65534` =
`nobody:nogroup`）。**空の** named volume なら手当ては要らない。Docker の
copy-up がマウント点の所有権を volume 側にコピーするため。所有権を焼く前の
イメージで**起動しただけ**の volume も、書き込みが一度も無ければ空である。
イメージを上げ直すだけで直る（実測: 空 volume を旧イメージでマウントすると
`/mnt/media` は `root:root`。そのあと修正後イメージでマウントすると
`nobody:nogroup` になり `touch` が通る）。

**手当てが要るのは volume に既に中身がある場合**。典型は、この不具合を踏んで
compose 側で `user: "0:0"` を当てて回避した volume。root のまま録画やカタログを
書いてしまっている。copy-up は空の volume にしか効かないので、イメージを
上げ直しても所有権は変わらない。

`chown` は **`-R` でかける**。`/mnt/media` だけを chown しても配下の
`catalog/` などが root 所有のまま残る。`os.MkdirAll` は既存ディレクトリに
`nil` を返すので、ファイル作成で同じ `permission denied` になる。

実測: root で `catalog/` を書いた volume では、非再帰 `chown` 後も
`touch /mnt/media/catalog/probe` が `Permission denied` になった。
`chown -R` 後は成功した。

```sh
docker compose down   # user: "0:0" を当てて回避していたら、この機に消す
docker run --rm --user 0 --entrypoint chown \
  -v <project>_media:/mnt/media <image> -R 65534:65534 /mnt/media
docker compose up -d
```

bind mount には copy-up が無いので、ホスト側を先に用意する
（[setup.md](setup.md) の「録画の保存先」）。

### `ingest: transfer complete` が出ない

```sh
docker compose exec postgres psql -U rokuban -d rokuban -c \
  "SELECT id, kind, state, attempt, errors FROM river_job WHERE kind='ingest' ORDER BY id DESC LIMIT 3"
```

- `errors` に **500** — mirakc のイメージに `cat` / `dd` が無い（[setup.md](setup.md) の前提を参照）。
  HEAD だけ試すと成功するので騙されやすい
- `errors` に **`context deadline exceeded`** — River の総時間タイムアウト。
  ingest は無効化してあるので、出るなら設定が壊れている
- `state=retryable` のまま進まない — mirakc への到達性か、`media_dir` の
  書き込み権限（上記「`media_dir` に書けない」）を確認する

**「転送しているが遅いだけ」と「止まっている」は UI で見分けられる**。録画一覧・
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

### エンコードが失敗している（理由を知りたい）

一覧・詳細のバッジは「`h264: エンコード失敗`」までしか言わない。**失敗の理由と
最後に試した時刻は API に出さない**（プロファイル名以上の内部情報を配らない）ので、
運用者は DB を直接見る:

```sh
docker compose exec postgres psql -U rokuban -d rokuban -c \
  "SELECT recording_id, profile, state, error, attempted_at FROM recording_encode_attempts ORDER BY attempted_at DESC LIMIT 20"
```

- `error` は ffmpeg の失敗メッセージを含む先頭 2000 バイト（`EncodeWorker` が
  切り詰める。全文は worker のログ）
- `state='running'` のまま `attempted_at` が古い —— worker が実行中に落ちた行。
  再投入されれば上書きされる。**この行を掃除する回収器は無い**（設定から消えた
  プロファイルで River のリトライも尽きていると残り続ける）
- `error` に `unknown encode profile` —— 設定から消えたプロファイル。
  `config.encode.profiles` に戻すか、その録画の `recording_encode_policy` から
  外す（[schema/recordings.md](../schema/recordings.md) の
  `recording_encode_attempts` 節）

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

**実測（このリリースノートを書くために実バイナリで確認済み。手順は下記）**。
site 単位のキュー名を修飾する変更（`ingest` → `ingest_<site>` 等。
[運用](../operations.md) の「`worker.queues` に書く名前は論理名」参照）がある。
`delete_reconcile` / `catalog_export` の `default` → `cleanup` への移設もある。
どちらも、デプロイ前に投入済みだった旧キューの行を新キューへ自動移行しない。

**現在のコード（`UniqueOpts.ByQueue: true`、`internal/worker/worker.go` の
`uniqueByQueue`）では、旧キューの残骸があっても新しいジョブの投入自体は
ブロックされない**。一意キーがキュー名を含むため、旧キュー（キューを含まない
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

**それでも掃除は推奨する**。旧キューの残骸（上の `id=1`）は残さない。
どの worker も購読しないキューに永久に残るためだ。
`state='available'` のままになり、滞留メトリクスを汚し続ける。
デプロイ後に 1 回だけ次を実行する。
`internal/jobs/queue.go` の `pendingJobStates` と同じ 5 状態を対象にする。
`available`/`scheduled`/`retryable` だけでは不十分である。
`pending`/`running` の残骸も取りこぼさない。

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
`queues: [cleanup]`）を運用する構成を考える**。**この構成では `default` キューの残骸
（旧 `delete_reconcile` / `catalog_export`）はどの worker も購読しないままに
なる**。`queue = 'default'` の分岐を上の DELETE から省略しないこと。
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
- **サーキットブレーカーは疑わなくてよい**。止まるのは削除だけで、作成は続く

### 予約はあるのに mirakc に schedule が作られない

```sh
curl -s http://localhost:40773/api/reservations | \
  jq '.[] | select(.title == "<番組名>") | {state, skip, dedupMatchRecordingId}'
```

- `skip: true` — 除外か重複排除。`dedupMatchRecordingId` / `dedupSimilarity` が
  入っていれば重複排除（相手の録画 ID と類似度まで説明できる）。無ければ
  ユーザーの除外（`program_intents.action = 'skip'`）
- `state: "orphaned"` — 同期対象外（放送済み番組の予約は GC まで残る）
- どちらでもない — `rokuban_reconcile_pending_diff{action="create"}` が減らないなら
  mirakc が作成を拒否している。`reconciler: creating schedule` の ERROR を見る

### `overrides.contentPath` を明示指定したのに既存 schedule のパスが変わらない・毎パス再作成される

`overrides.contentPath` の反映（[reconciler.md](../recording/reconciler.md) §3.2「予約オプションの差分反映」）は、mirakc の挙動に依存する。mirakc が `GET /api/recording/schedules` で `options.contentPath` を POST した値のまま返す（正規化しない）ことが前提である。この依存は `internal/mirakc/conformance` の `TestConformance/ContentPathRoundTrip` が mirakc 4.0.0-dev.0 相当に対して判定している。本番の mirakc がこの pin と異なる版なら、この前提はその版で改めて確認する（下記の症状はその手がかり）。

```sh
curl -s http://localhost:9090/api/v1/query --data-urlencode \
  'query=rokuban_reconcile_pending_diff{action="update"}' | jq
```

- `action="update"` がゼロに戻らず、`reconciler: recreated schedule` の Info ログで同じ `reservation_id` に対して `reason=content_path` が反復していないか見る。反復しているなら mirakc が contentPath をそのまま返していない。これは比較が収束せず毎パス DELETE→POST になっていることを意味する。実 mirakc に対して `GET /api/recording/schedules` の応答を直接見て、POST した `contentPath` と一致するか確認する
- `action="update_deferred"` 側に出ているだけなら、`scheduled` 以外の状態（録画中等）で allowlist に見送られているだけで異常ではない

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
