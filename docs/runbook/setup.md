> [runbook.md](../runbook.md) の一部。索引から辿る。

## 前提

導入の前に、mirakc が動いていて、録画の保存先が設定されていることを確かめる。mirakc のイメージに cat と dd が含まれていることと、専用チューナーが兼用チューナーより前に並んでいることも確かめる。

- mirakc が動いていて `GET /api/version` が返る
- mirakc の `recording.basedir` と `recording.records-dir` が設定されている。どちらも未設定だと `/api/recording/*` が 404 になる
- mirakc の `tuners:` では専用チューナーを兼用チューナーより前に並べる
- mirakc のイメージに `cat` と `dd` が含まれている

### イメージに cat と dd が要る理由

必要なバイナリだけを含むイメージだと、録画ストリームの取得が 500 で失敗して ingest が完走しない。HEAD は成功するので、「HEAD は通るのに GET が 500」という切り分けにくい形で出る。

`FROM scratch` + magicpak のようなイメージだと、`GET /api/recording/records/{id}/stream` が 500 を返す。HEAD はファイルのメタデータだけで応答できるので成功する。

### チューナーの並び順

mirakc の実装を読んだ限り、mirakc は設定順で最初に空いているチューナーを選ぶ。兼用チューナーを先に置くと、専用チューナーで受けられる種別にも兼用チューナーを使い、他の種別の余地を減らす。

専用チューナーを先にしても、mirakc は後に来る予約を見ずに選び、番組の途中でチューナーを替えない。そのため防げない失敗は残る。並び順はどちらにも反例がある。

<details><summary>根拠</summary>

- 設定順に最初の空きチューナーを選ぶ、mirakc main（`4f2493f98374`）の実装: `mirakc-core/src/tuner.rs` の `activate_tuner`

</details>

## 起動

docker compose up で起動する。初回は公式イメージに ffmpeg を積んだ full イメージをローカルでビルドする。自前の compose を持っている場合は、停止の猶予を確認する。

```sh
cp .env.example .env
$EDITOR .env          # MIRAKC_URL と POSTGRES_PASSWORD は必須
docker compose up -d  # 初回は公式イメージに ffmpeg を積んだ rokuban:full をローカルビルドする
docker compose logs -f rokuban
```

初回の `up` は `Dockerfile.full` を使って `rokuban:full` を組む。公式イメージ `ghcr.io/fetburner/rokuban` を pull し、その上に apt で ffmpeg を足し、CM 検出の JLSE ツールを C/C++ のソースからビルドして載せる。Go / Node のソースビルドは走らない。ffmpeg を自分用にビルドするのは再配布ではないので、公式配布物は ffmpeg 非同梱のまま保てる（[docs/overview.md](../overview.md)）。HW エンコード等で自前イメージを使うなら、`.env` の `ROKUBAN_IMAGE` で差し替える。

`http://localhost:40773` で UI が開く。

### 自前の compose の停止の猶予

自前の compose を持っている場合は、停止の猶予を確認する。ジョブの待ち時間を伸ばす（長いエンコードを守る）なら、猶予も対で伸ばす。足りないと停止のたびに、実行中のジョブが SIGKILL される。

確認するのは `docker-compose.yml` の `stop_grace_period` である。`docker compose down` / `stop` のたびに、猶予が足りないと実行中のジョブが落ちる。

rokuban は SIGTERM を受けると、実行中のジョブを `--soft-stop-timeout`（既定 5 秒）まで待ってから畳む。Docker の既定の猶予は 10 秒なので、既定のままなら収まる。リポジトリの `docker-compose.yml` には 30 秒を書いてある（[docs/operations.md](../operations.md) §5「Deployment 併用時」の足し算）。

ジョブごとの回収は次の通り。

