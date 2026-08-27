#!/usr/bin/env bash
# 判定 2: worker を 0 にスケールしても CronJob が投入し続け、KEDA が Job を
# 起こして消化する（0 → 1 → 0 を実際に観測する）。
#
# **「Job が存在した」で緑にしない。** ここで見るのは遷移である。存在だけを
# 見ると、前の判定が残した Job や、KEDA と無関係に作られた Job でも緑になる。
#
# **観測を引き金より先に始める。** KEDA が起こした Job は数秒で終わりうるので、
# 「投入 → しばらくして get」では遷移が終わった後を見て取りこぼす（非同期の
# 空虚な成功。lib/kube.sh の冒頭）。watch を張ってから CronJob の発火を待つ。
set -uo pipefail

E2E_LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)"
# shellcheck source=../lib/env.sh
source "$E2E_LIB/env.sh"
# shellcheck source=../lib/log.sh
source "$E2E_LIB/log.sh"
# shellcheck source=../lib/kube.sh
source "$E2E_LIB/kube.sh"

log_section "判定 2: worker 0 でも CronJob が投入し、KEDA が起こして消化する"

plan "2.1" "2.2" "2.3" "2.4"

queue="epg_${E2E_SITE_A}"
cronjob="$(cronjob_enqueueing epg-sync "$E2E_SITE_A")"
scaledjob="$(scaledjob_for_queue "$queue")"

if discovery_is_ambiguous "$cronjob" || discovery_is_ambiguous "$scaledjob"; then
  reason="判定対象が一意に決まらない（CronJob: $(discovery_detail "${cronjob:-なし}") / ScaledJob: $(discovery_detail "${scaledjob:-なし}")）"
  fail "2.1" "$reason"
  fail "2.2" "$reason"
  fail "2.3" "$reason"
  fail "2.4" "$reason"
  exit 0
fi
if [ -z "$cronjob" ] || [ -z "$scaledjob" ]; then
  missing=""
  [ -n "$cronjob" ] || missing="'rokuban enqueue epg-sync --site ${E2E_SITE_A}' を打つ CronJob"
  [ -n "$scaledjob" ] || missing="${missing:+${missing} と }キュー ${queue} を引く KEDA ScaledJob"
  todo "2.1" "${missing} がまだ無い"
  todo "2.2" "同上（開始状態を見ていない）"
  todo "2.3" "同上（投入を観測していない）"
  todo "2.4" "同上（0 → 1 → 0 を観測していない）"
  exit 0
fi

# ---- 2.1 投入側が生きているか -------------------------------------------

# **読めなかったことを「未設定（= false）」に潰さない。** 潰すと、get が
# 失敗しただけで 2.1 が緑になる（mock_stat で空を 0 に潰さないのと同じ形）。
if suspended="$(k get cronjob "$cronjob" -o jsonpath='{.spec.suspend}' 2>/dev/null)"; then
  assert_eq "2.1" "false" "${suspended:-false}" "CronJob ${cronjob} が suspend されていない"
else
  fail "2.1" "CronJob ${cronjob} の suspend 状態を読めない --- 判定が成立しない"
fi

# ---- 2.2 worker が 0 の状態から始める ------------------------------------
#
# ScaledJob モデルでは「worker を 0 にスケールする」は特別な操作ではなく、
# **待ち行列が空なら Job が 1 つも無い**という定常状態そのものである。
# ここを確かめないと、以降の観測が「もともと動いていた worker が消化した」
# だけでも通ってしまう。

drained() {
  [ "$(river_backlog "$queue")" = "0" ] && [ -z "$(k get jobs -l "scaledjob.keda.sh/name=${scaledjob}" \
    -o jsonpath='{.items[?(@.status.active)].metadata.name}')" ]
}
if ! retry_until 180 "the queue and the worker jobs to drain" drained; then
  fail "2.2" "開始時点で worker が 0 になっていない --- backlog=$(river_backlog "$queue") jobs=$(jobs_owned_by_scaledjob "$scaledjob")"
  # **宣言した id は必ず埋める。** 埋めないと集計側が「判定が黙って死んだ」と
  # 報告し、本物の突然死と区別が付かなくなる（lib/log.sh の plan）。
  todo "2.3" "開始状態が 0 でないので投入を観測していない"
  todo "2.4" "同上（0 → 1 → 0 を観測していない）"
  exit 0
fi
pass "2.2" "開始時点で worker Job は 0（待ち行列も空）"

# ---- 2.3 / 2.4 CronJob の自然な発火から 0 → 1 → 0 まで --------------------

