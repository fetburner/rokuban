# shellcheck shell=bash
#
# クラスタの用意と片付け。

# require_tools は必要なコマンドが揃っていることを最初に確かめる。
#
# 途中で「command not found」に落ちると、判定が FAIL なのか環境の不足なのか
# 出力から区別できない。**環境の不足は判定を 1 つも記録せずに落とす。**
require_tools() {
  local missing=()
  # rsync は `--oracles` の変異イメージビルドが要求する。**先頭で見る** ---
  # run_oracles の中で見ると、クラスタ作成・イメージビルド・KEDA 導入を全部
  # 済ませた後に落ちる。
  for cmd in kind kubectl kustomize docker go python3 rsync; do
    command -v "$cmd" >/dev/null 2>&1 || missing+=("$cmd")
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    printf 'missing required tools: %s\n' "${missing[*]}" >&2
    return 1
  fi
  if ! docker info >/dev/null 2>&1; then
    printf 'docker daemon is not reachable\n' >&2
    return 1
  fi
}

# validate_site_names は lib/env.sh のサイト名と overlays/e2e/config.yml の
# `mirakcs:` が一致していることを確かめる。
#
# 食い違うと、判定 1.6 / 4 / 5 が揃って TODO に化ける（対象を探す鍵が
# サイト名なので、「まだ実装されていない」と同じ見た目になる）。**環境の
# 不備は判定を 1 つも記録せずに落とす。**
validate_site_names() {
  local config="${E2E_ROOT}/deploy/k8s/overlays/e2e/config.yml" site
  for site in "$E2E_SITE_A" "$E2E_SITE_B"; do
    if ! grep -qE "^[[:space:]]*-[[:space:]]*site:[[:space:]]*${site}[[:space:]]*$" "$config"; then
      printf 'site %s is not declared in %s (mirakcs:)\n' "$site" "$config" >&2
      return 1
    fi
    # k8s の名前に埋めるので DNS-1123 ラベルに収まる必要がある。
    if ! printf '%s' "$site" | grep -qE '^[a-z0-9]([a-z0-9-]*[a-z0-9])?$'; then
      printf 'site %s cannot be used in k8s object names (DNS-1123)\n' "$site" >&2
      return 1
    fi
  done
}

cluster_exists() {
  kind get clusters 2>/dev/null | grep -qx "$E2E_CLUSTER"
}

cluster_create() {
  if cluster_exists; then
    log_step "kind cluster ${E2E_CLUSTER} already exists (reusing)"
    return 0
  fi
  log_step "creating kind cluster ${E2E_CLUSTER}"
  kind create cluster --name "$E2E_CLUSTER" --wait 120s
}

cluster_delete() {
  if cluster_exists; then
    log_step "deleting kind cluster ${E2E_CLUSTER}"
    kind delete cluster --name "$E2E_CLUSTER"
  fi
}

# build_images は rokuban 本体と mirakc モックのイメージを作って kind に載せる。
#
# モックは `go build` してから scratch に COPY する（Dockerfile のコメント参照）。
# **context を一時ディレクトリにする**ので、ビルド生成物がリポジトリに残らない。
# E2E_BUILD_ID は「このイメージはいつ焼いたか」の印。**Pod テンプレートに
# 載せて、焼き直したら Pod が作り直されるようにする。**
#
# タグを固定（`:e2e`）+ `imagePullPolicy: IfNotPresent` にしてあるので、
# イメージを焼き直しても Pod テンプレートは 1 バイトも変わらず、apply は
# no-op になる。kubelet はイメージを Pod 作成時にしか解決しないので、
# **クラスタを使い回すと、モックやツールボックスを直しても古いバイナリを
# 測り続ける**（`--no-build` を用意している＝使い回しが常用なので日常的に踏む）。
E2E_BUILD_ID=""

