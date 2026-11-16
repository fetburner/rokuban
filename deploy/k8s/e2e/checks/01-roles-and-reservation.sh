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

plan "1.1" "1.2" "1.3" "1.4" "1.5" "1.6" "1.7" "1.8"

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
      fail "$id" "$component ($n): rollout が完了しない --- $(k get pods -l "app.kubernetes.io/component=$component${E2E_FIXTURE_SCOPE:+,rokuban-e2e/fixture=true}" --no-headers)"
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

if discovery_is_unusable "$epg_scaledjob"; then
  fail "1.6" "キュー ${epg_queue} を判定対象にできない（$(discovery_detail "$epg_scaledjob")）--- 複数一致か、読み取り失敗"
  fail "1.7" "同上"
  fail "1.8" "1.7 と同じ理由で遅延を測っていない"
  exit 0
fi
# **1.7 の対象が曖昧でも 1.6 は測れる。** 鍵を分けた理由（片方の欠落で
# もう片方が巻き込まれない）を、曖昧の側でも守る。
reconciler_ambiguous=""
if discovery_is_unusable "$reconciler_scaledjob"; then
  reconciler_ambiguous="キュー ${reconciler_queue} を判定対象にできない（$(discovery_detail "$reconciler_scaledjob")）--- 複数一致か、読み取り失敗"
fi
if [ -z "$epg_scaledjob" ]; then
  todo "1.6" "番組表: キュー ${epg_queue} を引く KEDA ScaledJob がまだ無い（epg_sync を消化する worker が居ない）"
  todo "1.7" "予約の mirakc 反映: 番組表が無いので予約する番組を選べない"
  todo "1.8" "1.7 と同じ理由で遅延を測っていない"
  exit 0
fi
if [ -n "$reconciler_ambiguous" ]; then
  fail "1.7" "$reconciler_ambiguous"
  fail "1.8" "1.7 と同じ理由で遅延を測っていない"
  reconciler_missing=1
elif [ -z "$reconciler_scaledjob" ]; then
  todo "1.7" "予約の mirakc 反映: キュー ${reconciler_queue} を引く KEDA ScaledJob がまだ無い（reconcile_pass を消化する worker が居ない）"
  todo "1.8" "1.7 と同じ理由で遅延を測っていない"
  reconciler_missing=1
fi

# **前の周回の結果を消してから測る。** クラスタは使い回す設計なので、
# EPG 射影も mirakc モックの予約も残っている。消さないと、今回 epg_sync も
# reconcile_pass も一度も走らなくても 1.6 / 1.7 が緑になる（判定が
# 「前回の残骸が見える」ことを主張するだけになる）。
# **前の周回の録画意図も消す。** 番組 ID はモックが毎回同じ値で作るので、
# 意図が残っていると定期の reconcile が予約を mock へ戻してしまい、1.8 の PUT の時点で
# schedule が既にある（実測で 0〜1s になった）。消すのは api の DELETE で、
# 製品と同じく ruler へのヒントが積まれ、予約が引き取られる。
log_step "clearing the previous round's intents on ${E2E_SITE_A}"
clear_intents() {
  local pid ids
  ids="$(psql_q "SELECT program_id FROM program_intents WHERE site = '${E2E_SITE_A}'")" || return 1
  for pid in $ids; do
    tb_curl -f -X DELETE -H 'Host: rokuban.local' \
      "http://rokuban-api:40773/api/sites/${E2E_SITE_A}/programs/${pid}/intent" >/dev/null 2>&1 || return 1
  done
}
if ! clear_intents; then
  fail "1.6" "測定前の掃除（前回の録画意図）に失敗した --- 前回の残骸を見てしまうので測らない"
  fail "1.7" "同上"
  fail "1.8" "同上"
  exit 0
fi

