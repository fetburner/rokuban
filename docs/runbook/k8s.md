> [runbook.md](../runbook.md) の一部。索引から辿る。

## k8s（中央 1 式）の確認手順

k8s 用のマニフェスト一式が kind で上がり、api に到達できることを人間の目で確かめる手順である。見るのは中央の 1 式までで、worker と KEDA 込みの確認は下の§受け入れ判定ハーネスに任せる。

対象のマニフェストは `deploy/k8s` である。判断の根拠は [operations.md](../operations.md) §5、マニフェスト側の注意は [deploy/k8s/README.md](../../deploy/k8s/README.md)。

この節が見るのは中央（api / notifier / streamer / migration Job / ConfigMap）である。worker は KEDA の `ScaledJob` なので、CRD の無いクラスタではその部分だけが `no matches for kind "ScaledJob"` で失敗する。Deployment 側の apply は成功するので、**ジョブを誰も消化しない構成が気づかないうちに立つ。**

ロール分割デプロイ全体の受け入れ（KEDA / watcher の二重起動 / サイト間の独立）はここでは見ない。下の§受け入れ判定ハーネスが機械判定する。

### 前提

kind・kubectl・kustomize と Docker を用意する。DB は使い捨てで立て、base に postgres は含めない。本番の DB は外にあるからである。

<details><summary>根拠</summary>

- 必要な道具: `kind` / `kubectl` / `kustomize`

</details>

### 手順

kind クラスタを作り、使い捨ての postgres を先に立ててから、イメージを載せて 1 式を適用する。postgres を先にするのは、migration Job が DB の居ない状態から始めると再試行を使い切って恒久失敗しうるからである。

```sh
kind create cluster --name rokuban-test

# 1. 使い捨ての postgres。パスワードは overlays/kind の secretGenerator と揃える
kubectl create deployment postgres --image=postgres:17
kubectl set env deployment/postgres POSTGRES_USER=rokuban POSTGRES_DB=rokuban \
  POSTGRES_PASSWORD=kindtest
kubectl expose deployment postgres --port=5432
# `kubectl wait --for=condition=ready pod` にしないこと。Pod がまだ作られていない
# 瞬間に打つと「no matching resources found」で即エラーになる（実測）。
kubectl rollout status deployment/postgres --timeout=180s

# 2. イメージをビルドして kind に載せる（overlays/kind が指すタグに合わせる。
#    :latest にすると imagePullPolicy が Always になり、ghcr の公式イメージを引く）
#    **2 本ある**: encode / thumbnail の worker は ffmpeg 入りを指す
#    （公式イメージは ffmpeg 非同梱で、その worker は起動時に fail-fast する）
docker build -t rokuban:kind-test .
docker build -f Dockerfile.full --build-arg BASE=rokuban:kind-test -t rokuban-full:kind-test .
kind load docker-image rokuban:kind-test rokuban-full:kind-test --name rokuban-test

# 3. 適用（image の差し替えは overlay の images: が行う）
#    KEDA を入れていないクラスタでは ScaledJob だけが失敗する（上記）。
kustomize build deploy/k8s/overlays/kind | kubectl apply -f -
kubectl wait --for=condition=complete job/rokuban-migrate --timeout=180s

# 4. 起動しているのが手元のイメージであることを確かめる。overlay の images: の
#    名前が typo だと kustomize はそれを警告なしに無視し、ghcr の公開イメージが
#    起動する（測っているものが手元の変更でなくなる）
kubectl get pods -l app.kubernetes.io/component=api \
  -o jsonpath='{.items[*].spec.containers[*].image}'   # rokuban:kind-test
```