build_images() {
  log_step "building ${E2E_IMAGE}"
  docker build -t "$E2E_IMAGE" "$E2E_ROOT" >/dev/null || return 1

  log_step "building ${E2E_MOCK_IMAGE}"
  local ctx
  ctx="$(mktemp -d)" || return 1
  # クラスタのノードは linux。ホストが darwin でも GOARCH は同じなので
  # そのまま使う。
  if ! (cd "$E2E_ROOT" && CGO_ENABLED=0 GOOS=linux go build -o "$ctx/mirakcmock" ./deploy/k8s/e2e/mirakcmock) ||
     ! cp "$E2E_DIR/mirakcmock/Dockerfile" "$ctx/Dockerfile" ||
     ! docker build -t "$E2E_MOCK_IMAGE" "$ctx" >/dev/null; then
    rm -rf "$ctx"
    return 1
  fi
  rm -rf "$ctx"

  log_step "loading images into kind"
  kind load docker-image "$E2E_IMAGE" "$E2E_MOCK_IMAGE" --name "$E2E_CLUSTER" >/dev/null || return 1

  # 焼いた印。apply_template がテンプレートに差し込む。
  #
  # **読めなかったら落とす。** 空のまま連結すると `-` という定数になり、
  # 「焼き直しても Pod が作り直されない」= この仕組みが黙って無効になる。
  local app_id mock_id
  app_id="$(docker image inspect --format '{{.Id}}' "$E2E_IMAGE" 2>/dev/null | cut -c8-19)"
  mock_id="$(docker image inspect --format '{{.Id}}' "$E2E_MOCK_IMAGE" 2>/dev/null | cut -c8-19)"
  if [ -z "$app_id" ] || [ -z "$mock_id" ]; then
    printf 'could not read the built image ids\n' >&2
    return 1
  fi
  E2E_BUILD_ID="${app_id}-${mock_id}"
  export E2E_BUILD_ID
}

# install_keda は KEDA を入れる。既に入っていれば版だけ確かめて飛ばす。
install_keda() {
  if keda_installed; then
    log_step "KEDA already installed"
    return 0
  fi
  local url="https://github.com/kedacore/keda/releases/download/${E2E_KEDA_VERSION}/keda-${E2E_KEDA_VERSION#v}.yaml"
  log_step "installing KEDA ${E2E_KEDA_VERSION}"
  # **失敗を握り潰さない。** ここが失敗したまま先へ進むと、判定 2 / 3 / 5 が
  # 揃って「ScaledJob がまだ無い」になり、環境の破損が「まだ実装されていない」
  # と同じ出力になる（preflight のコメント）。
  if ! kall apply --server-side -f "$url" >/dev/null; then
    printf 'failed to install KEDA from %s\n' "$url" >&2
    return 1
  fi
  # **Deployment を名前で決め打ちしない。** 最初は
  # `keda-operator-metrics-apiserver` と書いていたが実際の名前は
  # `keda-metrics-apiserver` で、しかも失敗を握り潰していたので**存在しない
  # 名前を 1 回も気付かずに待っていた**（`|| return 1` を足して初めて出た）。
  # 上流が Deployment を増減させても追随するよう、名前空間ごと待つ。
  local dep
  for dep in $(kall -n keda get deployments -o name 2>/dev/null); do
    kall -n keda rollout status "$dep" --timeout=300s || return 1
  done
  if ! keda_installed; then
    printf 'KEDA CRDs are still missing after install\n' >&2
    return 1
  fi
}