watch_file="$(mktemp)"
trap 'watch_jobs_stop; rm -f "$watch_file" "$watch_file.pre" "$watch_file.err"' EXIT
watch_jobs_start "$scaledjob" "$watch_file"

# **読めなかったことを既定値に潰さない。** `before_inserted` を空のまま
# `${x:-0}` で比べると、`k exec` の一過性の失敗が「0 件だった」になり、
# **CronJob の発火も投入も観測していないのに 2.3 が緑**になる（実測）。
if ! before_schedule="$(k get cronjob "$cronjob" -o jsonpath='{.status.lastScheduleTime}')"; then
  fail "2.3" "CronJob ${cronjob} の lastScheduleTime を読めない --- 発火を観測できない"
  todo "2.4" "投入を観測していないので 0 → 1 → 0 も観測していない"
  exit 0
fi
if ! before_inserted="$(psql_q "SELECT count(*) FROM river_job WHERE queue = '$queue'" | tr -d '[:space:]')" ||
   [ -z "$before_inserted" ]; then
  fail "2.3" "${queue} の現在のジョブ件数を読めない --- 投入を観測できない"
  todo "2.4" "同上"
  exit 0
fi

log_step "waiting for ${cronjob} to fire on its own schedule (up to 180s)"
cronjob_fired() {
  local now
  now="$(k get cronjob "$cronjob" -o jsonpath='{.status.lastScheduleTime}')" || return 1
  [ -n "$now" ] && [ "$now" != "$before_schedule" ]
}
if ! retry_until 180 "the CronJob to fire" cronjob_fired; then
  fail "2.3" "CronJob ${cronjob} が自分の schedule で発火しない（schedule=$(k get cronjob "$cronjob" -o jsonpath='{.spec.schedule}')）"
  todo "2.4" "投入が起きていないので 0 → 1 → 0 を観測していない"
  exit 0
fi

# **投入されたことを DB 側でも見る。** CronJob が発火しただけでは足りない
# （`rokuban enqueue` が失敗していても lastScheduleTime は進む）。
inserted() {
  local now
  now="$(psql_q "SELECT count(*) FROM river_job WHERE queue = '$queue'" | tr -d '[:space:]')" || return 1
  [ -n "$now" ] || return 1
  [ "$now" -gt "$before_inserted" ]
}
if retry_until 120 "a job to be inserted into ${queue}" inserted; then
  pass "2.3" "worker が 0 のまま CronJob が ${queue} にジョブを投入した"
else
  fail "2.3" "CronJob は発火したが ${queue} にジョブが入らない --- $(k logs "job/$(k get jobs -l "batch.kubernetes.io/cronjob-name=${cronjob}" -o jsonpath='{.items[-1:].metadata.name}')" 2>&1 | tail -3 | tr '\n' ' ')"
  todo "2.4" "投入が起きていないので 0 → 1 → 0 を観測していない"
  exit 0
fi

log_step "waiting for KEDA to spawn and drain a worker job"
# **今回起きた Job が、今回完走したこと**を見る。
#
# 「active な行がある」「succeeded な行がある」を別々に見ると、観測開始前から
# 居た Job（KEDA は完走した Job を successfulJobsHistoryLimit まで残す）や、
# 別の周回の Job で両方成立してしまう。**同じ 1 つの新しい Job が 0 → 1 → 0 を
# 通ったこと**が主張したいことなので、名前で結ぶ。
#
# backlog が 0 に戻ったことは根拠に**しない**。River の `running` は
# riverBacklogStates に入っていないので、掴んだまま固まっている Job でも 0 に
# なる --- それはこの判定が捕まえるべき失敗そのものである。
spawned_and_drained() {
  local name
  for name in $(observed_new_jobs "$watch_file" active); do
    if observed_new_job_reached "$watch_file" succeeded "$name"; then
      E2E_OBSERVED_JOB="$name"
      return 0
    fi
  done
  return 1
}
if retry_until 240 "KEDA to spawn and drain a worker job" spawned_and_drained; then
  pass "2.4" "KEDA が起こした Job ${E2E_OBSERVED_JOB} が消化して落ちた（0 → 1 → 0 を watch で観測）"
else
  fail "2.4" "0 → 1 → 0 を観測できない --- 新しく起きた Job: [$(observed_new_jobs "$watch_file" | tr '\n' ' ')] backlog=$(river_backlog "$queue") watch: $(tr '\n' ';' <"$watch_file" | tail -c 300)"
fi
exit 0
