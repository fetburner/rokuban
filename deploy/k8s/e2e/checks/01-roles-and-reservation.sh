#!/usr/bin/env bash
# 判定 1: 全ロールが上がり、番組表が見えて予約が mirakc に反映される。
#
# **この判定だけは今も部分的に緑になる**（api しか無いので）。1.1〜1.5 が
# ロールの存在と api への到達、1.6〜1.7 が「api → DB → worker → mirakc」を
# 端から端まで通す。
#
# 1.6 / 1.7 は worker が要る。worker が居ない間は TODO であって FAIL ではない
# --- **その区別がこのハーネスの存在理由**（lib/log.sh）。
set -uo pipefail

E2E_LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)"
# shellcheck source=../lib/env.sh
source "$E2E_LIB/env.sh"
# shellcheck source=../lib/log.sh
source "$E2E_LIB/log.sh"
# shellcheck source=../lib/kube.sh
source "$E2E_LIB/kube.sh"

log_section "判定 1: 全ロールが上がり、番組表が見えて予約が mirakc に反映される"

plan "1.1" "1.2" "1.3" "1.4" "1.5" "1.6" "1.7"

api_get() {
  tb_curl -H 'Host: rokuban.local' "http://rokuban-api:40773$1"
}

# assert_deployment_ready <id> <component> --- ラベルで役を引き、全レプリカが
# Ready であることを見る。
#
# **期待レプリカ数をリテラルで固定しない。** 「2 本」はマニフェスト側の判断で
# あって受け入れ基準ではない。ここが主張するのは「宣言した数だけ Ready」だけ。
assert_deployment_ready() {
  local id="$1" component="$2" name
  name="$(deployments_with_component "$component")"
  if [ -z "$name" ]; then
    todo "$id" "$component: app.kubernetes.io/component=$component の Deployment がまだ無い"
    return
  fi
  for n in $name; do
    if ! retry_until 180 "$n rollout" k rollout status "deployment/$n" --timeout=10s; then
      fail "$id" "$component ($n): rollout が完了しない --- $(k get pods -l "app.kubernetes.io/component=$component" --no-headers | tr '\n' ';')"
      return
    fi
  done
  # **母集団は name を引いたときと同じセレクタで取る。** ずれると、`$name` に
  # 無い Deployment のレプリカ数が和に入って、壊れていないのに赤くなる。
  local selector="app.kubernetes.io/name=rokuban,app.kubernetes.io/component=$component${E2E_FIXTURE_SCOPE:+,rokuban-e2e/fixture=true}"
  local desired ready
  # **読めなかったことを 0 に潰さない。** 潰すと、両方読めなかったときに
  # `0 == 0` で緑になる（bash は `$(( ))` に空文字を渡すと 0 にする）。
  if ! desired="$(k get deployments -l "$selector" -o jsonpath='{.items[*].spec.replicas}')" ||
     ! ready="$(k get deployments -l "$selector" -o jsonpath='{.items[*].status.readyReplicas}')"; then
    fail "$id" "$component ($name): レプリカ数を読めない --- 判定が成立しない"
    return
  fi
  desired="$(printf '%s' "$desired" | tr ' ' '+' | sed 's/+$//')"
  ready="$(printf '%s' "$ready" | tr ' ' '+' | sed 's/+$//')"
  if [ -z "$desired" ]; then
    fail "$id" "$component ($name): spec.replicas が空 --- 判定が成立しない"
    return
  fi
  # **0 レプリカを緑にしない。** 「宣言どおり」だけを見ると、役が 1 つも
  # 動いていない構成（replicas: 0）が「全ロールが上がり」で PASS する。
  if [ "$((desired))" -lt 1 ]; then
    fail "$id" "$component ($name): replicas が 0 --- 役が 1 つも動いていない"
    return
  fi
  assert_eq "$id" "$((desired))" "$((${ready:-0}))" "$component ($name) の Ready レプリカ数が宣言どおり"
}

assert_deployment_ready "1.1" api
assert_deployment_ready "1.2" notifier
assert_deployment_ready "1.3" watcher
assert_deployment_ready "1.4" streamer

# ---- api に届くか -------------------------------------------------------

if [ -z "$(deployments_with_component api)" ]; then
  todo "1.5" "api Deployment が無いので Service 経由の到達を見ていない"
else
  version="$(api_get /api/version 2>&1)"
  if printf '%s' "$version" | grep -q '"version"'; then
    pass "1.5" "Service 経由で api に届く"
  else
    fail "1.5" "Service 経由で api に届かない --- got: $version"
  fi
fi

# ---- 番組表と予約 -------------------------------------------------------
#
# EPG 射影を埋めるのは epg_sync（キュー `epg_<site>`）で、それを引くのは worker。
# **worker が居ないうちは TODO**。ここで FAIL にすると、残りのワークロードを
# 足していく途中で「まだ作っていない」と「作ったが壊れている」が同じ赤になる。

# **1.6 と 1.7 は別のキューを対象にする。** 1.6 は epg_sync（`epg_<site>`）、
# 1.7 は reconcile_pass（`reconciler_<site>`）を消化する側が要る。1 つの鍵で
# 両方を代表させると、epg の ScaledJob だけ先に入った状態で **1.7 が
# 「作っていないのに壊れている」（FAIL）に化ける** --- preflight_no_fixtures の
# コメントが避けるべきと名指ししている形。
# どちらのキューを 1 つの ScaledJob にまとめるかは判定側で決めない
# （不変条件 11）。同じ ScaledJob が両方に一致するなら、それはそれで通る。
epg_queue="epg_${E2E_SITE_A}"
reconciler_queue="reconciler_${E2E_SITE_A}"
epg_scaledjob="$(scaledjob_for_queue "$epg_queue")"
reconciler_scaledjob="$(scaledjob_for_queue "$reconciler_queue")"

