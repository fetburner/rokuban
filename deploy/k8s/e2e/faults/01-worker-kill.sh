#!/usr/bin/env bash
# Fault 1: kill a worker while it owns a long encode, then require durable recovery.
set -uo pipefail

E2E_FAULT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$E2E_FAULT_DIR/lib.sh"

log_section "故障注入 1: 実行中 worker の強制終了"
plan "F1.1" "F1.2" "F1.3" "F1.4"

title="e2e fault worker kill ${E2E_FAULT_RUN_ID:-manual}"
E2E_ENCODE_REL_PATH="e2e/fault-worker-${E2E_FAULT_RUN_ID:-manual}.m2ts"
encoded_rel_path="${E2E_ENCODE_REL_PATH%.m2ts}_e2e-slow.mp4"
profile="e2e-slow"
scaledjob="$(scaledjob_for_queue encode)"

cleanup() {
  restore_cronjobs
}
trap cleanup EXIT
suspend_all_cronjobs

if [ -z "$scaledjob" ] || discovery_is_unusable "$scaledjob"; then
  fail_from "F1.1" "encode worker を一意に特定できない（$(discovery_detail "$scaledjob")）"
  exit 0
fi

# 前周回や CronJob が残したキュー行を今回の証拠にしない。既存の仕事は捨てず、
# なくなるまで待つ。新しいテスト行だけを worker kill で対象にする。
encode_queue_idle() {
  [ "$(psql_q "SELECT count(*) FROM river_job WHERE queue = 'encode' AND state IN ('available','pending','retryable','running','scheduled')" | tr -d '[:space:]')" = "0" ]
}
supporting_jobs_idle() {
  fault_kind_idle delete_reconcile && fault_kind_idle encode_reconcile
}
if ! retry_until 300 "encode queue to drain before the fault" encode_queue_idle ||
   ! retry_until 300 "encode_reconcile and delete_reconcile to drain before the fault" supporting_jobs_idle; then
  fail_from "F1.1" "開始前の encode backlog が消えず、別の Job を worker kill の対象から分離できない"
  exit 0
fi

log_step "seeding a unique 240-second encode source"
size="$(e2e_seed_media_file)" || size=""
if [ -z "$size" ] || [ "$size" = "0" ]; then
  fail_from "F1.1" "encode source を media volume に作れない"
  exit 0
fi

recording_id="$(fault_insert_recording "$title" finished 240000 "now()")"
case "$recording_id" in
  ''|*[!0-9]*)
    fail_from "F1.1" "録画 fixture の id を取得できない"
    exit 0 ;;
esac

if ! psql_q "INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes, state)
              VALUES (${recording_id}, 'original', '${E2E_ENCODE_REL_PATH}', ${size}, 'active')" >/dev/null ||
   ! psql_q "INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes, state)
              VALUES (${recording_id}, 'thumbnail', '${E2E_ENCODE_REL_PATH%.m2ts}_thumb.jpg', 1, 'active'),
                     (${recording_id}, 'seek_tiles', '${E2E_ENCODE_REL_PATH%.m2ts}_tiles.jpg', 1, 'active')" >/dev/null ||
   ! psql_q "INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles)
              VALUES (${recording_id}, 'until_encoded', ARRAY['${profile}'])" >/dev/null ||
   ! psql_q "INSERT INTO river_job (state, queue, kind, args, max_attempts, priority, scheduled_at)
              VALUES ('available', 'encode', 'encode',
                jsonb_build_object('recording_id', ${recording_id}::bigint, 'profile', '${profile}'),
                25, 1, now())" >/dev/null ||
   ! fault_check_media_file "$E2E_ENCODE_REL_PATH"; then
  fail_from "F1.1" "original / policy / encode job の fixture が揃わない"
  exit 0
fi

river_job_id="$(psql_q "SELECT max(id) FROM river_job
                        WHERE kind = 'encode' AND args->>'recording_id' = '${recording_id}'")"
if [ -z "$river_job_id" ]; then
  fail_from "F1.1" "今回投入した River encode job を引けない"
  exit 0
fi

running_job=""
running_pod=""
encode_claimed() {
  local state active jobs pods
  state="$(psql_q "SELECT state FROM river_job WHERE id = ${river_job_id}")" || return 1
  [ "$state" = running ] || return 1
  jobs="$(k get jobs -l "scaledjob.keda.sh/name=${scaledjob}" \
    -o jsonpath='{.items[?(@.status.active)].metadata.name}' 2>/dev/null)" || return 1
  [ "$(printf '%s\n' "$jobs" | awk 'NF {n++} END {print n+0}')" = 1 ] || return 1
  running_job="$jobs"
  active="$(k get job "$running_job" -o jsonpath='{.status.active}' 2>/dev/null)" || return 1
  [ "${active:-0}" -ge 1 ] 2>/dev/null || return 1
  pods="$(k get pods -l "job-name=${running_job}" \
    -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' 2>/dev/null)" || return 1
  [ "$(printf '%s\n' "$pods" | awk 'NF {n++} END {print n+0}')" = 1 ] || return 1
  running_pod="$pods"
}

if ! retry_until 300 "the encode job to be running in one KEDA worker" encode_claimed; then
  fail_from "F1.1" "River job が running にならず、worker kill を注入できない"
  exit 0
fi
pass "F1.1" "期待した encode job ${river_job_id} を worker Job ${running_job} が claim した"

worker_pod_ip="$(k get pod "$running_pod" -o jsonpath='{.status.podIP}' 2>/dev/null)"
worker_db_conns() {
  psql_q "SELECT count(*) FROM pg_stat_activity WHERE client_addr = '${worker_pod_ip}'" | tr -d '[:space:]'
}
if [ -z "$worker_pod_ip" ] || [ "$(worker_db_conns)" = 0 ]; then
  fail_from "F1.2" "worker pod の DB 接続を特定できず、プロセス死亡を判定できない（ip=${worker_pod_ip:-empty}）"
  exit 0
