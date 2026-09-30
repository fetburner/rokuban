#!/usr/bin/env bash
# Fault 2: remove PostgreSQL service endpoints and disconnect established clients.
set -uo pipefail

E2E_FAULT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$E2E_FAULT_DIR/lib.sh"

log_section "故障注入 2: PostgreSQL 接続の一時断"
plan "F2.1" "F2.2" "F2.3"

site="$E2E_SITE_A"
original_selector=""
postgres_service_cut=0

restore_postgres_service() {
  [ "$postgres_service_cut" = 1 ] || return 0
  if [ -z "$original_selector" ]; then
    log_step "cannot restore PostgreSQL Service: original selector was not saved"
    return 1
  fi
  if ! k patch service postgres --type=merge \
      -p "{\"spec\":{\"selector\":${original_selector}}}" >/dev/null; then
    log_step "failed to restore PostgreSQL Service selector --- run.sh must be rerun before using the cluster"
    return 1
  fi
  postgres_service_cut=0
  return 0
}

cleanup() {
  restore_postgres_service || true
  restore_cronjobs
}
trap cleanup EXIT
suspend_all_cronjobs

ready_before="$(fault_ready_code)"
if [ "$ready_before" != 200 ]; then
  fail "F2.1" "注入前から API readyz が 200 でない（got ${ready_before:-empty}）"
  fail "F2.2" "PostgreSQL 復帰後の recording 状態を確認していない"
  fail "F2.3" "PostgreSQL 復帰後の schedule reconcile を確認していない"
  exit 0
fi

log_step "creating a current reservation through the API"
program_id="$(fault_seed_reservation "$site")"
case "$program_id" in
  ''|*[!0-9]*)
    fail "F2.1" "DB outage 前に再照合する schedule を作れない"
    fail "F2.2" "PostgreSQL 復帰後の recording 状態を確認していない"
    fail "F2.3" "PostgreSQL 復帰後の schedule reconcile を確認していない"
    exit 0 ;;
esac

recording_title="e2e fault outage recording ${E2E_FAULT_RUN_ID:-manual}"
recording_id="$(fault_insert_recording "$recording_title" recording)"
case "$recording_id" in
  ''|*[!0-9]*)
    fail "F2.1" "recording fixture を作れず PostgreSQL outage を開始していない"
    fail "F2.2" "PostgreSQL 復帰後の recording 状態を確認していない"
    fail "F2.3" "PostgreSQL 復帰後の schedule reconcile を確認していない"
    exit 0 ;;
esac

if ! fault_schedule_exists "$site" "$program_id"; then
  fail "F2.1" "PostgreSQL outage の前提となる schedule が mirakc mock に無い"
  fail "F2.2" "PostgreSQL 復帰後の recording 状態を確認していない"
  fail "F2.3" "PostgreSQL 復帰後の schedule reconcile を確認していない"
  exit 0
fi

site_reconcile_idle() {
  fault_site_kind_idle reconcile_pass "$site" "reconciler_${site}"
}
if ! retry_until 240 "initial reconcile-pass to finish before the DB outage" site_reconcile_idle; then
  fail "F2.1" "DB outage 前の reconcile_pass が残り、復旧後の再照合を今回の fault から分離できない"
  fail "F2.2" "PostgreSQL 復帰後の recording 状態を確認していない"
  fail "F2.3" "PostgreSQL 復帰後の schedule reconcile を確認していない"
  exit 0
fi

original_selector="$(k get service postgres -o json 2>/dev/null | python3 -c '
import json, sys
print(json.dumps(json.load(sys.stdin)["spec"]["selector"], separators=(",", ":")))
')"
if [ -z "$original_selector" ]; then
  fail "F2.1" "postgres Service の selector を読めないため、安全に outage を注入できない"
  fail "F2.2" "PostgreSQL 復帰後の recording 状態を確認していない"
  fail "F2.3" "PostgreSQL 復帰後の schedule reconcile を確認していない"
  exit 0
fi

