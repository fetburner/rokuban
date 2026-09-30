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

for script in "$E2E_DIR_SELF"/faults/[0-9][0-9]-*.sh; do
  if ! bash "$script"; then
    fail "$(basename "$script" .sh).exit" "故障注入スクリプトが異常終了した"
  fi
done

summary
