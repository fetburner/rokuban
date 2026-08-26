> [runbook.md](../runbook.md) の一部。索引から辿る。

## k8s（中央 1 式）の確認手順

`deploy/k8s` の最小 1 式（config / Secret / migration Job / api Deployment +
Service）が kind で上がり、api に到達できることを見る。判断の根拠は
[operations.md](../operations.md) §5、マニフェスト側の注意は
[deploy/k8s/README.md](../../deploy/k8s/README.md)。

**ロール分割デプロイ全体の受け入れ（KEDA / watcher の二重起動 / サイト間の
独立）はここでは見ない。** 機械判定するハーネスを別に作る。

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
docker build -t rokuban:kind-test .
kind load docker-image rokuban:kind-test --name rokuban-test

# 3. 適用（image の差し替えは overlay の images: が行う）
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