migration Job が失敗した場合は、`kubectl logs job/rokuban-migrate -c migrate` でログを見る。`storage adopt` の失敗なら `-c adopt` にする。Job は完走・失敗のどちらでも 1 時間残る。`migrate up` は initContainer なので、失敗すると本体の `adopt` は起動せず、`-c` を付けないとそのログを読めない。再実行は `kubectl delete job rokuban-migrate --ignore-not-found` してから手順 3 を打つ。Job の `spec` は immutable なので、残っていると apply が `field is immutable` で落ちる。

**api Pod の `RESTARTS` が 1〜2 になるのは想定どおり。** postgres の Pod は（readiness probe を持たないので）接続を受け付ける前に Ready になる。その窓で起動した api は DB 接続に失敗して fail-fast する（crash-only 方針。[operations.md](../operations.md) §5「DB 接続失敗」）。次の再起動で繋がる。

確認するのは次の 3 点。

```sh
# migration Job が完走している
kubectl get job rokuban-migrate            # COMPLETIONS 1/1

# api が Ready で、Service の Endpoints に載っている（= readiness が通っている）
kubectl get pods -l app.kubernetes.io/component=api
kubectl get endpoints rokuban-api

# Service 経由で api に届く。curl は公式イメージに同梱してある。
# **Host を付ける**: base は allowed_hosts を締めて出荷しているので、
# Service 名で叩くと 400（invalid host）になる（実測。/healthz /readyz /metrics は
# 免除されるが /api/... は対象）
POD=$(kubectl get pod -l app.kubernetes.io/component=api -o name | head -1)
kubectl exec $POD -- curl -s -H 'Host: rokuban.local' http://rokuban-api:40773/api/version
```

<details><summary>根拠</summary>

- migration Job の再試行回数と待ち方: `backoffLimit: 3`（指数バックオフ）

</details>

### `/readyz` が DB 断で 503 になること

readiness を足した理由そのものなので、DB を実際に落として、api が Service から外れても再起動しないことを見る。

```sh
kubectl scale deployment postgres --replicas=0
POD=$(kubectl get pod -l app.kubernetes.io/component=api -o name | head -1)
kubectl exec $POD -- curl -s -w ' [%{http_code}]\n' http://localhost:40773/readyz
kubectl exec $POD -- curl -s -w ' [%{http_code}]\n' http://localhost:40773/healthz
kubectl get pods -l app.kubernetes.io/component=api
kubectl get endpoints rokuban-api
```

期待する形は次の通り（kind v0.32.0 / k8s v1.36.1 で実測。2026-08-26）。

- `/readyz` → `{"status":"database unavailable"}` の 503
- `/healthz` → `{"status":"ok"}` の 200（liveness は依存を見ない）
- Pod の `RESTARTS` は 0 のまま。**DB 断で再起動しないことがここの本題である**
- `READY 0/1` になり `Endpoints` が空になる（Service から外れる）

`kubectl scale deployment postgres --replicas=1` で戻すと、readiness が再び通って Endpoints に戻る。片付けは `kind delete cluster --name rokuban-test`。

### PDB が全レプリカ同時の退去を止めること

1 ノードの kind で drain をかけ、PodDisruptionBudget が全レプリカの同時退去を止めることを見る。

```sh
kubectl drain "$(kubectl get node -o name | head -1 | cut -d/ -f2)" \
  --ignore-daemonsets --delete-emptydir-data --timeout=60s
```

1 ノードの kind では退去できるのは 1 本だけで、2 本目は PDB に阻まれる。`Cannot evict pod as it would violate the pod's disruption budget` が出て drain がタイムアウトするのが期待する形。PDB が無いと 2 本とも退去して全断する。確認後は `kubectl uncordon <node>` で戻す。

### config を変えると rollout が起きること

config を編集すると、Pod が入れ替わる rollout が起きることを見る。rokuban は config を起動時に 1 回しか読まないので、**この rollout が起きなければ設定変更は反映されない。**

config 本体は `configMapGenerator` 経由なので、編集すると ConfigMap 名のハッシュが変わり、Pod テンプレートも変わる。

