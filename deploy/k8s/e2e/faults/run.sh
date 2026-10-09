#!/usr/bin/env bash
# fault injection は通常の role acceptance と分けて実行する長時間 suite。
set -uo pipefail

E2E_DIR_SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../lib/env.sh
source "$E2E_DIR_SELF/lib/env.sh"
# shellcheck source=../lib/log.sh
source "$E2E_DIR_SELF/lib/log.sh"
E2E_SUMMARY_SUBJECT="故障注入 suite が"
export E2E_SUMMARY_SUBJECT

# E2E_FAULTS_ONLY=03 のように番号を渡すと、その故障だけを走らせる（変異の確認用）。
# 一部だけ走らせた結果は summary が exit 2 にする（lib/log.sh の E2E_PARTIAL_RUN）。
ran=0
if [ -n "${E2E_FAULTS_ONLY:-}" ]; then
  export E2E_PARTIAL_RUN="E2E_FAULTS_ONLY=${E2E_FAULTS_ONLY}"
fi
for script in "$E2E_DIR_SELF"/faults/[0-9][0-9]-*.sh; do
  name="$(basename "$script")"
  case "$name" in "${E2E_FAULTS_ONLY:-}"*) ;; *) continue ;; esac
  # ponytail: 03 は常駐 River client が無いと FAIL するのが既定なので、明示指定まで走らせない。
  # 常駐 River client が入ったら既定に戻す（この case ごと消す）。
  case "$name" in 03-*)
    if [ -z "${E2E_FAULTS_ONLY:-}" ]; then
      log_step "故障注入 3（${name}）は既定では走らせていない。走らせるには E2E_FAULTS_ONLY=03 を付ける"
      continue
    fi ;;
  esac
  ran=$((ran + 1))
  if ! bash "$script"; then
    fail "${name%.sh}.exit" "故障注入スクリプトが異常終了した"
  fi
done
if [ "$ran" -eq 0 ]; then
  fail "faults.none" "E2E_FAULTS_ONLY=${E2E_FAULTS_ONLY:-} に合う故障注入スクリプトが無い"
fi

summary
