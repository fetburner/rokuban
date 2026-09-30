# shellcheck shell=bash
# 故障注入 suite の共有アサーション。

E2E_FAULT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_DIR="$(cd "$E2E_FAULT_DIR/.." && pwd)"
# shellcheck source=../lib/env.sh
source "$E2E_DIR/lib/env.sh"
# shellcheck source=../lib/log.sh
source "$E2E_DIR/lib/log.sh"
# shellcheck source=../lib/kube.sh
source "$E2E_DIR/lib/kube.sh"

# fault_enqueue <CLI name> <River kind> [site]
# 新しく投入された行 id を返す。既存の完了行は履歴として残し、投入時刻より
# 前にあった行を今回の成功扱いにしない。
fault_enqueue() {
  local cli_name="$1" kind="$2" site="${3:-}" before after
  before="$(psql_q "SELECT COALESCE(max(id), 0) FROM river_job WHERE kind = '${kind}'")" || return 1
  if [ -n "$site" ]; then
    tb_rokuban enqueue "$cli_name" --site "$site" >/dev/null || return 1
  else
    tb_rokuban enqueue "$cli_name" >/dev/null || return 1
  fi
  after="$(psql_q "SELECT COALESCE(max(id), 0) FROM river_job WHERE kind = '${kind}'")" || return 1
  case "$after" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ "$after" -gt "$before" ] || return 1
  printf '%s' "$after"
}

fault_job_has_state() {
  local id="$1" expected="$2" actual
  actual="$(psql_q "SELECT state FROM river_job WHERE id = ${id}")" || return 1
  [ "$actual" = "$expected" ]
}

fault_kind_idle() {
  local kind="$1"
  [ "$(psql_q "SELECT count(*) FROM river_job
                WHERE kind = '${kind}' AND state IN ('available','pending','retryable','running','scheduled')" \
      | tr -d '[:space:]')" = 0 ]
}

fault_site_kind_idle() {
  local kind="$1" site="$2" queue="$3"
  [ "$(psql_q "SELECT count(*) FROM river_job
                WHERE kind = '${kind}' AND queue = '${queue}'
                  AND args->>'site' = '${site}'
                  AND state IN ('available','pending','retryable','running','scheduled')" \
      | tr -d '[:space:]')" = 0 ]
}

fault_wait_job_complete() {
  local id="$1" description="$2"
  if retry_until 600 "$description" fault_job_has_state "$id" completed; then
    return 0
  fi
  log_step "job ${id} state=$(psql_q "SELECT state FROM river_job WHERE id = ${id}" 2>/dev/null | tr -d '[:space:]')"
  return 1
}

fault_ready_code() {
  tb_curl -o /dev/null -w '%{http_code}' http://rokuban-api:40773/readyz 2>/dev/null
}

api_is_ready() {
  [ "$(fault_ready_code)" = 200 ]
}

fault_schedule_exists() {
  local site="$1" program_id="$2"
  tb_curl -o /dev/null -w '%{http_code}' \
    "http://mirakc-${site}:40772/api/recording/schedules/${program_id}" 2>/dev/null \
    | grep -qx 200
}

fault_schedule_missing() {
  local site="$1" program_id="$2"
  tb_curl -o /dev/null -w '%{http_code}' \
    "http://mirakc-${site}:40772/api/recording/schedules/${program_id}" 2>/dev/null \
    | grep -qx 404
}

# fault_check_media_file <relative path>
# media を持つ one-shot Pod で実体の存在と非ゼロサイズを確認する。
fault_check_media_file() {
  local rel_path="$1"
  case "$rel_path" in
    ''|/*|*'|'*|*'..'*)
      log_step "refusing unsafe media path for the harness: ${rel_path}"
      return 1 ;;
  esac
  k delete job e2e-media-check --ignore-not-found >/dev/null 2>&1 || return 1
  if ! apply_template "$E2E_DIR/cluster/media-check-job.yaml" \
      -e "s|__MEDIA_CHECK_PATH__|${rel_path}|g"; then
    return 1
  fi
  if ! k wait --for=condition=complete job/e2e-media-check --timeout=120s >/dev/null 2>&1; then
    log_step "media check did not complete: $(k logs job/e2e-media-check 2>&1 | tail -3 | tr '\n' ' ')"
    return 1
  fi
  k logs job/e2e-media-check 2>/dev/null | grep -q '^SIZE=[1-9][0-9]*$'
}

# fault_insert_recording <title> <status>
# outage 中も残る録画状態を DB に作る。CI 用 mirakc mock は record stream を
# 生成しないため、このテスト fixture は watcher の結果列だけを作る。
fault_insert_recording() {
  local title="$1" recording_status="$2"
  psql_q "INSERT INTO recordings (
      source, site, network_id, service_id, event_id, service_name,
      channel_type, channel, title, program_start_at, program_duration_ms,
      status, started_at
    ) VALUES (
      'manual', '${E2E_SITE_A}', 32736, 1024, 1, 'e2e fault fixture',
      'GR', '13', '${title}', now(), 1800000, '${recording_status}', now()
    ) RETURNING id" | head -1 | tr -d '[:space:]'
}

fault_recording_status() {
  local id="$1"
  psql_q "SELECT status FROM recordings WHERE id = ${id}"
}

# fault_seed_reservation は既存の mirakc mock の EPG から今後の番組を選び、
# 正規 API で録画意図を作る。reconcile の回復を direct SQL の期待値で代用しない。
fault_seed_reservation() {
  local site="$1" programs program_id
  mock_reset "$site" || return 1
  tb_rokuban enqueue epg-sync --site "$site" >/dev/null || return 1
  if ! retry_until 240 "${site} EPG projection" fault_site_has_programs "$site"; then
    return 1
  fi
  programs="$(fault_site_programs_json "$site")" || return 1
  program_id="$(printf '%s' "$programs" | python3 -c '
import json, sys
programs = json.load(sys.stdin)
if not programs:
    raise SystemExit(1)
print(programs[0]["programId"])
')" || return 1
  if ! tb_curl -f -X PUT -H 'Host: rokuban.local' -H 'Content-Type: application/json' \
      -d '{"action":"record"}' \
      "http://rokuban-api:40773/api/sites/${site}/programs/${program_id}/intent" >/dev/null; then
    return 1
  fi
  if ! retry_until 240 "${site} schedule ${program_id}" fault_schedule_exists "$site" "$program_id"; then
    return 1
  fi
  printf '%s' "$program_id"
}

fault_site_has_programs() {
  local site="$1" programs
  programs="$(fault_site_programs_json "$site")" || return 1
  printf '%s' "$programs" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin) else 1)'
}

fault_site_programs_json() {
  local site="$1" start end
  start="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  end="$(date -u -v+1d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+1 day' +%Y-%m-%dT%H:%M:%SZ)"
  tb_curl -f -H 'Host: rokuban.local' \
    "http://rokuban-api:40773/api/sites/${site}/programs?start=${start}&end=${end}"
}

# PostgreSQL が認証するアプリ接続を切る。Service を外した後に呼ぶので、既存
# pool connection も切断され、readyz が 503 になることを観測できる。
fault_terminate_app_connections() {
  psql_q "SELECT pg_terminate_backend(pid)
           FROM pg_stat_activity
           WHERE datname = 'rokuban' AND usename = 'rokuban'
             AND pid <> pg_backend_pid()" >/dev/null
}