- ingest は回収処理が回収する
- encode は、args に保存された締切後に回収処理が同じ job を再試行する。実尺が不明な場合と、締切のない旧 args は 12 時間を使う
- cm_detect は、次の `cm_detect_reconcile` が lock の解放を確認して代替ジョブへ置き換える
- その他のジョブは回収処理が回収する

<details><summary>根拠</summary>

- 回収処理: `JobRescuer`

</details>

### 起動直後の同期

正常なら起動から 30 秒ほどで EPG の全量同期が 1 回走る。

```
INFO epg sync complete services_projected=19 programs_fetched=7139 programs_projected=2680 ...
```

`programs_projected` が 0 のままなら、mirakc 側の EPG がまだ空か、`update-schedules` ジョブが一度も走っていない（既定は 1 日 2 回）。

同じ頃にチューナーの射影も 1 回走る。容量超過の判定はこの射影だけを読むので、これが出ていない間は超過が一切報告されない（[troubleshooting.md](troubleshooting.md) の「`/api/capacity/overages` が常に空」）。

```
INFO tuner sync complete site=default tuners_fetched=4 tuners_projected=4 stale=0 capacity_overages=0
```

## 設定の 2 段構え

compose 運用の設定は 2 箇所に分かれる。環境ごとに変わる値は環境変数のファイルに置き、恒久的な設定は Rokuban 本体の設定ファイルに直接書く。キーの網羅は config.example.yml が権威である。

キーの網羅は [config.example.yml](../../config.example.yml)、各値をどう決めるかの判断材料は [docs/configuration.md](../configuration.md) を見る。環境ごとに変わる値は `.env` に、恒久的な設定は `config.compose.yml` に書く。

1. `.env` は、`docker-compose.yml` が読む環境変数である。`MIRAKC_URL` と `POSTGRES_PASSWORD` は必須で、無いと compose が起動を拒否する。他は任意で、コメントアウトされたキー（ポート・イメージ・ログレベル等）は既定値を持つ
2. `config.compose.yml` は、compose がコンテナ内の `/config.yml` に read-only マウントする Rokuban 本体の設定である。`${VAR:-default}` の形で `.env` の値を参照しているキー（`ingest.concurrency` / `epg.sync_interval` / `log.*` 等）は、`.env` から変えられる

`media_dir` や `ruler.max_deletes_per_pass` のような恒久的な設定は `config.compose.yml` に書く。`.env` に口があるのは環境ごとに変わる値だけで、それ以外のキーは `config.compose.yml` を直接編集する。書かれていないキーは既定値で動くので、変えたいキーを書き足す。

## 録画の保存先

既定では、録画ファイルは Docker が管理する named volume に置かれる。実ディスクや NAS に置くには、bind mount に差し替え、ホスト側のディレクトリを uid/gid 65534 で書き込めるようにしておく。

named volume `media` は、コンテナの `/mnt/media`（`storage.media_dir`）にマウントされる。bind mount にするには、`docker-compose.yml` の `rokuban` サービスの volume 行を差し替える。

```yaml
    volumes:
      - ./config.compose.yml:/config.yml:ro
      # named volume の代わりにホスト側のパスをマウントする
      - /mnt/nas/rokuban-media:/mnt/media
```

コンテナ内のパス（`/mnt/media`）を変えないかぎり、`config.compose.yml` の `storage.media_dir` はそのままでよい。

### bind mount の所有権

bind mount には Dockerfile 側の所有権修正が適用されないので、ホスト側のディレクトリを事前に uid/gid 65534 で書き込めるようにしておく。

適用されないのは、named volume の copy-up がボリューム管理領域だけの挙動で、bind mount はホスト側のディレクトリの所有権をそのまま見せるからである。Dockerfile は `/mnt/media` を nobody 所有で焼き込んでいる。uid/gid `65534`（`nobody:nogroup`）は rokuban コンテナの実行ユーザーである。

```sh
sudo mkdir -p /mnt/nas/rokuban-media
sudo chown 65534:65534 /mnt/nas/rokuban-media
```