# deploy_scaffold は名前空間・Secret・postgres・mirakc モック・ツールボックスを立てる。
deploy_scaffold() {
  log_step "creating namespace and secret"
  # **各行の失敗を伝える。** とくに e2e-keda-postgres の失敗は、後段で
  # ScaledJobCheckFailed として現れて判定 2 / 3 / 5 が原因を名指ししない赤に
  # なる --- preflight を置いた理由（環境の破損を判定結果にしない）の対象。
  kall create namespace "$E2E_NAMESPACE" --dry-run=client -o yaml | kall apply -f - >/dev/null || return 1
  k create secret generic rokuban-secrets \
    --from-literal=POSTGRES_PASSWORD="$E2E_PGPASSWORD" \
    --dry-run=client -o yaml | k apply -f - >/dev/null || return 1
  # KEDA の postgresql スケーラが読む接続文字列。
  #
  # **パスワードを含む 1 本の文字列**を別キーにしてあるのは、KEDA のトリガが
  # `connectionFromEnv` で env 1 つを読む形しか（TriggerAuthentication を
  # 使わない限り）取れないため。出どころは E2E_PGPASSWORD で 1 つのまま。
  #
  # **ホスト名は FQDN にする。** トリガの接続を張るのは Job の Pod ではなく
  # **keda 名前空間の operator** なので、短い `postgres` は operator 側の
  # search domain で解決されず `lookup postgres on 10.96.0.10:53: no such host`
  # になる（実測。KEDA v2.20.2）。このとき ScaledJob は
  # `Ready=False / ScaledJobCheckFailed` のまま Job を一度も起こさないので、
  # 症状は「KEDA が動かない」ではなく「いつまでも 0 のまま」になる。
  # 本物の ScaledJob でも同じ。
  k create secret generic e2e-keda-postgres \
    --from-literal=POSTGRES_CONNECTION_STRING="postgresql://rokuban:${E2E_PGPASSWORD}@postgres.${E2E_NAMESPACE}.svc.cluster.local:5432/rokuban?sslmode=disable" \
    --dry-run=client -o yaml | k apply -f - >/dev/null || return 1

  # ツールボックス用の素の ConfigMap。製品側はハッシュ名なので名前を固定できない。
  k create configmap e2e-toolbox-config \
    --from-file=config.yml="$E2E_ROOT/deploy/k8s/overlays/e2e/config.yml" \
    --dry-run=client -o yaml | k apply -f - >/dev/null || return 1

  # **apply_template を通す**（素の `k apply -f` にしない）。足場のイメージ名も
  # lib/env.sh の 1 か所から来る --- 直書きすると E2E_IMAGE を差し替えたときに
  # 足場だけ古い名前を引いて ImagePullBackOff になり、「唯一の出どころ」が
  # 成り立たなくなる。
  log_step "applying scaffold (postgres / mirakc mocks / toolbox)"
  # **各段の失敗を戻り値に載せる。** 関数の戻り値が最後のコマンドだけだと、
  # postgres が上がらなくても、最後のコマンドが 0 を返せば素通りする。
  apply_template "$E2E_DIR/cluster/scaffold.yaml" || return 1
  local dep
  for dep in postgres "mirakc-${E2E_SITE_A}" "mirakc-${E2E_SITE_B}" e2e-toolbox; do
    k rollout status "deployment/$dep" --timeout=300s || return 1
  done
}

# preflight は「判定を走らせられる状態か」を確かめる。**ここだけは対象が
# 無いことを TODO ではなく FAIL にする。**
#
# 理由: 判定側の探索関数はどれも「見つからない = まだ実装されていない」と
# 解釈する。そのため **KEDA の導入に失敗した / context が違う / 名前空間が
# 消えた**といった環境の破損が、いまと 1 文字も違わない出力
# （FAIL 0、TODO だけ、終了コード 2）になる。残りのワークロードは「TODO が減ること」で
# 進捗を見る運用なので、この経路は「環境が壊れた」を毎回黙って通す。
#
# 判定を 1 つも走らせる前に落とすので、preflight の失敗は必ず先頭に出る。
preflight() {
  preflight_environment || return 1
  preflight_no_fixtures || return 1
}

