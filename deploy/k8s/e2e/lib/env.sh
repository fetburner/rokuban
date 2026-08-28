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
# ffmpeg 入り（`Dockerfile.full`）。**encode / thumbnail の ScaledJob だけが
# これを指す**（公式イメージは ffmpeg を同梱しないので、encode キューを購読する
# worker は起動時に fail-fast する）。判定 3 は実際にエンコードを走らせるので、
# ここが無いと「encode Job が起きない」で赤くなる。
E2E_FULL_IMAGE="${E2E_FULL_IMAGE:-rokuban-full:e2e}"

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

# ツールボックスは Deployment（scaffold.yaml の理由）。exec の宛先。
E2E_TOOLBOX="deploy/e2e-toolbox"

# KEDA の版は固定する。上流の最新を追うと、リポジトリ側の変更ゼロで判定の
# 前提（ScaledJob の既定挙動）が動く。**`deploy/k8s/schemas/` に置いてある
# ScaledJob の JSON スキーマも同じ版から作ってある**ので、上げるときは両方
# （と deploy/k8s/schemas/README.md の表）を揃える。
E2E_KEDA_VERSION="${E2E_KEDA_VERSION:-v2.20.2}"

# 判定 3 が「実行中の encode Job」を 1 つ作る手順（checks/03 が呼ぶ）。
#
# **既定の `insert_probe_job` では緑にならない。** あれが入れる `e2e_probe` は
# 実物の worker には未登録 kind なので、掴んだ Job は 1 回失敗して数秒で終わる
# （判定 3 は「窓の中で完走した」= TODO で抜ける）。実物のワークロードに対しては
# 実際に時間のかかるエンコードが要るので、lib/kube.sh の
# produce_real_encode_job に差し替える。
E2E_ENCODE_PRODUCER="${E2E_ENCODE_PRODUCER:-produce_real_encode_job}"

# 判定 3 が仕込む原本と、それをエンコードするプロファイル名。
#
# プロファイルは `deploy/k8s/overlays/e2e/config.yml` の `encode.profiles` に
# 定義してある（**狙って遅くしてある** --- 判定 3 は 2 つの窓ぶん走り続けて
# いることを要求する）。
E2E_ENCODE_REL_PATH="e2e/encode-probe.m2ts"
E2E_ENCODE_PROFILE="e2e-slow"
# 仕込んだ recording を後から見分けるための印。周回ごとに掃除する。
E2E_ENCODE_TITLE="e2e encode probe"

# リポジトリのルート（このファイルの 3 つ上）。
E2E_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
E2E_DIR="${E2E_ROOT}/deploy/k8s/e2e"