chown できない共有ストレージ（一部の NAS 等）では、代わりに compose 側で `rokuban` サービスに `user:` を指定して、ホスト側の既存 uid/gid に合わせる。この回避策は未検証である。計測環境が Docker Desktop for Mac で、bind mount の所有権がホストと一致しないため測れていない。

`user:` を使うのは bind mount のときだけにする。named volume では copy-up が `nobody` 所有を焼くので、`user:` を 65534 以外にすると書けなくなる。実測では、空の named volume を `--user 1000:1000` でマウントすると、マウント点は `nobody:nogroup` のままで `touch` が `Permission denied` になった。

## 置き場の目印

置き場の目印は、media root が設定した置き場であることを worker が確かめるためのファイルである。導入時に 1 回だけ作り、手では書かない。

目印は `rokuban storage adopt <store-name>` で作る。照合は完全一致で、末尾の改行も違いとして扱うので、`echo main > .rokuban-store` は改行を足して不一致になる。

worker ロールでは `storage.stores` に FS 置き場を定義する（今は 1 つだけ。[置き場の規則](../configuration/keys.md#置き場の規則)）。worker は media root の `.rokuban-store` をその名前と照合し、一致しなければ書き込みと削除を見送る。照合の判断は [operations/storage.md](../operations/storage.md) にある。

`adopt` は実際にマウントされた media root に対して実行する。マウントが外れた状態で実行すると、マウントポイントの下にあるローカルディスクに目印を書いてしまう。

新しく導入するときの順序は次のとおりである。`adopt` は DB に接続するので、`migrate up` の後でないと失敗する。

1. media root をマウントする
2. `storage.stores` に FS 置き場を定義し、その map key を目印に使う名前にする
3. `rokuban migrate up` を実行する
4. `rokuban storage adopt <store-name>` を実行する
5. worker を起動する

compose（`adopt-media` サービス）と k8s（`rokuban-migrate` Job の本体のコンテナ）では、名前 `main` でこの 3 と 4 が自動で走る。k8s の Job は `migrate up` を initContainer に、`storage adopt main` を本体のコンテナにしている。

`adopt` は、目印の無い root に目印を新しく作る前に、DB にその置き場のコピー行があるかを確かめる。あれば、マウント漏れか別のボリュームなので、エラーにして目印を書かない。同名の目印が既にあれば成功する。

置き場の名前を設定済みの構成は、この照合を含む版をデプロイした直後から、`adopt` するまで ingest・encode・削除がすべて見送られる。`rokuban_store_marker_mismatch_total{store}` が増える。`storage adopt` は新しい版にしか無いので、デプロイ後に実行する。デプロイした版のイメージで、media root と設定ファイルを worker と同じ構成でマウントし、`rokuban storage adopt <設定済みの名前>` を実行する。`adopt` は `storage.stores` に定義した FS 置き場の名前だけを受け付け、既存の同名の目印は成功として扱う。2 つ目以降の置き場も、その置き場の root をマウントして同じ手順で `adopt <名前>` を実行する。

置き場の名前は変えられない。名前を変えるときは、新しい置き場として導入し直す。

### 旧版からの更新

コピー世代を持たない版から、録画を持つ構成を更新する順序と、構成ごとの操作を示す。

1. 旧 worker をすべて停止する
2. `rokuban migrate up` で DB を移行する
3. `rokuban storage adopt <store-name>` を実行する。media root と設定ファイルは worker と同じ構成にし、DB にも接続できるようにする
4. 新しい worker を起動する

新しい worker は、コピーの無い論理資産が 1 件でもあるか、`store` が空の観測行・進捗行や、範囲が空の `delete_reconcile` のブレーカーの行が 1 件でもあれば、起動を拒む。

コピー世代を持たない版からは、2 回に分けて更新する。`media_assets.rel_path` / `state` / `deleted_at` を消す migration を含む版は、コピーの無い論理資産が残っていると停止するからである。1 回目は、コピー表とコピー世代を持ち、`storage adopt` がコピーを作る版を入れて、上の手順 1 から 4 を済ませる。2 回目で、その列を消す migration を含む版へ更新する。

更新の前に、設定へ `storage.stores` / `originals` / `derived` を書く。旧キー `storage.media_store_name` は廃止した。設定は厳密に読むので、残したまま新しい版を起動すると、全ロールで設定の読み込みが失敗する。`media_store_name: X` を持つ構成は、それを消して `X` を置き場の名前にする。持たない構成は、worker が使う FS 置き場の名前を決めて書く。どちらも下の形になる。`transfer.overlap` は置き場が 1 つでも、旧世代の削除の猶予として効く。省略すると 48 時間になるので、`cleanup.missing_asset_age` を延ばしているときだけ書く。

```yaml
# 旧
storage:
  media_dir: /mnt/media
  media_store_name: main

# 新
storage:
  media_dir: /mnt/media
  stores:
    main: { kind: fs }
  originals:
    - store: main
  derived: main
```

compose と k8s の標準の構成には、この名前（`main`）と adopt の手順が入っている。移行の操作は次のとおりである。

- compose: 手順 1 は `docker compose stop rokuban` である。手順 2 と 3 は `docker compose up -d migrate adopt-media` で、手順 4 は `docker compose up -d rokuban` である。
- k8s の手順 1: `kubectl delete scaledjob -l app.kubernetes.io/component=worker` で旧 worker の ScaledJob を消す。`kubectl get jobs,pods -l app.kubernetes.io/component=worker` が空になるまで待つ。
- k8s の手順 2 と 3: 標準の `rokuban-migrate` Job がやる。`migrate up` を initContainer、`storage adopt main` を本体のコンテナにして続けて走らせるので、`storage adopt main` は手で打たない。
- k8s の手順 2 の操作: `kubectl delete job rokuban-migrate --ignore-not-found` で前回の Job を消す。次の 2 つを順に apply し、`kubectl wait --for=condition=complete job/rokuban-migrate --timeout=300s` で完了を待つ。
  - `kustomize build base | kubectl apply -f - -l '!app.kubernetes.io/component'` で設定の ConfigMap を作る
  - `kustomize build base | kubectl apply -f - -l app.kubernetes.io/component=migrate` で migrate の Job を作る
- k8s の手順 2 で絞る理由: base には worker の ScaledJob も入っており、絞らずに apply すると完了前に worker が起きる。ConfigMap は名前に設定のハッシュが付き、component のラベルを持たない。Job だけを apply すると、新しい設定の ConfigMap が無く Job が起動しない。
- k8s の手順 4: `kustomize build base | kubectl apply -f -` と `kustomize build site | kubectl apply -f -` で、worker と api を起こす。[deploy/k8s/README.md](../../deploy/k8s/README.md) の使い方と同じ順序である。

移行後に旧 worker を動かすと、旧版の `ON CONFLICT (media_asset_id, store)` は削除済みの一意制約を推論できず、コピーを伴う公開・取り込みの commit が失敗する。`storage adopt` は既存コピーのパスや状態を同期しないので、旧 worker を止めてから adopt を再実行しても、commit に失敗したファイルは登録されない。移行前に止められなかったときは、旧 worker を止めた上で、commit に失敗したジョブと、孤児として報告されるファイルを確認する。公開の途中で commit が失敗したファイルは、DB の行が無いまま canonical 名で残る（`rokuban_orphan_files_unresolved`。[operations/monitoring.md](../operations/monitoring.md)）。


## 定期ジョブの周期とヒント

定期パスが状態の権威で、ヒントは投入を早めるだけである。落としても次の定期パスが拾う。ルールを作ってから録画が始まるまでの待ちは、EPG への取り込み・予約の導出・mirakc への schedule 作成の 3 段で、放送直前の番組は EPG の射影に無いことがある。

この原則は不変条件 5 に基づく。待ち時間は、次の表の周期でほぼ決まる。

| ジョブ | 既定周期 | 設定キー | 前倒しするヒント |
|---|---|---|---|
| `epg_sync` | 10 分 | `epg.sync_interval` | なし |
| `tuner_sync` | 10 分 | なし | なし |
| `ruler_pass` | 10 分 | なし | ルールの作成 / 更新 / 削除、`epg_sync` の完了 |
| `reconcile_pass` | 30 秒 | なし | 予約の作成 / 取消、`ruler_pass` の完了 |
| `record_sweep` | 5 分 | なし | なし |

- 5 つとも起動直後に 1 回走る設定になっている
- ヒントは、引数と状態で一意にして定期実行に合流する。ルールや予約を連続で編集してもパスは 1 回で足りる
- `worker.periodic_jobs: false`（k8s 構成）ではどのジョブも自動投入されない。CronJob から `rokuban enqueue` で投入する

`tuner_sync` はコード固定で設定キーを持たない。運用者が触る理由がないからである。ルール・意図・上書きの編集では、api が store の commit 後にヒントを投入する。投入に失敗してもログに残すだけで、定期パスが拾う。`record_sweep` は SSE の再接続を契機にする案を見送ったので、前倒しのヒントを持たない。

<details><summary>根拠</summary>

- 起動直後に 1 回走る設定: `RunOnStart`
- 定期実行に合流させる一意化: `UniqueOpts{ByArgs, ByState}`

</details>

### 手での即時実行

ジョブは手で即時実行できる。ジョブ名はハイフン区切りで、site 束縛ジョブには多サイトでは site の指定が要る。

site 束縛ジョブは多サイトでは `--site` が必須で、site 非依存ジョブには `--site` を付けない。どちらに属するかは `rokuban enqueue --help` が列挙する（[operations.md](../operations.md) §ジョブ化されたループの監視）。

```sh
docker compose exec rokuban rokuban enqueue ruler-pass --config /config.yml
# inserted job "ruler-pass" (id=42) for site "default"
# 既に待機中なら投入されず、終了コードは 0 のまま:
#   job "ruler-pass" already pending for site "default", not inserted

# catalog-export / storage-sync は --site なし（全体で 1 本）
docker compose exec rokuban rokuban enqueue catalog-export --config /config.yml
# inserted job "catalog-export" (id=43)
docker compose exec rokuban rokuban enqueue storage-sync --config /config.yml
# inserted job "storage-sync" (id=44)
```

### 録画開始までに待つもの

ルールを作ってから録画が始まるまでに待つものは 3 段ある。番組が EPG の射影に入ること、予約の導出、mirakc への schedule 作成である。

```mermaid
flowchart LR
  A["ルールを作る"] --> B["1 番組が EPG の射影に入る<br>epg_sync、最大 10 分"]
  B --> C["2 予約を導出する<br>通常は即座、ヒントが落ちれば最大 10 分"]
  C --> D["3 mirakc に schedule を作る<br>続けて走る、落ちれば最大 30 秒"]
```

1. 番組が EPG の射影に入る。`epg_sync`（最大 10 分）が担う。その手前に mirakc の `update-schedules`（既定 1 日 2 回）があるので、放送直前の番組はそもそも射影に無いことがある。ruler は射影しか見ない（不変条件 1）
2. ruler が予約の基底を導出する。ルール書き込みと同一トランザクションでヒントが入るので通常は即座である。ヒントが落ちれば最大 10 分かかる
3. reconciler が mirakc に schedule を作る。`ruler_pass` の完了がヒントなので続けて走る。落ちれば最大 30 秒かかる

<details><summary>根拠</summary>

- 2 段目の導出先: `reservations.base`

</details>