# preflight_environment は環境そのものを見る（オラクル自己検査でも通す）。
preflight_environment() {
  # **plan は「これから評価する 1 件」の直前に置く。** 先頭で 0.1〜0.5 を
  # まとめて宣言すると、最初の失敗で return したときに残りが「記録しなかった」
  # 扱いになり、原因 1 件に対して嘘の診断（「判定が途中で落ちた可能性」）が
  # 3 件出る。日常的に誤爆する検出器は読み飛ばされ、本物の突然死を隠す。
  plan "0.1"
  if ! kall version --request-timeout=10s >/dev/null 2>&1; then
    fail "0.1" "kind-${E2E_CLUSTER} の API サーバに届かない"
    return 1
  fi
  if ! k get namespace "$E2E_NAMESPACE" >/dev/null 2>&1; then
    fail "0.1" "名前空間 ${E2E_NAMESPACE} が無い"
    return 1
  fi
  pass "0.1" "クラスタと名前空間に届く"

  plan "0.2"
  if keda_installed; then
    pass "0.2" "KEDA の CRD がある"
  else
    fail "0.2" "KEDA の CRD が無い --- 判定 2 / 3 / 5 は「ScaledJob がまだ無い」と区別が付かなくなる"
    return 1
  fi

  # overlay が当たっていることの確認。api は base に居るので必ずある。
  plan "0.3"
  if k get deployment rokuban-api >/dev/null 2>&1; then
    pass "0.3" "製品のマニフェスト（overlays/e2e）が当たっている"
  else
    fail "0.3" "rokuban-api が無い --- overlay が当たっていないので、すべての判定が「未実装」に見える"
    return 1
  fi

  # **DB とマイグレーションを見る。** ここを見ないと、migration Job の失敗が
  # 判定 1.6 の「240 秒待った末の FAIL」として現れる。赤にはなるが、preflight が
  # 在る理由（環境の破損を判定結果として報告しない）を満たさない。
  # `deploy/k8s/README.md` は「Job が immutable で apply に失敗しても
  # Deployment の apply は成功する」を名指ししており、最も踏みやすい破損。
  plan "0.4"
  if psql_q "SELECT 1 FROM river_job LIMIT 0" >/dev/null 2>&1; then
    pass "0.4" "DB に届き、マイグレーションが当たっている"
  else
    fail "0.4" "river_job を引けない --- postgres が上がっていないか、マイグレーションが当たっていない"
    return 1
  fi
}

# preflight_no_fixtures は身代わりの残骸を落とす。**オラクル自己検査では
# 通さない**（あちらは身代わりを立てるのが仕事）。
#
# 残っていると判定 1.3 が fixture の watcher に対して緑になり（偽 PASS）、
# 判定 1.6/1.7 は fixture の ScaledJob を製品と見なして FAIL に化ける
# （作っていないものを「壊れている」と報告する）。セレクタ側で常時除外は
# しない --- それをするとオラクルが自分の身代わりを見つけられなくなる。
preflight_no_fixtures() {
  plan "0.5"
  local leftovers
  leftovers="$(k get deployments,scaledjobs,cronjobs -l 'rokuban-e2e/fixture=true' \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)"
  if [ -z "$leftovers" ]; then
    pass "0.5" "オラクル自己検査の身代わりは残っていない"
  else
    fail "0.5" "身代わりが残っている（${leftovers}）--- 判定が製品ではなくこれを見てしまう。'run.sh --oracles' が中断された可能性。消してから回すこと"
    return 1
  fi
}

# deploy_rokuban は製品のマニフェスト（overlays/e2e）を当てる。
#
# **migration Job を先に消す。** Job の spec は immutable なので、残っていると
# apply が「field is immutable」で落ちるが、**そのとき Deployment の apply は
# 成功する**（未マイグレートの DB に新しい api が乗る）。deploy/k8s/README.md。
deploy_rokuban() {
  log_step "applying deploy/k8s/overlays/e2e"
  k delete job rokuban-migrate --ignore-not-found >/dev/null
  kustomize build "$E2E_ROOT/deploy/k8s/overlays/e2e" | kall apply -f - >/dev/null || return 1
  # **マイグレーションの失敗はここで止める**（preflight 0.4 でも見るが、
  # 240 秒待たせてから判定の赤として出すより早い）。
  k wait --for=condition=complete job/rokuban-migrate --timeout=300s || return 1
  # **イメージを焼き直したなら api も作り直す。** 製品側の Pod テンプレートは
  # ハーネスが触れない（base の管轄）ので、build-id の annotation を挿す手が
  # 使えない。タグ固定 + IfNotPresent なので、これが無いとクラスタを使い回す
  # 限り古い api を測り続ける。
  if [ -n "${E2E_BUILD_ID:-}" ]; then
    # **名前で決め打ちしない**（判定側が名前を要求しないのと同じ理由）。
    # base に役が増えたとき、api だけ作り直して他は古いバイナリのまま、
    # という形になる --- 判定 4 は watcher の**バイナリ**を測る判定なので、
    # 「watcher を直したのに反映されないがハーネスは緑」になる。
    k rollout restart deployment -l app.kubernetes.io/name=rokuban >/dev/null || return 1
  fi
  # api の rollout は待つが、**失敗しても止めない**。api が上がらないこと
  # 自体が判定 1 の結果である。
  k rollout status deployment/rokuban-api --timeout=300s || true
}