```sh
kubectl get pod -l app.kubernetes.io/component=api -o name   # 変更前の Pod 名
# deploy/k8s/base/config.yml を編集（例: log.level を debug にする）
kubectl delete job rokuban-migrate --ignore-not-found         # spec が変わるので先に消す
kustomize build deploy/k8s/overlays/kind | kubectl apply -f -
kubectl get pod -l app.kubernetes.io/component=api -o name   # Pod 名が入れ替わる
```

実測（2026-08-26）: `configmap/rokuban-config-m9bh47m2h5 created` と `deployment.apps/rokuban-api configured` が出た。api Pod は 2 本とも別名の Pod に置き換わった。

### 詰まったとき

詰まったときに疑うのは、service links の環境変数との衝突、完走した migration Job の残り、Secret の欠落や空の値である。症状から次の項目を引く。

- `cannot unmarshal string into Go struct field Config.DB of type int` で起動しない: service links の env と衝突している。`postgres` Service があると `POSTGRES_PORT=tcp://10.96.x.x:5432` が注入される。同梱の config.yml はこの名前を参照していないが、`${VAR}` を足すと再発する。Pod 側の `enableServiceLinks: false` が落ちていないか見る
- `kubectl apply` が migration Job で `field is immutable` と言う: 完走した Job がまだ残っている。`kubectl delete job rokuban-migrate` してから apply する
- Pod が `CreateContainerConfigError` になる（`secret "rokuban-secrets" not found`）: base は Secret を出荷しない。overlay の `secretGenerator` か手で作った Secret が要る。`kubectl get secret | grep rokuban-secrets` で見る
- api が `db.password is required` で落ちる: Secret はあるが値が空。`kubectl get secret <名前> -o jsonpath='{.data}'` で中身を見る

## 受け入れ判定ハーネス（kind + KEDA）

ロール分割デプロイの受け入れ 5 項目を機械判定する。上の手順が「中央 1 式が上がること」を人間の目で見るのに対して、こちらは合否が終了コードで出る。設計と判定の中身は [deploy/k8s/e2e/README.md](../../deploy/k8s/e2e/README.md)。

```sh
./deploy/k8s/e2e/run.sh              # 5 項目を判定する（クラスタごと用意する）
./deploy/k8s/e2e/run.sh --only 2,4   # 一部だけ
./deploy/k8s/e2e/run.sh --oracles    # 判定そのものを検査する（変異注入）
./deploy/k8s/e2e/run.sh --faults     # worker kill / PostgreSQL 一時断からの収束
./deploy/k8s/e2e/run.sh --down       # クラスタを消す
```

### 判定 6（置き場 2 つ）

判定 6 は、置き場を 2 つ持つ別の overlay を別の名前空間で回す。5 項目とは別の判定である。ingest・encode・転送・削除・配信を、製品の経路で実際に走らせて見る。派生物の置き場は MinIO の S3 にする。

```sh
E2E_OVERLAY=e2e-two-stores E2E_NAMESPACE=rokuban-e2e-stores ./deploy/k8s/e2e/run.sh
```

判定 6 は kind で全項目が PASS した（2026-10-31）。判定の一覧・所要時間・実行の前提は [deploy/k8s/e2e/README.md](../../deploy/k8s/e2e/README.md) が持つ。

### 終了コード

終了コードは、判定が緑か、誤動作しているか、未実装かを分ける。使い方の誤りと環境の不足は、判定を記録しない別のコードで返す。

| | 意味 |
|---|---|
| `0` | 走らせた判定がすべて緑（`--only` などで絞った場合は 0 を返さない） |
| `1` | 誤動作している判定がある |
| `2` | 誤動作はしていないが、まだ実装されていない判定がある |
| `64` | 使い方の誤り（`--only` の値が判定番号でない、未知のオプション） |
| `70` | 環境の不足・準備の失敗（道具が無い、クラスタを用意できない） |