fi
log_step "force-deleting worker pod ${running_pod} (--grace-period=0 --force)"
if ! k delete pod "$running_pod" --grace-period=0 --force --wait=true >/dev/null 2>&1; then
  fail_from "F1.2" "worker pod を強制終了できない"
  exit 0
fi
# --force の delete は API オブジェクトが消えた時点で戻り、コンテナ終了は待たない。
# worker の DB 接続（lock heartbeat を含む）が全部消えたことで死亡を確認する。
worker_db_conns_gone() {
  [ "$(worker_db_conns)" = 0 ]
}
if ! retry_until 120 "killed worker's PostgreSQL connections to disappear" worker_db_conns_gone; then
  fail_from "F1.2" "pod object が消えた後も worker の DB 接続が残り、プロセス死亡を確認できない"
  exit 0
fi
worker_job_still_running() {
  fault_job_has_state "$river_job_id" running
}
if ! retry_until 30 "killed worker leaves its River job running" worker_job_still_running; then
  fail_from "F1.2" "pod が消えた後の River job が running のままではない。別の recovery が先に作用したため測定できない"
  exit 0
fi
pass "F1.2" "worker pod ${running_pod} は force delete され、DB 接続が消えた後も River の running job が残った"

# encoded が未公開の間に cleanup pass を実行し、until_encoded が original を
# 保護することを確認する。ビュー until_encoded_deletable_originals は有効な
# thumbnail / seek_tiles も要求するので fixture に入れてあり、original を守る
# のは「エンコード済みプロファイルが無い」ことだけである。
delete_job_id="$(fault_enqueue delete-reconcile delete_reconcile)"
if [ -z "$delete_job_id" ] || ! fault_wait_job_complete "$delete_job_id" "delete_reconcile before encode recovery"; then
  fail_from "F1.3" "故障中の delete_reconcile を完了できず、削除安全弁を判定できない"
  exit 0
fi
original_still_active() {
  [ "$(psql_q "SELECT state FROM media_assets
                WHERE recording_id = ${recording_id} AND kind = 'original'")" = active ]
}
if original_still_active && fault_check_media_file "$E2E_ENCODE_REL_PATH"; then
  pass "F1.3" "encoded asset が無い間、delete_reconcile は until_encoded original を残した"
else
  fail_from "F1.3" "encoded asset が無いのに original が削除されたか、実体を確認できない"
  exit 0
fi

encode_attempt_is_stale() {
  [ "$(psql_q "SELECT count(*) FROM river_job
                WHERE id = ${river_job_id} AND state = 'running'
                  AND attempted_at < now() - interval '61 seconds'" | tr -d '[:space:]')" = 1 ]
}
if ! retry_until 180 "killed encode attempt to become eligible for recovery" encode_attempt_is_stale; then
  fail_from "F1.4" "attempted_at が recovery 閾値を越えず、encode_reconcile を試せない"
  exit 0
fi

reconcile_id="$(fault_enqueue encode-reconcile encode_reconcile)"
if [ -z "$reconcile_id" ] || ! fault_wait_job_complete "$reconcile_id" "encode_reconcile after worker kill"; then
  fail_from "F1.4" "encode_reconcile が完了せず、stale worker の回収を確認できない"
  exit 0
fi

recovered() {
  [ "$(psql_q "SELECT count(*) FROM river_job
                WHERE id = ${river_job_id} AND state = 'discarded'
                  AND metadata->'encode_recovery'->>'reason' = 'encode process death detected: job advisory lock was not held'")" = 1 ] &&
  [ "$(psql_q "SELECT count(*) FROM river_job
                WHERE kind = 'encode' AND args->>'recording_id' = '${recording_id}'
                  AND id > ${river_job_id} AND state IN ('available','pending','retryable','running','completed')")" -ge 1 ]
}
if ! retry_until 30 "stale River job discarded and replacement inserted" recovered; then
  fail_from "F1.4" "encode_reconcile は killed worker の River job を discarded + replacement にしない"
  exit 0
fi

encoded_asset_active() {
  [ "$(psql_q "SELECT count(*) FROM media_assets
                WHERE recording_id = ${recording_id} AND kind = 'encoded'
                  AND profile = '${profile}' AND state = 'active' AND size_bytes > 0" | tr -d '[:space:]')" = 1 ]
}
if ! retry_until 900 "replacement encode to publish its asset" encoded_asset_active; then
  fail_from "F1.4" "recovered encode job が encoded media_asset を公開しない --- River=$(psql_q "SELECT state FROM river_job WHERE kind='encode' AND args->>'recording_id'='${recording_id}' ORDER BY id DESC LIMIT 1")"
  exit 0
fi

recording_api_has_encoded_asset() {
  local body
  body="$(tb_curl -f -H 'Host: rokuban.local' \
    "http://rokuban-api:40773/api/recordings/${recording_id}")" || return 1
  printf '%s' "$body" | python3 -c '
import json, sys
doc = json.load(sys.stdin)
assets = doc.get("encodedAssets", [])
sys.exit(0 if any(a.get("profile") == "e2e-slow" and a.get("sizeBytes", 0) > 0 for a in assets) else 1)
'
}
if fault_check_media_file "$encoded_rel_path" && recording_api_has_encoded_asset; then
  pass "F1.4" "replacement encode が media volume と GET /api/recordings/${recording_id} の両方に asset を公開した"
else
  fail_from "F1.4" "encoded row はあるが、実ファイルまたは録画詳細 API から確認できない"
  exit 0
fi
