> [runbook.md](../runbook.md) の一部。索引から辿る。

## k8s（中央 1 式）の確認手順

`deploy/k8s` の 1 式が kind で上がり、api に到達できることを見る。判断の根拠は
[operations.md](../operations.md) §5、マニフェスト側の注意は
[deploy/k8s/README.md](../../deploy/k8s/README.md)。

**この節が見るのは中央までである**（api / notifier / streamer / migration Job /
ConfigMap）。worker は KEDA の `ScaledJob` なので、CRD の無いクラスタでは
その部分だけが `no matches for kind "ScaledJob"` で失敗する。**Deployment 側の
apply は成功するので、ジョブを誰も消化しない構成が黙って立つ。**
KEDA 込みの確認は下の§受け入れ判定ハーネスが自動で行う。

**ロール分割デプロイ全体の受け入れ（KEDA / watcher の二重起動 / サイト間の
独立）はここでは見ない。** 下の§受け入れ判定ハーネスが機械判定する。

### 前提

`kind` / `kubectl` / `kustomize`、Docker。DB は使い捨てで立てる（base に postgres は
含めない --- 本番の DB は外にある）。

### 手順

**postgres を先に立てる。** migration Job は `backoffLimit: 3`（指数バックオフ）で、
DB が居ない状態から始めると再試行を使い切って恒久失敗しうる。

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
#    名前が typo だと kustomize はそれを黙って無視し、ghcr の公開イメージが
#    起動する（測っているものが手元の変更でなくなる）
kubectl get pods -l app.kubernetes.io/component=api \
  -o jsonpath='{.items[*].spec.containers[*].image}'   # rokuban:kind-test
```

migration Job が失敗した場合は `kubectl logs job/rokuban-migrate` を見る
（Job は完走・失敗のどちらでも 1 時間残る）。再実行は
`kubectl delete job rokuban-migrate --ignore-not-found` してから手順 3 を打つ。
Job の `spec` は immutable なので、残っていると apply が
`field is immutable` で落ちる。

**api Pod の `RESTARTS` が 1〜2 になるのは想定どおり。** postgres の Pod は
（readiness probe を持たないので）接続を受け付ける前に Ready になる。その窓で
起動した api は DB 接続に失敗して fail-fast する（crash-only 方針。
[operations.md](../operations.md) §5「DB 接続失敗」）。次の再起動で繋がる。

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

### `/readyz` が DB 断で 503 になること

readiness を足した理由そのものなので、DB を実際に落として見る。

```sh
kubectl scale deployment postgres --replicas=0
POD=$(kubectl get pod -l app.kubernetes.io/component=api -o name | head -1)
kubectl exec $POD -- curl -s -w ' [%{http_code}]\n' http://localhost:40773/readyz
kubectl exec $POD -- curl -s -w ' [%{http_code}]\n' http://localhost:40773/healthz
kubectl get pods -l app.kubernetes.io/component=api
kubectl get endpoints rokuban-api
```

期待する形（kind v0.32.0 / k8s v1.36.1 で実測。2026-08-26）:

- `/readyz` → `{"status":"database unavailable"}` の 503
- `/healthz` → `{"status":"ok"}` の 200（liveness は依存を見ない）
- Pod の `RESTARTS` は 0 のまま。**DB 断で再起動しないことがここの本題**
- `READY 0/1` になり `Endpoints` が空になる（Service から外れる）

`kubectl scale deployment postgres --replicas=1` で戻すと、readiness が再び通って
Endpoints に戻る。片付けは `kind delete cluster --name rokuban-test`。

### PDB が全レプリカ同時の退去を止めること

```sh
kubectl drain "$(kubectl get node -o name | head -1 | cut -d/ -f2)" \
  --ignore-daemonsets --delete-emptydir-data --timeout=60s
```

1 ノードの kind では**退去できるのは 1 本だけ**で、2 本目は PDB に阻まれる。
`Cannot evict pod as it would violate the pod's disruption budget` が出て
drain がタイムアウトするのが期待する形。PDB が無いと 2 本とも退去して全断する。
確認後は `kubectl uncordon <node>` で戻す。

### config を変えると rollout が起きること

config 本体は `configMapGenerator` 経由なので、編集すると ConfigMap 名のハッシュが
変わり、Pod テンプレートも変わる。rokuban は config を起動時に 1 回しか読まないので、
**この rollout が起きなければ設定変更は効かない**。