`64` と `70` は判定を 1 つも記録せずに落ちる。「終了コードが上がっていないこと」で見るときは、この 2 つを「1 より悪い」と読まないこと。

**通常の `run.sh` の 0 は「受け入れ 5 項目を判定できた」であって「ワークロードが網羅されている」ではない**（0 が保証しないものは [deploy/k8s/e2e/README.md](../../deploy/k8s/e2e/README.md) に列挙してある）。 `--faults` の 0 は F1〜F3 の全シナリオが PASS したことを意味する。F3 だけを回す場合は `E2E_FAULTS_ONLY=03` を使う。suite ごとの対象は [deploy/k8s/e2e/README.md](../../deploy/k8s/e2e/README.md) の表が持つ。ここには書かない。判定を足す人が触るのはあちらなので、ここに写すと気づかないうちに古くなる。

### 実測と所要時間

実測で、ビルドを除くと、通常の判定は約 10 分、判定そのものの検査（変異注入）は約 40 分かかる。mirakc の確認にはモックを使い、実機では確認していない（同 README）。

全 5 判定と `--oracles` を最後に通した環境は kind v0.32.0 / k8s v1.36.1 / KEDA v2.20.2（Colima 2 CPU / 3.8 GB、2026-08-28）。そのときの結果は次のとおり。 `--only 3` の行だけ 2026-10-01 に Colima aarch64 で通した。

| コマンド | 結果 | 終了コード | 所要（実測） |
|---|---|---|---|
| `run.sh` | `PASS 25 / FAIL 0 / TODO 0` | 0 | 約 10 分（`--no-build`） |
| `run.sh --oracles` | `PASS 19 / FAIL 0` | 0 | 約 40 分（同上） |
| `run.sh --only 3` | `PASS 9 / FAIL 0 / TODO 0` | 2（部分実行のため仕様どおり） | 未計測 |

`--oracles` は長い。判定 2 は CronJob の自然な発火（分単位）を待ち、判定 3 / 5 は「起きないこと」を窓で見る。変異のたびにその待ちを通るので、短縮できない。

製品のワークロードが入ってからさらに伸びた。O1 の変異（api の Service セレクタを外す）で判定 1.6 / 1.7 が実際に走るようになったためである。到達できない api を 240 秒ずつ待つ経路が増えた（それ以前は対象が無くて即 TODO で抜けていた）。一部だけ見たいときは `E2E_ORACLES_ONLY=3`。

イメージのビルドを含めると初回はさらに数分かかる。ffmpeg 入り（`Dockerfile.full`）を焼いて `kind load` する時間で、`--no-build` を付けた 2 回目以降は上の値になる。

### CI では回さない

**決定: CI では回さない。** web の e2e と同じく、ローカルの受け入れ確認の位置づけにする。

<details><summary>根拠</summary>

- CI では回さずローカルで受け入れ確認する先例: `web/e2e/`

</details>

#### 回さない理由

回さない理由は 3 つある。遅いこと、CI に新しい依存が増えること、静的に見られる範囲は既に CI にあることである。

遅い。判定 2 は CronJob の自然な発火（分単位）を待ち、判定 3 / 5 は「起きないこと」を窓で見る。どちらも短縮できない種類の待ちである。実測で `run.sh` が約 10 分、`--oracles` が約 40 分（上の表）。

CI イメージに kind / KEDA / Postgres という新しい依存が増える。さらに判定 3 は実際に ffmpeg でエンコードを回すので、CPU も要る。

静的に見られるぶんは既に CI にある。`go test ./deploy/k8s/` が見るのは次の 4 つである。キューと ScaledJob の対応・`rokuban enqueue` の全ジョブに CronJob があること・トリガのクエリが物理キュー名であること・overlay の patch が site 名を指していること。ここが赤くなる類の退行は、クラスタを立てなくても CI で止まる。

#### 回すタイミング

