# シャドー運用 runbook

既存の mirakc（EPGStation と共有）に Rokuban をぶら下げ、実放送で確認する手順。

| 段 | 確認すること |
|---|---|
| [M1](#m1-実放送を-1-本録る) | 実放送を 1 本、**手動予約で録って再生できる** |
| [M2](#m2-ルールで録る) | **ルールで録れる**。除外・上書きが生き残り、「なぜ録られていないか」を運用者が読める |

エンコードと保持ポリシーは入っていない
（[移行計画](https://github.com/fetburner/rokuban/issues/6) の M3 以降）。

M2 の機能のうち **UI があるのは予約側だけ**である。ルート
（`web/src/routes.tsx`）は番組 / 予約 / 予約詳細 / 録画の 4 つで、**ルールの編集画面と
検索画面は無い**（[issue #24](https://github.com/fetburner/rokuban/issues/24) の M2-11
以降）。ルールは API で作る。

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

同じ頃にチューナーの射影（M2-10）も 1 回走る。容量超過の判定はこの射影だけを
読むので、これが出ていない間は**超過が一切報告されない**（後述の「沈黙は保証ではない」）。

```
INFO tuner sync complete site=default tuners_fetched=4 tuners_projected=4 stale=0 capacity_overages=0
```

## 定期ジョブの周期とヒント

M2 の待ち時間はほぼこの表で決まる。**定期パスが真実**で、ヒントは投入を早めるだけ
（落としても次の定期パスが拾う。不変条件 5）。

| ジョブ | 既定周期 | 設定キー | 前倒しするヒント |
|---|---|---|---|
| `epg_sync` | 10 分 | `epg.sync_interval` | なし |
| `tuner_sync` | 10 分 | **なし**（コード固定。運用者が触る理由がないため） | なし |
| `ruler_pass` | 10 分 | なし | ルールの作成 / 更新 / 削除（api が**同一トランザクションで**投入）・`epg_sync` の完了 |
| `reconcile_pass` | 30 秒 | なし | 予約の作成 / 取消（同一トランザクション）・`ruler_pass` の完了 |
| `record_sweep` | 5 分 | なし | なし（SSE 再接続を契機にする案は M2-18 で見送り） |

- 5 つとも `RunOnStart` なので**プロセス起動直後に 1 回走る**
- ヒントは `UniqueOpts{ByArgs, ByState}` で定期実行に合流する。ルールや予約を連続で
  編集してもパスは 1 回で足りる
- `worker.periodic_jobs: false`（k8s 構成）ではどのジョブも自動投入されない。
  CronJob から `rokuban enqueue` で投入する

手で即時実行できる。ジョブ名はハイフン区切り（`epg-sync` / `tuner-sync` /
`ruler-pass` / `reconcile-pass` / `record-sweep`）。

```sh
docker compose exec rokuban rokuban enqueue ruler-pass --config /config.yml
# inserted job "ruler-pass" (id=42) for site "default"
# 既に待機中なら投入されず、終了コードは 0 のまま:
#   job "ruler-pass" already pending for site "default", not inserted
```

**ルールを作ってから録画が始まるまでに待つものは 3 段ある。**

1. **番組が EPG プロジェクションに入る** — `epg_sync`（最大 10 分）。その手前に
   mirakc の `update-schedules`（既定 1 日 2 回）があるので、**放送直前の番組は
   そもそも射影に無い**ことがある。ruler は射影しか見ない（不変条件 1）
2. **ruler が `reservations.base` を導出する** — ルール書き込みと同一トランザクションで
   ヒントが入るので通常は即座。ヒントが落ちれば最大 10 分
3. **reconciler が mirakc に schedule を作る** — `ruler_pass` の完了がヒントなので
   続けて走る。落ちれば最大 30 秒

## M1: 実放送を 1 本録る

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

**予約 API は書き込みと同じトランザクションで reconcile ジョブを投入する**ので、
通常は待たずに反映される（issue #24 M2-17）。定期パス（30 秒間隔）は取りこぼしを
拾う保険で、そちらが真実。ログに出る。

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

### 出口基準チェックリスト（M1）

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

## M2: ルールで録る

M1 が「1 本録れる」の確認だったのに対し、M2 は**放っておいても録れ続けること**と
**録られていない理由が読めること**の確認である。ルールの UI は無いので API で作る。

### 1. 条件を試す（M2-2）

`POST /api/programs/search` は ruler と**同じコンパイラ**（`internal/rulequery`）を
通る。ここでヒットする集合が ruler がマッチさせる集合なので、ルールを作って
10 分待ってから「1 件もマッチしなかった」と気付くのを避けられる。

ボディはルールの**条件部分と同じ形**（`name` / `priority` / `dedupe*` は受け取らない）。

```sh
curl -s -X POST http://localhost:40773/api/programs/search \
  -H 'Content-Type: application/json' \
  -d '{"textMatches":[{"target":"name","mode":"keyword","value":"アニメ"}],
       "channelTypes":["BS"],
       "genres":[7]}' | jq length
```

返るのは **programId の配列だけ**（昇順）。題名を見るには 1 件ずつ引く。

```sh
curl -s -X POST http://localhost:40773/api/programs/search \
  -H 'Content-Type: application/json' \
  -d '{"textMatches":[{"target":"name","mode":"keyword","value":"アニメ"}]}' |
  jq -r '.[]' | head -20 |
  while read -r id; do
    curl -s "http://localhost:40773/api/programs/$id" |
      jq -r '[.startAt, .name] | @tsv'
  done
```

- `mode: "keyword"` は**正規化した列同士の部分一致**（「ＮＨＫ」で「NHK」に当たる）
- `mode: "regex"` は Postgres の POSIX ARE。**後読みは使えない**。パターンは実際に
  Postgres で検証されるので、不正なら検索でもルール作成でも 400 で弾かれる
- `target` は `name` / `description` / `extended`、`negate: true` で除外条件
- `times[].weekdays` はビットマスク（bit0 = 月）。`endSec < startSec` は翌日跨ぎ

### 2. ルールを作る（M2-1）

```sh
curl -s -X POST http://localhost:40773/api/rules \
  -H 'Content-Type: application/json' \
  -d '{"name":"BS アニメ",
       "priority":20,
       "textMatches":[{"target":"name","mode":"keyword","value":"アニメ"}],
       "channelTypes":["BS"],
       "genres":[7],
       "dedupeEnabled":true,
       "dedupeThreshold":0.85,
       "dedupeWindowSeconds":2592000}' | jq
```

| 注意点 | |
|---|---|
| `dedupeEnabled: true` | `dedupeThreshold` が必須（無いと 400）。`dedupeWindowSeconds` は任意で、**省略すると時間窓なし**（全履歴が比較対象） |
| `sites` | 省略または空 = 全サイト |
| `keepOriginal: "until_encoded"` | `encodeProfiles` が空だと 400（M3 まで消費されない） |
| `filenameTemplate` | Go の `text/template` として作成時に検証され、壊れていれば 400 |
| `enabled` | 既定 `true`。`priority` の既定は 10 |

更新は `PATCH /api/rules/{id}`（ボディは `POST` と同じ `RuleInput`）、一覧は
`GET /api/rules`。

削除は導出予約の整理を伴い、**内訳が応答に載る**。

```sh
curl -s -X DELETE http://localhost:40773/api/rules/1 | jq
# {"id":1,"deletedReservations":11,"detachedReservations":1}
```

`detachedReservations` は overrides が付いていて**消さずに残した**予約
（`state = 'detached'`）。ユーザーの投資がある行は削除しない。

### 3. ruler が予約を導出するのを待つ（M2-3）

ルール書き込みと同一トランザクションで `ruler_pass` が投入されるので、通常は
待たずに走る。ログで確認する。

```
INFO ruler: pass complete site=default rules=1 desired=12 created=12 updated=0 deleted=0 delete_candidates=0
```

- `desired` が 0 — 条件がマッチしていない。手順 1 に戻る
- `desired` は出たが `created` が 0 で `updated` も 0 — 既に収束している（**評価は全量、
  書き込みは差分**なので、変化がなければ何も書かない。これが正常）
- `delete_candidates` > 0 で `deleted` が 0 — 削除候補が EPG プロジェクションから
  消えていて凍結された、またはブレーカーが発動中（後述）

```sh
curl -s http://localhost:40773/api/reservations |
  jq '.[] | {id, programId, source, state, ruleId, skip, title}'
```

- `source` は**列ではなく導出値**。`program_intents` に `action='record'` があれば
  `manual`、無ければ `rule`。手動予約に後からルールがマッチしても `manual` のまま
  変わらない（#26 で列を廃止した）
- `state` は `active` / `detached` / `orphaned`。`detached` はルールが base を
  供給しなくなったが overrides があるので残っている行

### 4. reconciler が mirakc に schedule を作るのを待つ

`ruler_pass` の完了が `reconcile_pass` のヒントなので続けて走る。ログと mirakc 側の
確認は [M1 の手順 2](#2-reconciler-が-mirakc-に-schedule-を作るのを待つ) と同じ。

```
INFO reconciler: pass complete desired=12 observed=12 missing=0 created=0 stale=0 deleted=0 update_diff=0 recreated=0 start_delayed=0
```

**`skip` が true の予約と `state = 'orphaned'` の予約は同期対象に入らない**
（`desired` にも数えられない）。予約はあるのに mirakc に schedule が無いときは、
まずここを見る。

### 5. 番組単位で外す / 上書きする（M2-4）

ルールを触らずに 1 番組だけ操作する。除外（a）と上書き（b〜d）の 2 系統がある。

```sh
# (a) この番組は録らない（= program_intents に action='skip' を書いて導出行を落とす）
curl -s -X DELETE http://localhost:40773/api/reservations/12 -o /dev/null -w '%{http_code}\n'
# → 204

# (b) この予約だけパラメータを上書きする
curl -s -X PATCH http://localhost:40773/api/reservations/12 \
  -H 'Content-Type: application/json' \
  -d '{"priority":30}' | jq '{id, overrides}'

# (c) 上書きをフィールド単位で戻す
curl -s -X PATCH http://localhost:40773/api/reservations/12 \
  -H 'Content-Type: application/json' \
  -d '{"reset":["priority"]}' | jq '{id, overrides}'

# (d) 上書きを全部戻す（予約単位の「ルールに戻す」）
curl -s -X DELETE http://localhost:40773/api/reservations/12/overrides | jq '{id, overrides}'
```

- **`null` で消す形は無い。** 消すのは `reset` 配列。同じフィールドを値と `reset` の
  両方に書くと 400、`reset` に未知の名前があっても 400
- `skip` は overrides のキーではない（`program_intents.action` が担う）。取消は (a)
- (d) は `program_overrides` の行を消すだけで、`action`（record/skip）には触らない

**取消（a）は行を消すだけではない。** 行を消すだけでは「消された行」と「最初から
無かった行」が ruler から区別できず、次の全量パスが復活させてしまう。だから
`program_intents` に `action='skip'` を書いてから導出行を落とす。

そのため**順序に意味がある落とし穴が 1 つある**。`program_overrides` に行がある番組は
`action='skip'` があっても ruler の desired に残る（上書きの存在は「この番組に
ユーザーの投資がある」の主張であって、録るかどうかとは別の問い）。したがって
**上書き済みの予約を取消すと、予約行は次の ruler パス（最大 10 分）で再び現れる** ——
`skip = true` の「なぜ録られないか」を説明する行として。行ごと消したいなら
先に (d) で上書きを消してから (a) を呼ぶ（(a) の後は予約行が無いので (d) は 404）。

### 6. 重複排除（M2-6）— なぜスキップされたかを読む

`dedupeEnabled` なルールの予約は、**同じ `rule_id` の `status='finished'` な録画**と
題名を pg_trgm の `similarity()` で比べ、`dedupeThreshold` 以上なら
`base.skip` が立つ。判定根拠は予約に焼かれる。

```sh
curl -s http://localhost:40773/api/reservations |
  jq '.[] | select(.skip) | {id, title, dedupMatchRecordingId, dedupSimilarity}'
```

**根拠 2 列の有無が「なぜ skip なのか」の判別基準である。**

| `dedupMatchRecordingId` | 意味 | UI |
|---|---|---|
| ある | 重複排除が立てた skip。相手の録画 ID と類似度まで説明できる | 一覧に「重複」バッジ、詳細に `重複（録画 #7・類似度 0.91）` |
| ない | ユーザーの「録るな」（`action='skip'`）、または凍結された `base.skip` | 一覧に「除外」バッジ、詳細に `録画しない（除外）` |

根拠 2 列は**毎パス作り直す導出値**で、マッチが消えれば NULL に戻る。これが
「いま重複と判定されている」ことの唯一の表現なので、`skip` だけを見て判断しない。

マッチした相手を確かめる。

```sh
curl -s http://localhost:40773/api/recordings | jq '.[] | select(.id == 7) | {title, status, startAt}'
```

**`program_intents.action = 'record'` は dedup の skip に勝つ**（EPGStation#473）。
このとき根拠 2 列は消えない —— UI が「重複と判定したが録る」と説明できるようにするため。

ただし **`action='record'` を既存の予約行に立てる API は M2 時点で無い。**
`POST /api/reservations` は予約行も同時に作るため、行がある programId に投げると
409 になる。運用上は取消 → 再予約の 2 手になる（`UpsertProgramIntent` が
`skip` を `record` に上書きする）。

```sh
# 重複と判定された予約 12 を「それでも録る」
curl -s -X DELETE http://localhost:40773/api/reservations/12 -o /dev/null   # intent{skip}
curl -s -X POST http://localhost:40773/api/reservations \
  -H 'Content-Type: application/json' \
  -d '{"programId":319215325618427,"title":"…","startAt":"2026-08-01T22:00:00+09:00",
       "durationMs":1800000}' | jq '{id, source, skip}'                     # intent{record}
# → source が manual に変わり、skip は false
```

次の ruler パスは `base.skip` と根拠 2 列を作り直すが、`action='record'` が勝つので
`skip` は false のままになる。

**自己一致は除外されている**（`(network_id, service_id, event_id)` の不一致が条件）。
放送済み番組の予約は GC まで残るので、これが無いと録画が `finished` になった次の
パスで類似度 1.0 の自己一致が必ず起き、その予約が `markOrphaned` /
開始遅延検出の入力からも消えてしまう。

### 7. 重なりの件数を見る（M2-8）

```sh
curl -s http://localhost:40773/api/programs/319215325618427/overlaps | jq
# {"count":2,"reservations":[{"id":3,"programId":…,"title":"…","startAt":"…","durationMs":1800000}, …]}
```

UI では予約ボタンの近くに常時出る（展開操作なし。0 件なら何も描画しない）。

```
⚠ 同じ時間帯に2件の予約があります（22:00 ○○・22:30 ××）
```

**これは件数だけを主張する。** チューナー本数は見ていない（`count > 0` でも同一物理
チャンネルなら 1 本で賄えるし、`count = 0` でも見えない消費者がいる）。「録画できません」
「競合しています」と読み替えてはならない。容量の話は手順 10。

自分自身（同じ programId の予約）・`state='orphaned'`・実効 `skip` の予約は数えない。
判定は**半開区間**（番組表は連続しているので、閉区間で見ると隣接番組が全部
重なりになる）。

### 8. 開始遅延を見る（M2-7）

「開始時刻 + 猶予」を過ぎても `recordings.started_at` が観測されない予約を
reconciler が毎パス検出する。猶予は `reconciler.start_delay_grace`（既定 **3 分**、
`config.compose.yml` には無いので compose 運用では既定値）。

```
ERROR reconciler: recording not started past start time + grace reservation_id=5 program_id=… title=… elapsed=6m12s
```

```sh
curl -s http://localhost:40773/metrics | grep rokuban_reconcile_start_delayed
# rokuban_reconcile_start_delayed{site="default"} 1
```

**ゲージなので、mirakc 側で開始が観測されれば次のパスでゼロに戻る。** ゼロに戻らない
まま続くのが異常（EPGStation#724 のようなチューナー再接続ハングへの保険であり、
Rokuban 側に打てる手は無い —— 検出して人に見せるところまでが仕事）。DB に状態は
持たないので、`quality_events` を探しても何も無い。

### 9. ドロップ統計と PID 種別（M2-13）

```sh
curl -s http://localhost:40773/api/recordings/1/drop-stats |
  jq '.[] | {pid, pidType, packets, drops, errors, scrambled}'
```

種別ごとに畳むと読みやすい。

```sh
curl -s http://localhost:40773/api/recordings/1/drop-stats |
  jq 'group_by(.pidType // "（分類なし）")
      | map({type: (.[0].pidType // "（分類なし）"),
             pids: length,
             drops: (map(.drops) | add)})'
```

値域は `video` / `audio` / `other` / `pat` / `cat` / `nit` / `sdt` / `eit` / `tot` /
`pmt`、そして**分類できなければフィールドそのものが無い**。

- **`pidType` が無い PID があるのは正常**。PSI 解析の失敗で ingest を落とさないし、
  統計を隠しもしない（分類の失敗で行が消えるのが最悪）。**「種別が無い = 異常」ではない**
- 字幕・文字スーパー・データ放送はすべて `other`。区別には記述子が必要で、
  **記述子は読まない**（不変条件 6）
- **音声が `other` に見えることがある。** `gots` の `IsAudioContent()` は LATM AAC
  （`stream_type = 0x11`）を含まない。ISDB の GR/BS/CS は 0x0F（MPEG-2 AAC）が主なので
  実害は 4K/8K に限られる見込みだが、**並走でこれを見たら「分類の限界」であって
  ドロップではない**。1 行で足せるので観測したら報告する

`scrambled` が 0 以外はドロップではなく**エッジ環境の異常**（B-CAS 接触不良・
pcscd 死亡・decode-filter 設定漏れ）で、別枠のアラート対象。

### 10. チューナー容量超過（M2-10）

```sh
# 窓は UTC の Z 表記で書くのが安全。`+09:00` をそのまま書くとクエリ文字列の `+` が
# 空白に復号されて時刻のパースに失敗する（書くなら `%2B09:00`）
curl -s "http://localhost:40773/api/capacity/overages?start=2026-07-28T00:00:00Z&end=2026-08-05T00:00:00Z" | jq
# [{"site":"default","startAt":"…","endAt":"…","shortfall":1,"jammedTypes":["BS"]}]
```

読み方は 3 つだけ。

- **返った区間は確実に超過している。** `shortfall` は不足本数、`jammedTypes` は
  詰まった種別（Hall の条件を破った部分集合）なので「BS が 1 本不足」まで言える
- **返らなかった区間が「収まる」保証は無い**（後述）
- **どの番組が負けるかは主張しない。** 勝敗を決めるのは mirakc であり、Rokuban から
  見えない消費者がいるので予測できない。UI の文言も「この予約は競合しています」ではなく
  「この時間帯はチューナーが不足しています」

区間の結合は `(不足本数, 詰まった種別)` が一致するときだけ。隣接区間を無条件に
結合して最大値を報告すると、不足が小さい部分区間について知らないことを主張して
しまうため（下界に限る原則）。

需要の単位は**予約件数ではなく異なる物理チャンネル数**（同一物理チャンネルなら
相乗りできる。副産物としてマルチ編成のサブサービスが畳まれる）。数えるのは
reconciler が実際に schedule を作る予約だけ（実効 `skip` と `orphaned` は需要に
ならない）。

#### 射影が動いているかを先に確認する

**`tuner_sync` に 1 行も無いサイトは何も主張しない**（超過区間を返さない）。
「チューナー 0 本」と「まだ同期していない」を射影から区別できず、後者を容量ゼロと
扱うと全区間が超過になって警告が洪水になるため。**つまり同期が壊れていると
警告が黙って消える。** 空配列を見たら必ずこちらを先に見る。

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

`rokuban_capacity_overages` は `tuner_sync` パスの完了時にだけ入れ直すので、鮮度は
同期間隔（10 分）ぶん遅れる。UI と API は常に再計算した値を返す。**ゲージは構成の
余裕を眺めるためのもので、アラートの一次情報ではない。**

### 11. 番組表グリッド（M2-9）

番組タブの表示形式切り替え（リスト / グリッド）は **`lg`（幅 64rem = 1024px）以上で
のみ出る**。モバイルは常にリスト。狭くしても選択は保持されるので、幅を戻せば
グリッドに戻る。

- 縦 120px/時間で 24 時間ぶんを 1 回で取る（リストは 6 時間の窓を積む）
- **セルの高さに下限が無い。** 5 分番組は 10px。見やすさのために下限を入れると
  同時性の符号化が崩れ、グリッドの存在理由が失われるため
- 番組を 1 つも持たないサービスは列にしない（空の列が数十本並ぶと同時性が読めない）
- `overlay` 層（番組セルより上・ヘッダより下、`pointer-events-none`）に現在時刻
  インジケータが乗る。容量超過の帯はこの口を使う予定で、**まだ繋がっていない**

**「全サービス × 24 時間でスクロールが落ちない」は実ブラウザで未検証**（jsdom では
計測できない）。縦横とも間引く実装は入っているが、**受け入れ確認は実機で行う**。
確認するのは次の 3 つ。

- [ ] 全サービス（サービス絞り込みなし）× 24 時間でスクロールが滑らかに動く
- [ ] sticky ヘッダ（時刻軸・サービス軸）が縦横スクロールでずれない
- [ ] 5 分番組のセルが潰れずに存在する（読めなくてよい。**在ることが要件**）

セル間の矢印キー移動は未実装（現状はグリッド領域の focus とスクロールまで）。

### サーキットブレーカー（M2-5）

導出削除の暴走を止めるラッチ。**行の存在 = 発動中**なので、`GET /api/breakers` が
空配列を返すのが正常系。

```sh
curl -s http://localhost:40773/api/breakers | jq
# []
```

発動すると UI に**居座り型のバナー**が出る（トーストにしない。一定時間で消えると
気付かないまま放置されうるため）。ログとメトリクスにも出る。

```
ERROR circuit breaker tripped — deletes withheld until manually resumed site=default breaker=ruler_deletes pending_deletes=137 threshold=50 tripped_at=…
```

```sh
curl -s http://localhost:40773/metrics | grep rokuban_circuit_breaker_tripped
# rokuban_circuit_breaker_tripped{breaker="ruler_deletes",site="default"} 1
```

| 名前 | 何を守るか |
|---|---|
| `ruler_deletes` | ruler が「ルール × EPG」から導出した予約削除。閾値は `ruler.max_deletes_per_pass`（既定 **50**）。**導出削除を止められる唯一の場所** |
| `reconcile_total_loss` | reconciler の「desired が 1 件も無いのに `rokuban:reservation=` tag の付いた schedule が観測される」全損シグネチャ。**件数の閾値ではないので、出たら本当に異常。** `threshold` が 0 で出るのは欠落ではなく「0 件しか許されない状況で N 件消そうとした」の意 |

**発動はラッチである。** 件数が閾値以下に戻っても自動では解けない（一瞬止まって
自動復帰した障害がアラートに残らないと、EPG が繰り返し欠損する状況を見逃す）。
再開は必ず API を通す。

```sh
# 1. 何が消されようとしていたかを見る（detail は最大 20 件の抜粋）
curl -s http://localhost:40773/api/breakers |
  jq '.[] | {name, trippedAt, pending, threshold, total: .detail.total,
             sample: [(.detail.programs // [])[] | {programId, title}]}'

# 2. EPG 射影が壊れていないかを確かめる
curl -s http://localhost:40773/metrics |
  grep -E 'rokuban_epg_programs_projected|rokuban_epg_channels_without_programs'

# 3. 納得したら再開する
curl -s -X POST http://localhost:40773/api/breakers/ruler_deletes/resume -o /dev/null -w '%{http_code}\n'
# 204 = 再開 / 404 = そもそも発動していない / 400 = 未知のブレーカー名
```

- **発動中でも作成と更新は続く。** 止めているのは削除だけなので、「新しい予約が
  作られない」はブレーカーのせいではない
- 番組終了後の GC はブレーカーの対象外（削除対象が時刻の比較だけで決まり、EPG の
  状態に左右されないため）。長時間停止後に溜まった期限切れ行の一括削除は正常
- 再開しても原因（EPG の欠損・ルールの一括削除）が残っていれば次のパスで再び発動する
- `rokuban_*_circuit_breaker_trips_total` は**発動に遷移した回数**。いま止まっているかは
  `rokuban_circuit_breaker_tripped` ゲージが答える

### M2 で見るメトリクス

| メトリクス | 読み方 |
|---|---|
| `rokuban_ruler_last_pass_timestamp_seconds` | `time() - この値` が周期（10 分）を大きく超えたら ruler が止まっている |
| `rokuban_ruler_reservations_total{action}` | `created` / `updated` / `deleted` / `gc`。差分書き込みなので収束後は増えないのが正常 |
| `rokuban_reconcile_last_pass_timestamp_seconds` | 同上（30 秒） |
| `rokuban_reconcile_pending_diff{action}` | `create` / `delete` / `update` は**ゼロに収束すべき**。`update_deferred` は録画中の番組を意図的に触っていないぶんで、**非ゼロが正常でありうる** |
| `rokuban_reconcile_schedules_total{action}` | 実際に差分を消した量（`created` / `deleted` / `recreated`） |
| `rokuban_reconcile_schedule_lost_total` | 再作成の DELETE 成功 → POST 失敗。**0 以外はアラート**（次パスまでに開始時刻を過ぎると取りこぼす） |
| `rokuban_reconcile_start_delayed{site}` | 開始遅延。ゼロに収束すべき |
| `rokuban_circuit_breaker_tripped{site,breaker}` | 1 の間、導出削除が止まっている。**時間ではなく即座に通知する対象** |
| `rokuban_tuners_projected{site}` / `rokuban_tuner_sync_last_success_timestamp_seconds{site}` | 射影が生きているか。0 / 古いなら容量判定は無効 |
| `rokuban_capacity_overages{site}` | 構成の余裕を眺めるゲージ。**非ゼロは信頼できるが、ゼロは保証ではない** |
| `rokuban_sweep_last_pass_timestamp_seconds` | `record_sweep`（5 分）が生きているか |

### 沈黙は保証ではない

M2 で足した観測のうち、**「出ていない = 大丈夫」と読んではいけない**ものを 1 箇所に
まとめる。設計上ここは全部「警告を見逃す」側に偏らせてある（過剰に警告しない代わりに
沈黙が保証にならない、という取引）。

| 沈黙 | 何を意味しないか | 何を見るか |
|---|---|---|
| `/api/capacity/overages` が空 | 収まるとは限らない。並走 EPGStation・ライブ視聴・EPG 収集は見えず、mirakc の `excluded_channels` は `/api/tuners` に載らないので**知る術がない** | `rokuban_tuners_projected` が 0 でないこと |
| 同上（射影が空） | 射影が 1 行も無いサイトは**何も主張しない**ので、同期が壊れると警告が黙って消える | `tuner_sync` の行と `tuner_sync_last_success` の鮮度 |
| `drop-stats` の `pidType` が無い | 分類できなかっただけで、ドロップ統計そのものは正しい | `packets` / `drops` は種別と独立に信頼できる |
| `pidType` が `other` | 音声でないとは限らない（LATM AAC は `other` に落ちる） | 4K/8K を録ったなら疑う |
| `/api/programs/{id}/overlaps` の `count = 0` | 録れるとは限らない（他サイトや mirakc の他の消費者は数えていない） | 手順 10 |
| `/api/breakers` が空 | 削除が正しかったとは限らない。**閾値を下回る削除は素通りする** | `rokuban_ruler_reservations_total{action="deleted"}` の増え方 |
| `rokuban_reconcile_start_delayed` が 0 | 録画が始まったことの確認ではない（猶予 3 分の内側は検出しない） | `recordings.started_at` |

### 既知の未解決事項（誤読しやすいもの）

**`orphaned` は「録れなかった」を意味しないことがある。** `markOrphaned`
（`internal/reconciler/reconciler.go:659`）の判定は「番組終了時刻を過ぎた」と
「mirakc の schedule に観測されない」の 2 つだけで、**`recordings` 行の有無を見ていない**。
mirakc が録画完了後に schedule を落とすなら、**成功した録画の予約も `orphaned` に
なりうる**。`docs/schema.md` §3 の定義（録画されずに終わった行）と食い違う。

- **未検証。** 分岐点は「録画完了後に mirakc が `GET /api/recording/schedules` から
  schedule を落とすか」で、実機で確認していない。落とさないなら起きない
- 並走中に `orphaned` を見たら、まず `recordings` に対応行があるか確かめる。
  あるならこの既知の挙動で、録画は成功している
- GC は終了 + 24 時間なので、その間 UI には `orphaned` として見える
- 重複排除の自己一致を許していたらこの挙動は dedup の skip に隠れていた
  （`effective.skip` で `listDesired` から落ちるため）。自己一致を除外する決定に
  したので**隠れずに残っている**。切り分けはこちらの方が楽

```sh
docker compose exec postgres psql -U rokuban -d rokuban -c \
  "SELECT r.id, r.program_id, r.title, r.state,
          (SELECT count(*) FROM recordings rec WHERE rec.reservation_id = r.id) AS recordings
     FROM reservations r WHERE r.state = 'orphaned' ORDER BY r.program_start_at DESC"
```

**`pidType` が `other` の音声 PID**（LATM AAC / `stream_type = 0x11`）も同種の
「誤読しやすい正常」。`gots` の `IsAudioContent()` の値域に従っているだけで、
自前の `stream_type` 表は作らない方針。観測したら 1 行で足せる。

**容量超過の帯 UI は未実装。** バックエンド（`/api/capacity/overages`）とグリッド側の
口（`overlay`）は揃っているので、超過はグリッドを見ても分からず API で確認する。

### 出口基準チェックリスト（M2）

M2 の出口基準は「ルールで録れ、除外・上書きが生き残り、予約差分がゼロ or 全件説明可能」
（[issue #6](https://github.com/fetburner/rokuban/issues/6) / #24 の M2-14）。

- [ ] `POST /api/programs/search` の結果と、同じ条件のルールが作る予約の集合が一致する
- [ ] `POST /api/rules` の後、**ヒント経路で**（10 分待たずに）`ruler: pass complete` が出て予約ができる
- [ ] その予約が mirakc に `rokuban:reservation=<id>` tag 付きで入り、実際に録画される
- [ ] ルールを `PATCH` で無効化（`enabled: false`）すると次の ruler パスで予約が消える（ruler は `enabled = true` のルールだけを評価する）
- [ ] `DELETE /api/rules/{id}` の応答の `deletedReservations` / `detachedReservations` が実際の予約の増減と一致する
- [ ] 上書きのない予約を `DELETE /api/reservations/{id}` すると**次の ruler パスで復活しない**（`program_intents` に skip が残る）
- [ ] 上書きのある予約を取消すと `skip = true` の説明行として現れる（消えたままにはならない。手順 5）
- [ ] `PATCH /api/reservations/{id}` の `priority` が mirakc の schedule に反映される（DELETE + POST の再作成）
- [ ] `reset` / `DELETE .../overrides` で上書きが戻り、ルールの値に戻る
- [ ] 同じ番組を 2 回録った後、重複排除が 2 回目を `skip` にし、`dedupMatchRecordingId` / `dedupSimilarity` が入る
- [ ] その予約を取消 → 再予約すると `skip` が false になり（`action='record'` が勝つ）、根拠 2 列は残る
- [ ] 重なる時間帯の番組で「同じ時間帯に N 件の予約があります」が予約前に見える
- [ ] `GET /api/breakers` が平常時は空配列である
- [ ] `ruler.max_deletes_per_pass` を 1 に下げてマッチ数の多いルールを無効化すると発動し、バナーが出る。**閾値を戻してから** `resume` すると消える（超えたまま再開すると次のパスで再発動する）
- [ ] `tuner_sync` に実機のチューナーが投影され、`rokuban_tuners_projected` が本数と一致する
- [ ] チューナー本数を超える予約を意図的に入れると `/api/capacity/overages` に区間が出て、`jammedTypes` が実際の種別と一致する
- [ ] 録画のドロップ統計に `pidType` が付き、`video` / `audio` / PSI テーブル名が妥当に見える
- [ ] **実ブラウザで**全サービス × 24 時間のグリッドがスクロールできる（前述の 3 項目）
- [ ] `rokuban shadow-diff` が **`RokubanOnly` / `EPGStationOnly` ともゼロ**、または全件が `Expected` で説明できる
- [ ] 数日放置しても `rokuban_ruler_last_pass_timestamp_seconds` / `rokuban_reconcile_last_pass_timestamp_seconds` が更新され続けている
- [ ] `rokuban_reconcile_pending_diff{action="create"}` と `{action="delete"}` がゼロに戻る（`update_deferred` は非ゼロでよい）

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

### shadow-diff で予約差分を確認する


EPGStation 側の API の形は**実機（v2.10.0）で確認済み**。`GET /api/reserves` は
`{ reserves: [...], total: n }` を返し、各要素は `programId`（数値の Mirakurun ID）/
`startAt` / `endAt`（UnixtimeMS）/ `isSkip` / `isConflict` / `isOverlap` /
`isTimeSpecified` / `ruleId`（ルール由来のみ）を持つ。別バージョンで並走する場合は
形が変わりうるので、最初に 1 回だけ確かめるとよい。

```sh
curl -s "$EPGSTATION_URL/api/reserves?type=all&isHalfWidth=false&limit=1&offset=0" | jq
```

M2 の出口基準は「予約差分ゼロ or 全件説明可能」（issue #6 / #24 の M2-14）。
`rokuban shadow-diff` は Rokuban（DB）と EPGStation（API）の予約集合を programId で
突き合わせ、差分を標準出力にレポートするサブコマンド。

```sh
rokuban shadow-diff --config config.yml --epgstation-url http://localhost:8888
```

出力例（`Both` は件数のみ、`RokubanOnly` / `EPGStationOnly` / `Expected` は明細も出る）:

```
=== shadow-diff レポート ===
Both:            12
RokubanOnly:      1
EPGStationOnly:   0
Expected:         3

-- RokubanOnly（説明できない差分。EPGStation 側に対応する予約がない） --
programId        title       startAt (JST)
327360102415398  ○○第3話  2026-08-01 22:00:00 JST

-- Expected（allowlist で説明可能な差分） --
programId        title    startAt (JST)            reason
327360102415399           2026-08-01 23:00:00 JST  Rokuban 側でこの番組を除外している（program_intents.action = 'skip'）ため、EPGStation 側にのみ存在するのは想定通り
```

時刻は番組表と同じく JST 固定で表示する（実行環境のローカルタイムゾーンではない）。

**終了コード**: 説明できない差分（`RokubanOnly` か `EPGStationOnly` が 1 件でもある）
があれば `1`。CI や運用スクリプトから `rokuban shadow-diff ... && echo ok` のように
`&&` で連ねられる。

以下は allowlist（`Expected` に落ちる、説明可能な差分）:

| 条件 | 理由 |
|---|---|
| EPGStation 側が `isTimeSpecified` または `programId` 欠落 | 時刻指定予約は programId を持たず照合できない。Rokuban に時刻指定予約の機能はない（[recording.md §9](recording.md)） |
| EPGStation 側が `isSkip` | EPGStation 側でユーザーが除外した予約 |
| EPGStation 側が `isOverlap` | EPGStation の重複排除ロジックで除外された予約 |
| Rokuban 側が skip 意図（`program_intents.action = 'skip'`） | Rokuban で除外した予約。EPGStation 側にだけ有るのは正常 |

**`isConflict` は allowlist に入らない。** チューナー競合は両者で起きる条件が同じはずで、
片方だけの予約に現れているなら `EPGStationOnly` / `RokubanOnly` として報告され、
調査が必要。

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