log_step "clearing the EPG projection and the mock's schedules"
if ! psql_q "DELETE FROM epg_programs WHERE site = '${E2E_SITE_A}'" >/dev/null ||
   ! psql_q "DELETE FROM epg_services WHERE site = '${E2E_SITE_A}'" >/dev/null ||
   ! mock_reset "$E2E_SITE_A"; then
  # 掃除に失敗したまま測ると、前回の残骸を見て緑になる。
  fail "1.6" "測定前の掃除（EPG 射影 / モックの予約）に失敗した --- 前回の残骸を見てしまうので測らない"
  fail "1.7" "同上"
  fail "1.8" "1.7 と同じ理由で遅延を測っていない"
  exit 0
fi

log_step "enqueue epg-sync --site ${E2E_SITE_A}"
if ! tb_rokuban enqueue epg-sync --site "$E2E_SITE_A" >/dev/null 2>&1; then
  # ここを握ると、下のメッセージ「投入したが番組表が空」が嘘になる。
  fail "1.6" "epg-sync を投入できない（rokuban enqueue が失敗）"
  todo "1.7" "投入できていないので予約の反映を観測していない"
  todo "1.8" "1.7 と同じ理由で遅延を測っていない"
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
  if [ -z "${reconciler_missing:-}" ]; then
    todo "1.7" "予約の mirakc 反映: 番組表が空なので予約する番組を選べない"
    todo "1.8" "1.7 と同じ理由で遅延を測っていない"
  fi
  exit 0
fi

if [ -n "${reconciler_missing:-}" ]; then
  # 1.7 / 1.8 は上で記録済み。ここから先は 1.7 / 1.8 のためだけの手順なので打たない。
  exit 0
fi

# ---- 1.7 / 1.8: 予約の反映と、その遅延 -----------------------------------
#
# 1.8 は「意図の書き込み（PUT の発行）から mirakc モックが schedule を持つまで」の
# 秒数を測る。録画を保証するのは番組終了前に mirakc へ同期できた予約だけなので、
# この遅延がそのまま保証の幅になる（docs/recording/reconciler.md §起動契機）。
#
# **測るのは常駐 client の経路である。** reconciler_<site> を引く常駐 client は
# watcher Deployment に居る（deploy/k8s/site/watcher.yaml）。ScaledJob の Job が
# 同じ行を claim すると、常駐の速さを測ったことにならない。そこで 2 つを確かめる。
#   - 常駐 client が `--queues reconciler --sites <site>` で居ることを argv で見る
#     （無ければ 1.8 は FAIL。ScaledJob だけで通った速さを常駐の速さと読まない）
#   - 各 PUT の前に、reconciler の ScaledJob の Job が実行中でなく、その番組の予約が
#     まだ無いことを待つ（cold_before_put）。空の Job が claim の窓を奪わないように
#     する。待たないと次の 2 つで PUT と無関係に速く届く。
#       - CronJob の滞留で KEDA が起こした Pod が、仕事を掴めずに `--once-idle-timeout`
#         まで待っていて、ヒントを即座に claim する
#       - 前の周回の意図から作られた予約を、定期の reconcile が先に mock へ戻している
#
# 3 番組ぶん測って最大を見る。分解能はポーリングの 1 秒 + toolbox への exec ぶん。
latency_max="${E2E_RECONCILE_LATENCY_MAX:-3}"
samples=3
program_ids="$(printf '%s' "$programs_json" | python3 -c '
import json, sys
for p in json.load(sys.stdin)[:'"$samples"']:
    print(p["programId"])
')"
if [ "$(printf '%s\n' "$program_ids" | grep -c .)" -lt "$samples" ]; then
  fail "1.7" "番組表に ${samples} 件ない --- 測定に足りない"
  fail "1.8" "同上"
  exit 0
fi

# **この周回で PUT した番組そのものが mirakc に届いたか**を見る（件数ではなく
# programId で照合する）。件数だと、別の番組の予約が 1 件でもあれば緑になる。
schedule_arrived() {
  tb_curl -o /dev/null -w '%{http_code}' \
    "http://mirakc-${E2E_SITE_A}:40772/api/recording/schedules/$1" 2>/dev/null \
    | grep -qx 200
}