# Pod と emptyDir は残し、サービスディスカバリだけを外す。さらに既存 TCP セッションも
# 切ることで、単に新規接続が落ちただけで API の readiness が古い connection に隠れる
# 形を作らない。trap は途中失敗・中断でも元の selector に戻す。
postgres_service_cut=1
if ! k patch service postgres --type=merge \
    -p '{"spec":{"selector":{"app.kubernetes.io/name":"rokuban-e2e","app.kubernetes.io/component":"fault-no-postgres"}}}' >/dev/null; then
  postgres_service_cut=0
  fail "F2.1" "PostgreSQL の Service endpoint を一時的に外せない"
  fail "F2.2" "PostgreSQL 復帰後の recording 状態を確認していない"
  fail "F2.3" "PostgreSQL 復帰後の schedule reconcile を確認していない"
  exit 0
fi

postgres_endpoints_empty() {
  [ -z "$(k get endpoints postgres -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null)" ]
}
if ! retry_until 60 "postgres Service to have no endpoints" postgres_endpoints_empty ||
   ! fault_terminate_app_connections; then
  fail "F2.1" "Service endpoint を外すか既存 PostgreSQL connection を切断できない"
  fail "F2.2" "PostgreSQL 復帰後の recording 状態を確認していない"
  fail "F2.3" "PostgreSQL 復帰後の schedule reconcile を確認していない"
  exit 0
fi

api_not_ready() {
  [ "$(fault_ready_code)" = 503 ]
}
if ! retry_until 60 "API readyz to report PostgreSQL unavailable" api_not_ready; then
  fail "F2.1" "既存接続を切った後も readyz=503 を観測できず、DB 断を測れていない"
  fail "F2.2" "PostgreSQL 復帰後の recording 状態を確認していない"
  fail "F2.3" "PostgreSQL 復帰後の schedule reconcile を確認していない"
  exit 0
fi

# PostgreSQL から独立している mirakc mock の schedule を落とす。DB 復帰後に
# reconcile-pass が真実の reservations を再取得し、同じ schedule を作り直せるかを見る。
if ! mock_reset "$site" || ! fault_schedule_missing "$site" "$program_id"; then
  fail "F2.1" "DB 接続が失われている間に mirakc mock 側の schedule を失わせられない"
  fail "F2.2" "PostgreSQL 復帰後の recording 状態を確認していない"
  fail "F2.3" "PostgreSQL 復帰後の schedule reconcile を確認していない"
  exit 0
fi

if ! restore_postgres_service; then
  fail "F2.1" "PostgreSQL Service の復元に失敗した"
  fail "F2.2" "PostgreSQL 復帰後の recording 状態を確認していない"
  fail "F2.3" "PostgreSQL 復帰後の schedule reconcile を確認していない"
  exit 0
fi
postgres_endpoints_ready() {
  [ -n "$(k get endpoints postgres -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null)" ]
}
if ! retry_until 120 "postgres Service endpoint to return" postgres_endpoints_ready ||
   ! retry_until 180 "API readyz to recover after PostgreSQL returns" api_is_ready; then
  fail "F2.1" "PostgreSQL を復元した後に API readyz=200 へ戻らない"
  fail "F2.2" "PostgreSQL 復帰後の recording 状態を確認していない"
  fail "F2.3" "PostgreSQL 復帰後の schedule reconcile を確認していない"
  exit 0
fi
pass "F2.1" "DB 接続を遮断すると readyz=503、復元すると readyz=200 に戻った"

if [ "$(fault_recording_status "$recording_id")" = recording ]; then
  pass "F2.2" "一時 DB 断を跨いで recording の状態が保持された"
else
  fail "F2.2" "一時 DB 断後に recording 状態が失われた"
fi

reconcile_id="$(fault_enqueue reconcile-pass reconcile_pass "$site")"
if [ -z "$reconcile_id" ] || ! fault_wait_job_complete "$reconcile_id" "reconcile-pass after PostgreSQL outage"; then
  fail "F2.3" "PostgreSQL 復帰後の reconcile-pass が完了しない"
  exit 0
fi
if retry_until 120 "lost mirakc schedule to be recreated" fault_schedule_exists "$site" "$program_id"; then
  pass "F2.3" "DB 断中に失った schedule が level-triggered reconcile で復元された"
else
  fail "F2.3" "DB 復帰後も reservations から mirakc schedule が再生成されない"
fi