```sh
kubectl get pod -l app.kubernetes.io/component=api -o name   # 変更前の Pod 名
# deploy/k8s/base/config.yml を編集（例: log.level を debug にする）
kubectl delete job rokuban-migrate --ignore-not-found         # spec が変わるので先に消す
kustomize build deploy/k8s/overlays/kind | kubectl apply -f -
kubectl get pod -l app.kubernetes.io/component=api -o name   # Pod 名が入れ替わる
```

実測（2026-08-26）: `configmap/rokuban-config-m9bh47m2h5 created` と
`deployment.apps/rokuban-api configured` が出た。api Pod は 2 本とも別名の Pod に
置き換わった。

### 詰まったとき

- **`cannot unmarshal string into Go struct field Config.DB of type int` で
  起動しない**: service links の env と衝突している。`postgres` Service があると
  `POSTGRES_PORT=tcp://10.96.x.x:5432` が注入される。同梱の config.yml は
  この名前を参照していないが、`${VAR}` を足すと再発する。Pod 側の
  `enableServiceLinks: false` が落ちていないか見る
- **`kubectl apply` が migration Job で `field is immutable` と言う**: 完走した Job が
  まだ残っている。`kubectl delete job rokuban-migrate` してから apply する
- **Pod が `CreateContainerConfigError` になる**（`secret "rokuban-secrets" not
  found`）: base は Secret を出荷しない。overlay の `secretGenerator` か手で
  作った Secret が要る。`kubectl get secret | grep rokuban-secrets` で見る
- **api が `db.password is required` で落ちる**: Secret はあるが値が空。
  `kubectl get secret <名前> -o jsonpath='{.data}'` で中身を見る

## 受け入れ判定ハーネス（kind + KEDA）

ロール分割デプロイの受け入れ 5 項目を機械判定する。上の手順が「中央 1 式が
上がること」を人間の目で見るのに対して、こちらは**合否が終了コードで出る**。
設計と判定の中身は [deploy/k8s/e2e/README.md](../../deploy/k8s/e2e/README.md)。

```sh
./deploy/k8s/e2e/run.sh              # 5 項目を判定する（クラスタごと用意する）
./deploy/k8s/e2e/run.sh --only 2,4   # 一部だけ
./deploy/k8s/e2e/run.sh --oracles    # 判定そのものを検査する（変異注入）
./deploy/k8s/e2e/run.sh --down       # クラスタを消す
```

終了コードは次のとおり。

| | 意味 |
|---|---|
| `0` | 走らせた判定がすべて緑（`--only` などで絞った場合は 0 を返さない） |
| `1` | 壊れている判定がある |
| `2` | 壊れてはいないが、まだ実装されていない判定がある |
| `64` | 使い方の誤り（`--only` の値が判定番号でない、未知のオプション） |
| `70` | 環境の不足・準備の失敗（道具が無い、クラスタを用意できない） |

`64` と `70` は判定を 1 つも記録せずに落ちる。「終了コードが上がっていないこと」
で見るときは、この 2 つを「1 より悪い」と読まないこと。

**0 は「受け入れ 5 項目を判定できた」であって「ワークロードが網羅されている」
ではない**（0 が保証しないものは
[deploy/k8s/e2e/README.md](../../deploy/k8s/e2e/README.md) に列挙してある）。
項目ごとの対象は
[deploy/k8s/e2e/README.md](../../deploy/k8s/e2e/README.md) の表が持つ
（ここには書かない --- 判定を足す人が触るのはあちらなので、ここに写すと
黙って古くなる）。

mirakc は実機ではなくモックで確認している（同 README）。
最後に通した環境は kind v0.32.0 / k8s v1.36.1 / KEDA v2.20.2（colima 2 CPU /
3.8 GB、2026-08-28）。そのときの結果は次のとおり。

| コマンド | 結果 | 終了コード | 所要（実測） |
|---|---|---|---|
| `run.sh` | `PASS 25 / FAIL 0 / TODO 0` | 0 | 約 10 分（`--no-build`） |
| `run.sh --oracles` | `PASS 19 / FAIL 0` | 0 | 約 40 分（同上） |

**`--oracles` は長い。** 判定 2 は CronJob の自然な発火（分単位）を待ち、
判定 3 / 5 は「起きないこと」を窓で見る。変異のたびにその待ちを通るので、
短縮が効かない。

