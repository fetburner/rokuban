#!/usr/bin/env bash
# 判定 5: サイト B の滞留でサイト A の Job が起きない。
#
# **これが壊れると自己増殖する。** キューが共有だと、サイト A のスケーラが
# サイト B の滞留を見て A に Job を起こし、起きた Job は verifySite
# （internal/worker/worker.go）で即死し、滞留は減らないのでまた起きる。
# CPU を焼き続けるだけで、症状は「B が遅い」ではなく「A のノードが埋まる」に
# なる。
#
# **1 サイトでは原理的に測れない**ので、ハーネスは常に 2 サイトで立てる。
# 単体テスト（internal/worker/site_queue_scoping_test.go）が見ているのは
# キュー名の組み立てまでで、**KEDA のトリガのクエリが site で修飾されているか**
# はここでしか出ない。
set -uo pipefail

E2E_LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)"
# shellcheck source=../lib/env.sh
source "$E2E_LIB/env.sh"
# shellcheck source=../lib/log.sh
source "$E2E_LIB/log.sh"
# shellcheck source=../lib/kube.sh
source "$E2E_LIB/kube.sh"

log_section "判定 5: サイト B の滞留でサイト A の Job が起きない"

plan "5.1" "5.2" "5.3"

queue_a="epg_${E2E_SITE_A}"
queue_b="epg_${E2E_SITE_B}"
sj_a="$(scaledjob_for_queue "$queue_a")"
sj_b="$(scaledjob_for_queue "$queue_b")"

if discovery_is_unusable "$sj_a" || discovery_is_unusable "$sj_b"; then
  reason="判定対象の ScaledJob が一意に決まらない（${queue_a}: $(discovery_detail "${sj_a:-なし}") / ${queue_b}: $(discovery_detail "${sj_b:-なし}")）"
  fail "5.1" "$reason"
  fail "5.2" "$reason"
  fail "5.3" "$reason"
  exit 0
fi
if [ -z "$sj_a" ] || [ -z "$sj_b" ]; then
  todo "5.1" "2 サイトぶんの ScaledJob が揃っていない（${queue_a}: ${sj_a:-なし} / ${queue_b}: ${sj_b:-なし}）"
  todo "5.2" "サイト B の滞留を作れないので、A が起きないことを観測していない"
  todo "5.3" "positive control を実施していない"
  exit 0
fi
# **A と B が同じ ScaledJob なら、この判定は成立しない。** 1 本の ScaledJob が
# 両サイトのキューを見る構成は判定 5 が捕まえるべき違反そのものだが、そのまま
# 進むと「B を止めたつもりが A も止まる」ので 5.2 が緑・5.3 が赤という、原因と
# 無関係な出力になる。
if [ "$sj_a" = "$sj_b" ]; then
  fail "5.1" "サイト A と B のキューを同じ ScaledJob（${sj_a}）が見ている --- サイトごとに分かれていない"
  todo "5.2" "サイトが分かれていないので観測していない"
  todo "5.3" "同上"
  exit 0
fi

cleanup() {
  watch_jobs_stop
  # **空文字のまま rm しない。** `rm -f '' '.pre'` はカレントディレクトリの
  # `.pre` を消しにいく（5.1 で失敗して cleanup に入る経路で起きる）。
  if [ -n "${watch_file:-}" ]; then
    rm -f "$watch_file" "$watch_file.pre" "$watch_file.err"
  fi
  scaledjob_pause "$sj_b" false
  restore_cronjobs
  drain_queue "$queue_a"
  drain_queue "$queue_b"
}
watch_file=""
trap cleanup EXIT

# ---- 5.1 サイト B に滞留を作る --------------------------------------------
#
# B の消化を止めてから積む。止めずに積むと B が即座に消化してしまい、
# 「滞留があるのに A が起きなかった」ではなく「滞留が無かった」になる
# （空虚な成功）。**滞留ができたことを確かめてから次へ進む。**
#
# 同時に CronJob を全部止める。A 向けの投入が窓の中で走ると、A が**正当に**
# 起こした Job を「B の滞留で起きた」と読んでしまう（suspend_all_cronjobs の
# コメントに実測を書いてある）。

suspend_all_cronjobs
drain_queue "$queue_a"
drain_queue "$queue_b"
scaledjob_pause "$sj_b" true
insert_probe_job "$queue_b" 20

