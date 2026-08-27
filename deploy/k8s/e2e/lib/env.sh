# shellcheck shell=bash
#
# ハーネス全体で共有する名前と版。**ここが唯一の出どころ**にする。
#
# 特に E2E_PGPASSWORD は、Secret（製品 Pod が読む）と postgres の
# POSTGRES_PASSWORD と psql の PGPASSWORD の 3 か所に届く必要がある。
# 出どころを 1 つにする理由は deploy/k8s/README.md §ここを使うハーネス。

E2E_CLUSTER="${E2E_CLUSTER:-rokuban-e2e}"
E2E_NAMESPACE="${E2E_NAMESPACE:-rokuban-e2e}"

# 使い捨てクラスタの使い捨てパスワード。ここを変える理由は普通は無い。
E2E_PGPASSWORD="${E2E_PGPASSWORD:-e2etest}"

# イメージのタグ。**`:latest` にしないこと** --- latest だと
# imagePullPolicy の既定が Always になり、`kind load` で入れた手元のイメージでは
# なく ghcr の公式イメージが引かれる（k8s の既定。docs/runbook/k8s.md）。
E2E_IMAGE="${E2E_IMAGE:-rokuban:e2e}"
E2E_MOCK_IMAGE="${E2E_MOCK_IMAGE:-mirakcmock:e2e}"

# サイト名。**環境変数で上書きさせない。**
#
# ここだけ変えても `overlays/e2e/config.yml` の `mirakcs:` は追随しないので、
# 判定側（fixture の `__SITE_A__` 置換）と製品側の config が食い違い、判定 1.6 /
# 4 / 5 が揃って TODO に化ける（FAIL ではないので気付きにくい）。加えて site 名の
# 構文（internal/config）は `_` を許すが k8s の名前は許さないので、`site_a` の
# ような値にすると fixture の apply が DNS-1123 違反で落ちる。
#
# 変えるときは overlays/e2e/config.yml と一緒に変える（足場と身代わりは
# `__SITE_A__` 置換で追随する）。run.sh の validate_site_names が config.yml
# との食い違いを起動時に落とす。
E2E_SITE_A="sitea"
E2E_SITE_B="siteb"

E2E_TOOLBOX="e2e-toolbox"

# KEDA の版は固定する。上流の最新を追うと、リポジトリ側の変更ゼロで判定の
# 前提（ScaledJob の既定挙動）が動く。
E2E_KEDA_VERSION="${E2E_KEDA_VERSION:-v2.20.2}"

# リポジトリのルート（このファイルの 3 つ上）。
E2E_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
E2E_DIR="${E2E_ROOT}/deploy/k8s/e2e"