if discovery_is_ambiguous "$epg_scaledjob"; then
  fail "1.6" "キュー ${epg_queue} に一致する ScaledJob が複数ある（$(discovery_detail "$epg_scaledjob")）--- どれを判定すべきか決まらない"
  fail "1.7" "同上"
  exit 0
fi
if discovery_is_ambiguous "$reconciler_scaledjob"; then
  fail "1.6" "キュー ${reconciler_queue} に一致する ScaledJob が複数ある（$(discovery_detail "$reconciler_scaledjob")）--- どれを判定すべきか決まらない"
  fail "1.7" "同上"
  exit 0
fi
if [ -z "$epg_scaledjob" ]; then
  todo "1.6" "番組表: キュー ${epg_queue} を引く KEDA ScaledJob がまだ無い（epg_sync を消化する worker が居ない）"
  todo "1.7" "予約の mirakc 反映: 番組表が無いので予約する番組を選べない"
  exit 0
fi
if [ -z "$reconciler_scaledjob" ]; then
  todo "1.7" "予約の mirakc 反映: キュー ${reconciler_queue} を引く KEDA ScaledJob がまだ無い（reconcile_pass を消化する worker が居ない）"
  reconciler_missing=1
fi

# **前の周回の結果を消してから測る。** クラスタは使い回す設計なので、
# EPG 射影も mirakc モックの予約も残っている。消さないと、今回 epg_sync も
# reconcile_pass も一度も走らなくても 1.6 / 1.7 が緑になる（判定が
# 「前回の残骸が見える」ことを主張するだけになる）。
log_step "clearing the EPG projection and the mock's schedules"
if ! psql_q "DELETE FROM epg_programs WHERE site = '${E2E_SITE_A}'" >/dev/null ||
   ! psql_q "DELETE FROM epg_services WHERE site = '${E2E_SITE_A}'" >/dev/null ||
   ! mock_reset "$E2E_SITE_A"; then
  # 掃除に失敗したまま測ると、前回の残骸を見て緑になる。
  fail "1.6" "測定前の掃除（EPG 射影 / モックの予約）に失敗した --- 前回の残骸を見てしまうので測らない"
  fail "1.7" "同上"
  exit 0
fi

log_step "enqueue epg-sync --site ${E2E_SITE_A}"
if ! tb_rokuban enqueue epg-sync --site "$E2E_SITE_A" >/dev/null 2>&1; then
  # ここを握ると、下のメッセージ「投入したが番組表が空」が嘘になる。
  fail "1.6" "epg-sync を投入できない（rokuban enqueue が失敗）"
  todo "1.7" "投入できていないので予約の反映を観測していない"
  exit 0
fi

programs_json=""
programs_count() {
  programs_json="$(api_get "/api/sites/${E2E_SITE_A}/programs?$(python3 -c '
import datetime, urllib.parse
now = datetime.datetime.now(datetime.timezone.utc)
print(urllib.parse.urlencode({
    "start": now.isoformat().replace("+00:00", "Z"),
    "end": (now + datetime.timedelta(days=1)).isoformat().replace("+00:00", "Z"),
}))')" 2>/dev/null)"
  printf '%s' "$programs_json" | python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
except Exception:
    print(0); sys.exit(1)
print(len(doc))
sys.exit(0 if doc else 1)
'
}

if retry_until 240 "EPG projection to fill" programs_count; then
  pass "1.6" "番組表が見える（$(programs_count) 件）"
else
  fail "1.6" "epg-sync を投入したが番組表が空のまま --- backlog=$(river_backlog "$epg_queue") jobs=$(jobs_owned_by_scaledjob "$epg_scaledjob")"
  [ -n "${reconciler_missing:-}" ] || todo "1.7" "予約の mirakc 反映: 番組表が空なので予約する番組を選べない"
  exit 0
fi

if [ -n "${reconciler_missing:-}" ]; then
  # 1.7 は上で TODO 済み。ここから先は 1.7 のためだけの手順なので打たない。
  exit 0
fi

program_id="$(printf '%s' "$programs_json" | python3 -c '
import json, sys
print(json.load(sys.stdin)[0]["programId"])
')"

log_step "PUT intent record for program ${program_id}"
if ! tb_curl -f -X PUT -H 'Host: rokuban.local' -H 'Content-Type: application/json' \
  -d '{"action":"record"}' \
  "http://rokuban-api:40773/api/sites/${E2E_SITE_A}/programs/${program_id}/intent" >/dev/null 2>&1; then
  fail "1.7" "録画意図の PUT が失敗した（api が 2xx を返さない）"
  exit 0
fi

# **この周回で PUT した番組そのものが mirakc に届いたか**を見る（件数ではなく
# programId で照合する）。件数だと、別の番組の予約が 1 件でもあれば緑になる。
schedule_arrived() {
  tb_curl -o /dev/null -w '%{http_code}' \
    "http://mirakc-${E2E_SITE_A}:40772/api/recording/schedules/${program_id}" 2>/dev/null \
    | grep -qx 200
}

if retry_until 240 "mirakc mock to receive the schedule" schedule_arrived; then
  pass "1.7" "PUT した番組 ${program_id} の予約が mirakc（モック）に反映された"
else
  fail "1.7" "予約が mirakc（モック）に届かない --- schedules=$(mock_stat "$E2E_SITE_A" schedules) reservations=$(psql_q 'SELECT count(*) FROM reservations')"
fi
exit 0