stalled() {
  [ "$(river_backlog "$queue_b")" -ge 10 ] && [ "$(river_backlog "$queue_a")" = "0" ]
}
if ! retry_until 60 "a backlog on site B" stalled; then
  fail "5.1" "サイト B に滞留を作れない（B=$(river_backlog "$queue_b") A=$(river_backlog "$queue_a")）--- 以降の判定は成立しない"
  todo "5.2" "滞留が無いので観測していない"
  todo "5.3" "positive control を実施していない"
  exit 0
fi
pass "5.1" "サイト B に滞留を作った（B=$(river_backlog "$queue_b") / A=0）"

# ---- 5.2 A が起きないこと（negative assertion）-----------------------------

# jsonpath はフィールドが無くても 0 で返る（checks/03 の同じ箇所参照）。
if ! polling="$(k get scaledjob "$sj_a" -o jsonpath='{.spec.pollingInterval}')" || [ -z "$polling" ]; then
  log_step "pollingInterval が読めない/未指定なので既定の 30s を使う"
  polling=""
fi
window=$(( 3 * ${polling:-30} ))

# **Job の「個数」で見ない。** KEDA は successfulJobsHistoryLimit を超えた Job を
# 消すので、窓の中で違反の Job が 1 本起きても同時に古い 1 本が GC されれば
# 個数は変わらず緑になり、逆に GC だけが起きれば根拠のない赤になる。
# 5.3 の positive control と**同じ観測手段**（新しく現れた Job 名）で見る。
log_step "watching ${sj_a} for ${window}s while site B is stalled"
watch_file="$(mktemp)"
watch_jobs_start "$sj_a" "$watch_file"

deadline=$((SECONDS + window))
# **A 自身の待ち行列も監視する。** 窓の中で A に何かが投入されたら、その後で
# A の Job が起きても違反ではない（正当な仕事）。判定の前提が崩れたことを
# 出力に残せるよう、窓の中で見た最大値を持っておく。
a_backlog_max=0
while [ "$SECONDS" -lt "$deadline" ]; do
  # 読めなかったサンプルは「0 だった」ではない。測定できていないので落とす。
  if ! a_now="$(river_backlog "$queue_a")" || [ -z "$a_now" ]; then
    watch_jobs_stop
    fail "5.2" "測定できない: サイト A の待ち行列を読めなかった"
    todo "5.3" "5.2 が測定できていないので positive control を実施していない"
    exit 0
  fi
  if [ "$a_now" -gt "$a_backlog_max" ]; then
    a_backlog_max="$a_now"
  fi
  if [ -n "$(observed_new_jobs "$watch_file")" ]; then
    break
  fi
  sleep 2
done
watch_jobs_stop
spawned_a="$(observed_new_jobs "$watch_file" | tr '\n' ' ' | sed 's/ *$//')"
rm -f "$watch_file" "$watch_file.pre" "$watch_file.err"
watch_file=""
if [ -n "$spawned_a" ] && [ "$a_backlog_max" != "0" ]; then
  fail "5.2" "測定できない: 窓の中でサイト A 自身にも投入があった（A の待ち行列が最大 ${a_backlog_max}）。起きた Job [${spawned_a}] は正当な仕事かもしれない"
elif [ -n "$spawned_a" ]; then
  fail "5.2" "サイト B が滞留している間にサイト A の Job が起きた [${spawned_a}] --- A の待ち行列は空のまま（${window}s 観測）"
else
  pass "5.2" "サイト B が滞留している間、サイト A の Job は起きない（${window}s 観測）"
fi

# ---- 5.3 positive control -------------------------------------------------
#
# **「起きなかった」を主張する前に、「起きたら分かる」ことを示す。** 5.2 は
# 窓を取るだけなので、ScaledJob の取り違え・ラベルの typo・KEDA の停止でも
# 黙って緑になる。同じ観測手段で A に滞留を作り、今度は起きることを見る。

watch_file="$(mktemp)"
watch_jobs_start "$sj_a" "$watch_file"
insert_probe_job "$queue_a" 1
spawned() { [ -n "$(observed_new_jobs "$watch_file" active)" ]; }
if retry_until 240 "site A to spawn a job for its own backlog" spawned; then
  pass "5.3" "positive control: サイト A 自身の滞留では Job が起きる（$(observed_new_jobs "$watch_file" active | tr '\n' ' ')）"
else
  fail "5.3" "positive control: サイト A に滞留を作っても Job が起きない --- 5.2 の緑は信用できない（backlog=$(river_backlog "$queue_a")）"
fi
watch_jobs_stop
rm -f "$watch_file" "$watch_file.pre" "$watch_file.err"
watch_file=""
exit 0