**製品のワークロードが入ってからさらに伸びた。** O1 の変異（api の Service
セレクタを外す）で判定 1.6 / 1.7 が実際に走るようになったためである。
到達できない api を 240 秒ずつ待つ経路が増えた（それ以前は対象が無くて即
TODO で抜けていた）。一部だけ見たいときは `E2E_ORACLES_ONLY=3`。

**イメージのビルドを含めると初回はさらに数分かかる。** ffmpeg 入り
（`Dockerfile.full`）を焼いて `kind load` する時間で、`--no-build` を付けた
2 回目以降は上の値になる。

### CI では回さない

**決定: 回さない。** `web/e2e/` と同じくローカル受け入れ確認の位置づけにする。

- **遅い。** 判定 2 は CronJob の**自然な発火**（分単位）を待ち、判定 3 / 5 は
  「起きないこと」を窓で見る。どちらも短縮が効かない種類の待ちである。実測で
  `run.sh` が約 10 分、`--oracles` が約 40 分（上の表）
- **CI イメージに kind / KEDA / Postgres という新しい依存が増える。** さらに
  判定 3 は実際に ffmpeg でエンコードを回すので、CPU も要る
- **静的に見られるぶんは既に CI にある。** キューと ScaledJob の対応・
  `rokuban enqueue` の全ジョブに CronJob があること・トリガのクエリが物理
  キュー名であること・overlay の patch が site 名を指していることは
  `go test ./deploy/k8s/`（`workloads_test.go`）が見る。ここが赤くなる類の
  退行は、クラスタを立てなくても CI で止まる

**回さないなら「いつ誰が回すのか」を決めておく**（誰も回さない判定手段は無いのと
同じ）。

| いつ | 誰 | 何を見る |
|---|---|---|
| `deploy/k8s/` 配下を触る PR を出す前 | その PR の作者 | `run.sh` が **0** を返すこと（5 項目が緑のまま） |
| 判定・身代わり（`fixtures/`）を足す / 変えるとき | 変更した人 | `run.sh --oracles` が全部緑（判定が効いていること） |

CI が見るのはクラスタが要らない範囲の 3 つ。

- `manifests`: マニフェストが `kustomize build` + kubeconform（KEDA の CRD を
  含む。スキーマは `deploy/k8s/schemas/`）を通る
- `manifests`: `deploy/k8s/e2e/lib/selftest.sh` と shellcheck
- `test`: `go test ./deploy/k8s/` が overlay の `config.yml` を `config.Load` に
  通し、**argv とキュー名の噛み合わせ**（`workloads_test.go`）を見る。どちらも
  kustomize にも kubeconform にも見えない

**回さないものが腐るのを、せめてそこで止める。**

### 詰まったとき（ハーネス）

- **判定が全部 TODO で、しかも対象を作ったはずなのに TODO のまま**:
  判定はラベル（`app.kubernetes.io/component=<役>`）と argv（`--sites <site>`）と
  キュー名への言及で対象を引く。`kubectl get deploy,scaledjob,cronjob -n rokuban-e2e`
  で、その 3 つのどれかに手掛かりが載っているか見る
- **ScaledJob が `Ready=False / ScaledJobCheckFailed` のまま Job を起こさない**:
  トリガの接続先を疑う。理由は [operations.md](../operations.md) §5「worker:
  KEDA ScaledJob」。ログは
  `kubectl -n keda logs deploy/keda-operator | grep -i error` で見る。
  直しても spec が変わらないと再 reconcile されない。ScaledJob を作り直すのが早い
- **`--oracles` を中断した後、製品の Job が起きない / CronJob が止まっている**:
  オラクル 2〜5 は製品のワークロードを止めてから回す。身代わりと同じキューと
  同じ mirakc モックを共有しているためである
  （[deploy/k8s/e2e/README.md](../../deploy/k8s/e2e/README.md)）。
  止めた印はクラスタ側の annotation（`rokuban-e2e/paused-by-harness`）にあり、
  **次の `run.sh` が起動時に戻す**。手で戻すなら
  `kubectl -n rokuban-e2e get scaledjobs,cronjobs,deployments -o json | grep paused-by-harness`
  で対象を出してから、`autoscaling.keda.sh/paused` の annotation と
  `spec.suspend` / `spec.replicas` を戻す
- **クラスタを作り直したい**: `run.sh --fresh`（`--down` してから立て直す）