elapsed_all=""
max_elapsed=0
# cold_before_put <program_id> --- 測れる状態か。reconciler の ScaledJob が起こした
# Job に実行中のものが無く（空の Job が claim の窓を奪わない）、この番組の予約が
# DB にも mock にも無いこと。**予約が既にあると、PUT と無関係に届いた時刻を測ってしまう。**
cold_before_put() {
  local active n
  active="$(k get jobs -l "scaledjob.keda.sh/name=${reconciler_scaledjob}" \
    -o jsonpath='{.items[*].status.active}')" || return 1
  [ -z "$(printf '%s' "$active" | tr -d ' 0')" ] || return 1
  n="$(psql_q "SELECT count(*) FROM reservations WHERE site = '${E2E_SITE_A}' AND program_id = $1")" || return 1
  [ "$n" = "0" ] || return 1
  ! schedule_arrived "$1"
}

# 常駐 client の所在。1.8 の判定材料であって 1.7 の前提ではない（1.7 は ScaledJob
# だけでも通るので、欠けても 1.7 は進め、1.8 だけを落とす）。
resident_reconciler="$(deployments_consuming_queue reconciler "$E2E_SITE_A")"
resident_problem=""
if discovery_is_unusable "$resident_reconciler"; then
  resident_problem="常駐 client を判定対象にできない（$(discovery_detail "$resident_reconciler")）"
elif [ -z "$resident_reconciler" ]; then
  resident_problem="reconciler_${E2E_SITE_A} を --queues で引く常駐 client が無い（測っているのは ScaledJob の経路だけ）"
fi

for program_id in $program_ids; do
  # Job は `--once-idle-timeout` の 30 秒で終わり、残った予約は毎分の
  # reconcile が mock から消すので、180 秒あれば冷えた状態になる。
  if ! retry_until 180 "cold path for program ${program_id}" cold_before_put "$program_id"; then
    fail "1.7" "180 秒待っても冷えた状態にならない（${reconciler_scaledjob} の実行中 Job か、番組 ${program_id} の既存の予約が残る）--- 測らない"
    fail "1.8" "同上"
    exit 0
  fi
  log_step "PUT intent record for program ${program_id}"
  t0=$SECONDS
  if ! tb_curl -f -X PUT -H 'Host: rokuban.local' -H 'Content-Type: application/json' \
    -d '{"action":"record"}' \
    "http://rokuban-api:40773/api/sites/${E2E_SITE_A}/programs/${program_id}/intent" >/dev/null 2>&1; then
    fail "1.7" "録画意図の PUT が失敗した（api が 2xx を返さない）"
    fail "1.8" "同上"
    exit 0
  fi
  if ! retry_until 240 "mirakc mock to receive the schedule" schedule_arrived "$program_id"; then
    fail "1.7" "予約が mirakc（モック）に届かない --- schedules=$(mock_stat "$E2E_SITE_A" schedules) reservations=$(psql_q 'SELECT count(*) FROM reservations')"
    fail "1.8" "届かないので遅延を測れない"
    exit 0
  fi
  elapsed=$((SECONDS - t0))
  log_step "program ${program_id}: ${elapsed}s from PUT to the mock's schedule"
  elapsed_all="${elapsed_all:+$elapsed_all, }${elapsed}s"
  [ "$elapsed" -le "$max_elapsed" ] || max_elapsed=$elapsed
done

pass "1.7" "PUT した ${samples} 番組の予約が mirakc（モック）に反映された"
polling="$(k get scaledjob "$reconciler_scaledjob" -o jsonpath='{.spec.pollingInterval}' 2>/dev/null)"
detail="意図の PUT から mirakc の schedule まで ${elapsed_all}（最大 ${max_elapsed}s、閾値 ${latency_max}s。reconciler の ScaledJob ${reconciler_scaledjob} の pollingInterval=${polling:-?}s）"
if [ -n "$resident_problem" ]; then
  fail "1.8" "$resident_problem --- $detail"
elif [ "$max_elapsed" -le "$latency_max" ]; then
  pass "1.8" "常駐 ${resident_reconciler} 経由: $detail"
else
  fail "1.8" "常駐 ${resident_reconciler} 経由: $detail"
fi
exit 0
