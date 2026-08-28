#!/usr/bin/env bash
# ロール分割デプロイの受け入れ判定ハーネス（kind + KEDA）。
#
#   ./deploy/k8s/e2e/run.sh              5 項目を判定する
#   ./deploy/k8s/e2e/run.sh --only 2,4   一部だけ
#   ./deploy/k8s/e2e/run.sh --oracles    判定そのものを検査する（変異注入）
#   ./deploy/k8s/e2e/run.sh --down       クラスタを消す
#
# その他:
#   --no-build           イメージを焼き直さない（2 回目以降）
#   --fresh              クラスタを作り直してから始める
#   E2E_ORACLES_ONLY=3   --oracles で検査する判定を絞る（既定 1,2,3,4,5）
#
# 終了コード:
#   0  走らせた判定がすべて緑（--only などで絞ったときは 0 を返さない）
#   1  壊れている判定がある（FAIL）
#   2  壊れてはいないが、まだ実装されていない判定がある（TODO）
#  64  使い方の誤り（--only の値が判定番号でない、未知のオプション）
#  70  環境の不足・準備の失敗（道具が無い、クラスタを用意できない）
#
# **0 は「受け入れ 5 項目を判定できた」であって「ワークロードが網羅されている」
# ではない**（0 が保証しないものは deploy/k8s/e2e/README.md に列挙してある）。
#
# **CI では回さない。** 理由と「いつ誰が回すのか」は docs/runbook/k8s.md
# §受け入れ判定ハーネス。
set -uo pipefail

E2E_DIR_SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/env.sh
source "$E2E_DIR_SELF/lib/env.sh"

E2E_RESULTS="${E2E_RESULTS:-$(mktemp)}"
export E2E_RESULTS
: >"$E2E_RESULTS"

# shellcheck source=lib/log.sh
source "$E2E_DIR_SELF/lib/log.sh"
# shellcheck source=lib/kube.sh
source "$E2E_DIR_SELF/lib/kube.sh"
# shellcheck source=lib/cluster.sh
source "$E2E_DIR_SELF/lib/cluster.sh"

only=""
do_build=1
do_fresh=0
mode="checks"

while [ $# -gt 0 ]; do
  case "$1" in
    --only)
      # `set -u` の下で `$2` を裸で読むと、値を忘れたときに
      # "unbound variable" で死ぬ（使い方の誤りが読めないメッセージになる）。
      if [ $# -lt 2 ]; then
        printf -- '--only needs a value (e.g. --only 2,4)\n' >&2
        exit 64
      fi
      only="$2"
      # 1〜5 以外を黙って受けると、1 本も走らせずに「一部だけ緑」を返す。
      if ! printf '%s' "$only" | grep -qE '^[1-5](,[1-5])*$'; then
        printf -- '--only takes judgement numbers 1-5 (e.g. --only 2,4), got: %s\n' "$only" >&2
        exit 64
      fi
      shift 2 ;;
    --no-build) do_build=0; shift ;;
    --fresh) do_fresh=1; shift ;;
    --oracles) mode="oracles"; shift ;;
    --down) mode="down"; shift ;;
    -h|--help) sed -n '2,25p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) printf 'unknown option: %s\n' "$1" >&2; exit 64 ;;
  esac
done

require_tools || exit 70
validate_site_names || exit 70

if [ "$mode" = "down" ]; then
  cluster_delete
  exit 0
fi

if [ "$do_fresh" = "1" ]; then
  cluster_delete
fi

log_section "クラスタの用意"
cluster_create || exit 70
if [ "$do_build" = "1" ]; then
  build_images || exit 70
else
  log_step "skipping image build (--no-build)"
fi
install_keda || exit 70
deploy_scaffold || exit 70
# 前回の中断で止まったままの CronJob をここで戻す（restore_cronjobs の
# コメント。annotation で「ハーネスが止めたもの」だけを見分ける）。
restore_cronjobs
# 同じ理由で、オラクル自己検査が止めた製品のワークロード（ScaledJob / CronJob /
# watcher）も戻す。**戻さないと、次の `run.sh` は「KEDA が Job を起こさない」を
# 永久に FAIL し続ける。**
resume_product_workloads
deploy_rokuban || exit 70

if [ "$mode" = "oracles" ]; then
  # shellcheck source=oracles.sh
  source "$E2E_DIR_SELF/oracles.sh"
  run_oracles
  status=$?
  printf '\nクラスタは残してある。消すには: %s --down\n' "${BASH_SOURCE[0]}"
  exit "$status"
fi

log_section "判定の前提"
if ! preflight; then
  printf '\n前提が満たされていないので判定を走らせていない。\n'
  summary
  exit $?
fi

if [ -n "$only" ]; then
  # 「一部しか走らせていない」を summary に伝える（走らせた判定が全部緑でも
  # 0 を返さないようにするため。lib/log.sh）。
  export E2E_PARTIAL_RUN="--only $only"
fi

log_section "判定"
for script in "$E2E_DIR_SELF"/checks/*.sh; do
  n="$(basename "$script" | cut -c1-2 | sed 's/^0//')"
  if [ -n "$only" ] && ! printf '%s' ",$only," | grep -q ",$n,"; then
    continue
  fi
  # **判定スクリプトの終了コードを見る。** 記録を残さずに死んだ場合は
  # plan と summary が拾うが、記録は残したうえで途中で死んだ場合はここでしか
  # 分からない。
  if ! bash "$script"; then
    fail "$n.exit" "判定 $n のスクリプトが異常終了した（終了コード非 0）"
  fi
done

summary
status=$?
printf '\nクラスタは残してある。消すには: %s --down\n' "${BASH_SOURCE[0]}"
exit "$status"
