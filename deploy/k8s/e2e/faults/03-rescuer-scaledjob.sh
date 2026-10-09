#!/usr/bin/env bash
# Fault 3: kill a worker that owns a finite-timeout job, keep only ScaledJob +
# CronJob running, and require River's JobRescuer to rescue the dead attempt.
set -uo pipefail

E2E_FAULT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$E2E_FAULT_DIR/lib.sh"

log_section "故障注入 3: ScaledJob と CronJob だけの構成での River rescuer"
plan "F3.1" "F3.2" "F3.3" "F3.4"

site="$E2E_SITE_A"
rescued_error="Stuck job rescued by JobRescuer"
# tuner_sync の Timeout()（internal/worker/tuner.go の tunerSyncTimeout）。
# rescue はこれと worker.rescue_stuck_jobs_after の長い方を待つ。
job_timeout_seconds=60
# 締切を越えてから rescue までの観測窓。本番も e2e も reconcile-pass が毎分
# Pod を起こすので、数分あれば leader の保守ループが 1 回は回る。
rescue_window_seconds=300
leader_log="$(mktemp)"
sampler_pid=""

cleanup() {
  [ -n "$sampler_pid" ] && kill "$sampler_pid" 2>/dev/null
  tb_curl -f -X DELETE "http://mirakc-${site}:40772/mock/hang/tuners" >/dev/null 2>&1 || true
  rm -f "$leader_log"
}
trap cleanup EXIT

# **この判定の前提は「常駐する River client が無い」こと。** CronJob を止めると
# Pod が起きず、常駐 worker があるとそれが leader になる。どちらも測りたい構成ではない。
suspended="$(k get cronjobs -o jsonpath='{.items[?(@.spec.suspend==true)].metadata.name}' 2>/dev/null)"
worker_deployments="$(k get deployments -o json 2>/dev/null | python3 -c '
import json, sys
doc = json.load(sys.stdin)
print(" ".join(d["metadata"]["name"] for d in doc.get("items", [])
                for c in d["spec"]["template"]["spec"]["containers"]
                if "worker" in " ".join(c.get("args", []))))
')"
if [ -n "$suspended" ] || [ -n "$worker_deployments" ]; then
  fail_from "F3.1" "ScaledJob と CronJob だけの構成ではない（suspend 中: ${suspended:-なし} / worker Deployment: ${worker_deployments:-なし}）"
  exit 0
fi
rescue_after="$(k exec "$E2E_TOOLBOX" -- cat /etc/rokuban/config.yml 2>/dev/null |
  sed -n 's/^ *rescue_stuck_jobs_after: *\([^ #]*\).*/\1/p' | head -1)"
log_step "worker.rescue_stuck_jobs_after=${rescue_after:-未設定（River 既定 1h）} / tuner_sync Timeout=${job_timeout_seconds}s"
log_step "cronjobs: $(k get cronjobs -o jsonpath='{range .items[*]}{.metadata.name}={.spec.schedule} {end}' 2>/dev/null)"

tuner_idle() {
  fault_site_kind_idle tuner_sync "$site" "epg_${site}"
}
if ! retry_until 300 "tuner_sync for ${site} to drain before the fault" tuner_idle; then
  fail_from "F3.1" "開始前の tuner_sync が消えず、今回の job を分離できない"
  exit 0
fi

# mock の /api/tuners をヘッダだけ返して止める。tuner_sync は Timeout（60 秒）まで
# running のまま掴まれ、その間に worker を殺す。
if ! tb_curl -f -X PUT "http://mirakc-${site}:40772/mock/hang/tuners" >/dev/null; then
  fail_from "F3.1" "mirakc mock の /api/tuners を止められない"
  exit 0
fi
river_job_id="$(fault_enqueue tuner-sync tuner_sync "$site")"
case "$river_job_id" in
  ''|*[!0-9]*)
    fail_from "F3.1" "tuner_sync を投入できない"
    exit 0 ;;
esac