変更した人が、PR を出す前に、変更の種類に応じて、通常実行・変異注入・故障注入のうち該当するものを回す。誰も回さない判定手段は無いのと同じなので、いつ誰が回すかを決めておく。

| いつ | 誰 | 何を見る |
|---|---|---|
| `deploy/k8s/` 配下を触る PR を出す前 | その PR の作者 | `run.sh` が 0 を返すこと（5 項目が緑のまま） |
| 判定・身代わり（`fixtures/`）を足す / 変えるとき | 変更した人 | `run.sh --oracles` が全部緑（判定が機能していること） |
| River recovery / `delete_reconcile` / `encode_reconcile` / DB 接続復旧 / media asset 公開、または `deploy/k8s/e2e/faults/` を変える PR を出す前 | その PR の作者 | `run.sh --faults` が 0 を返し、worker kill・PostgreSQL 接続断・rescuer の各シナリオが PASS |

#### CI が見る範囲

CI が見るのは、クラスタが要らない範囲の 3 つである。回さない判定が実態から外れていくのを、せめてそこで止める。kind 上の動的な suite は CI では回さない。

`--faults` を含む kind 上の動的 suite は CI では回さない。240 秒 fixture の encode と KEDA recovery の待ちがあり、kind / KEDA / Postgres を追加するためである。CI では故障シナリオの shellcheck と media-check Job の YAML schema を検査し、実クラスタでの実行責任は上表の PR 作者に置く。

3 つは次の通り。

- `manifests`: マニフェストが `kustomize build` + kubeconform（KEDA の CRD を含む。スキーマは `deploy/k8s/schemas/`）を通る
- `manifests`: `deploy/k8s/e2e/lib/selftest.sh` と shellcheck
- `test`: `go test ./deploy/k8s/` が overlay の `config.yml` を設定の読み込み処理に通し、**argv とキュー名の噛み合わせ**を見る。どちらも kustomize にも kubeconform にも見えない

<details><summary>根拠</summary>

- 設定の読み込み処理: `config.Load`
- 静的な検査を置いたテストファイル: `workloads_test.go`

</details>

### 詰まったとき（ハーネス）

ハーネスで詰まったときに疑うのは、判定が対象を引く手掛かりの欠落、KEDA のトリガの接続先、中断したオラクルが止めたままのワークロードである。症状から次の項目を引く。

- 判定が全部 TODO で、しかも対象を作ったはずなのに TODO のまま: 判定はラベル（`app.kubernetes.io/component=<役>`）と argv（`--sites <site>`）とキュー名への言及で対象を引く。`kubectl get deploy,scaledjob,cronjob -n rokuban-e2e` で、その 3 つのどれかに手掛かりが載っているか見る
- ScaledJob が `Ready=False / ScaledJobCheckFailed` のまま Job を起こさない: トリガの接続先を疑う。理由は [operations.md](../operations.md) §5「worker: KEDA ScaledJob」。ログは `kubectl -n keda logs deploy/keda-operator | grep -i error` で見る。直しても spec が変わらないと再 reconcile されない。ScaledJob を作り直すのが早い
- `--oracles` を中断した後、製品の Job が起きない / CronJob が止まっている: オラクル 2〜5 は製品のワークロードを止めてから回す。身代わりと同じキューと同じ mirakc モックを共有しているためである（[deploy/k8s/e2e/README.md](../../deploy/k8s/e2e/README.md)）。止めた印はクラスタ側の annotation（`rokuban-e2e/paused-by-harness`）にあり、次の `run.sh` が起動時に戻す。手で戻すなら `kubectl -n rokuban-e2e get scaledjobs,cronjobs,deployments -o json | grep paused-by-harness` で対象を出す。次に `autoscaling.keda.sh/paused` の annotation と `spec.suspend` / `spec.replicas` を戻す
- クラスタを作り直したい: `run.sh --fresh`（`--down` してから立て直す）