# River の client ID は `<hostname>_<起動時刻>`（river@v0.47.0 client.go の
# defaultClientIDWithHost）。hostname は Pod 名なので、attempted_by から掴んだ Pod を引ける。
running_pod=""
job_claimed() {
  local client
  fault_job_has_state "$river_job_id" running || return 1
  client="$(psql_q "SELECT attempted_by[array_length(attempted_by, 1)] FROM river_job WHERE id = ${river_job_id}")" || return 1
  running_pod="$(printf '%s' "$client" | sed 's/_[0-9]\{4\}_[0-9][0-9]_[0-9][0-9]T.*$//')"
  [ "$(k get pod "$running_pod" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ]
}
if ! retry_until 120 "tuner_sync ${river_job_id} to be running in a KEDA worker" job_claimed; then
  fail_from "F3.1" "tuner_sync ${river_job_id} が running にならない、または掴んだ Pod を特定できない（pod=${running_pod:-empty}）"
  exit 0
fi
running_job="$(k get pod "$running_pod" -o jsonpath='{.metadata.labels.job-name}' 2>/dev/null)"
pass "F3.1" "tuner_sync ${river_job_id} を ScaledJob の Job ${running_job:-?} の Pod ${running_pod} が掴んだ"

worker_pod_ip="$(k get pod "$running_pod" -o jsonpath='{.status.podIP}' 2>/dev/null)"
worker_db_conns() {
  psql_q "SELECT count(*) FROM pg_stat_activity WHERE client_addr = '${worker_pod_ip}'" | tr -d '[:space:]'
}
if [ -z "$worker_pod_ip" ] || [ "$(worker_db_conns)" = 0 ]; then
  fail_from "F3.2" "worker pod の DB 接続を特定できず、プロセス死亡を判定できない（ip=${worker_pod_ip:-empty}）"
  exit 0
fi
log_step "force-deleting worker pod ${running_pod} (--grace-period=0 --force)"
if ! k delete pod "$running_pod" --grace-period=0 --force --wait=true >/dev/null 2>&1; then
  fail_from "F3.2" "worker pod を強制終了できない"
  exit 0
fi
worker_db_conns_gone() {
  [ "$(worker_db_conns)" = 0 ]
}
if ! retry_until 120 "killed worker's PostgreSQL connections to disappear" worker_db_conns_gone; then
  fail_from "F3.2" "pod object が消えた後も worker の DB 接続が残り、プロセス死亡を確認できない"
  exit 0
fi
killed_at="$(psql_q "SELECT now()")"
# 再試行が同じ hang に掴まらないように戻す。殺した試行には影響しない。
tb_curl -f -X DELETE "http://mirakc-${site}:40772/mock/hang/tuners" >/dev/null 2>&1 || true
if ! fault_job_has_state "$river_job_id" running; then
  fail_from "F3.2" "pod が消えた後の River job が running ではない（state=$(psql_q "SELECT state FROM river_job WHERE id = ${river_job_id}")）。死んだ試行を残せていない"
  exit 0
fi
pass "F3.2" "worker pod ${running_pod} は死に、River job ${river_job_id} は running のまま残った"

# leader の推移を DB 時刻付きで記録する。rescuer は leader の保守ループでしか
# 動かないので、rescue の時刻を含む任期の leader が回収の担い手である。
(
  while :; do
    psql_q "SELECT to_char(now(), 'HH24:MI:SS') || ' ' || COALESCE((SELECT leader_id FROM river_leader LIMIT 1), '(none)')" \
      >>"$leader_log" 2>/dev/null
    sleep 2
  done
) &
sampler_pid=$!

was_rescued() {
  [ "$(psql_q "SELECT count(*) FROM river_job j, unnest(j.errors) e
                WHERE j.id = ${river_job_id} AND e->>'error' = '${rescued_error}'" | tr -d '[:space:]')" -ge 1 ]
}
attempted_at="$(psql_q "SELECT attempted_at FROM river_job WHERE id = ${river_job_id}")"
eligible_wait="$(psql_q "SELECT GREATEST(0, ceil(extract(epoch FROM attempted_at + interval '${job_timeout_seconds} seconds' - now())))::int
                          FROM river_job WHERE id = ${river_job_id}")"
if ! retry_until "$((${eligible_wait:-0} + rescue_window_seconds))" "JobRescuer to rescue job ${river_job_id}" was_rescued; then
  kill "$sampler_pid" 2>/dev/null; sampler_pid=""
  leaders="$(awk '{print $2}' "$leader_log" | uniq | tr '\n' ' ')"
  fail_from "F3.3" "締切 + ${rescue_window_seconds}s の間に rescue されなかった（state=$(psql_q "SELECT state FROM river_job WHERE id = ${river_job_id}")、観測した leader: ${leaders:-なし}）"
  exit 0
fi
kill "$sampler_pid" 2>/dev/null; sampler_pid=""
rescued_at="$(psql_q "SELECT e->>'at' FROM river_job j, unnest(j.errors) e
                       WHERE j.id = ${river_job_id} AND e->>'error' = '${rescued_error}' LIMIT 1")"
timing="$(psql_q "SELECT round(extract(epoch FROM '${rescued_at}'::timestamptz - '${killed_at}'::timestamptz))::int || 's after kill, '
                      || round(extract(epoch FROM '${rescued_at}'::timestamptz - '${attempted_at}'::timestamptz))::int || 's after attempted_at'")"
rescued_hms="$(psql_q "SELECT to_char('${rescued_at}'::timestamptz, 'HH24:MI:SS')")"
leader_at_rescue="$(awk -v t="$rescued_hms" '$1 <= t {l = $2} END {print l}' "$leader_log")"
log_step "rescue timing: ${timing} (attempted_at=${attempted_at}, killed_at=${killed_at}, rescued_at=${rescued_at})"
log_step "leader at rescue: ${leader_at_rescue:-unknown}; leader samples (distinct): $(awk '{print $2}' "$leader_log" | uniq | tr '\n' ' ')"
pass "F3.3" "JobRescuer が job ${river_job_id} を rescue した（${timing}、leader=${leader_at_rescue:-unknown}）"

# rescue は retryable にするだけで、available に戻すのも leader の保守ループ
# （JobScheduler）である。再実行が完了するまでを回収と見なす。
if fault_wait_job_complete "$river_job_id" "rescued tuner_sync ${river_job_id} to be retried to completion"; then
  pass "F3.4" "rescue された tuner_sync ${river_job_id} は再実行されて completed になった（attempt=$(psql_q "SELECT attempt FROM river_job WHERE id = ${river_job_id}")）"
else
  fail_from "F3.4" "rescue された tuner_sync ${river_job_id} が再実行されない（state=$(psql_q "SELECT state FROM river_job WHERE id = ${river_job_id}")）"
fi
