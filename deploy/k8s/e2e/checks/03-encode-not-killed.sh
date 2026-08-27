#!/usr/bin/env bash
# 判定 3: 実行中の encode Job がスケールインで殺されない。
#
# 「スケールイン」は 2 つの経路で起きる。**両方見る。**
#   3.2 待ち行列が空になったとき（KEDA が新しい Job を起こさなくなる側）
#   3.3 ScaledJob そのものが更新されたとき（KEDA の `rollout.strategy` 次第。
#       値ごとの実測は docs/operations.md §5「worker: KEDA ScaledJob」）
#
# **これは negative assertion（殺されないこと）である。** 窓を取るしかないので、
# 同じ観測手段で「殺されたら分かる」ことを 3.4 で必ず示す（positive control）。
# それが無いと、観測が壊れていても緑になる。
#
# 実行中の encode Job を作る手順は **本物の encode ワークロードに依存する**
# ので、`E2E_ENCODE_PRODUCER` で差し替えられるようにしてある。既定は
# River の encode キューに 1 行入れるだけ（オラクル自己検査の fixture は
# これで長時間 Job になる）。実物の encode ワークロードでは、実際に時間の
# かかるエンコードを投入するコマンドに差し替えること。
set -uo pipefail

E2E_LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)"
# shellcheck source=../lib/env.sh
source "$E2E_LIB/env.sh"
# shellcheck source=../lib/log.sh
source "$E2E_LIB/log.sh"
# shellcheck source=../lib/kube.sh
source "$E2E_LIB/kube.sh"

log_section "判定 3: 実行中の encode Job がスケールインで殺されない"

plan "3.1" "3.2" "3.3" "3.4"

queue="encode"
scaledjob="$(scaledjob_for_queue "$queue")"
if discovery_is_ambiguous "$scaledjob"; then
  reason="キュー ${queue} に一致する ScaledJob が複数ある（$(discovery_detail "$scaledjob")）--- どれを判定すべきか決まらない"
  fail "3.1" "$reason"
  fail "3.2" "$reason"
  fail "3.3" "$reason"
  fail "3.4" "$reason"
  exit 0
fi
if [ -z "$scaledjob" ]; then
  # **この TODO は「encode を KEDA ScaledJob で回す」という形を前提にしている。**
  # 別の形（Deployment + HPA など）を採ると、この判定は永久に TODO の
  # まま =「受け入れ 3 が一度も検査されないまま完了」になりうる。形を変える
  # なら、この判定も同じ PR で書き換えること。
  todo "3.1" "キュー ${queue} を引く KEDA ScaledJob がまだ無い（encode を別の形で回すなら判定側も要変更）"
  todo "3.2" "待ち行列が空になったときの生存を観測していない（対象が無い）"
  todo "3.3" "ScaledJob 更新時の生存を観測していない（対象が無い）"
  todo "3.4" "positive control を実施していない（対象が無い）"
  exit 0
fi

# 判定が触った跡を残さない。probe の annotation を残すと、次の周回は実際に
# デプロイされたものと違う spec を判定することになる。
#
# **ただし、この片付け自体が 2 回目の rollout である**（Pod テンプレートを
# 変えるので KEDA から見れば更新）。実行中の Job が残っているときに撃つと、
# 観測も記録もしないまま殺しうる --- 判定 3 が「殺されないこと」を見る道具
# なのに、道具が殺すことになる。実行中の Job が無いときだけ戻し、居るなら
# 残して警告する（次に apply されたときに消える）。
cleanup() {
  if [ -n "$(k get jobs -l "scaledjob.keda.sh/name=${scaledjob}" \
      -o jsonpath='{.items[?(@.status.active)].metadata.name}' 2>/dev/null)" ]; then
    log_step "probe annotation を残す（実行中の Job があるので、片付けの rollout を撃たない）"
    return
  fi
  k patch scaledjob "$scaledjob" --type=json \
    -p '[{"op":"remove","path":"/spec/jobTargetRef/template/metadata/annotations/rokuban-e2e~1probe"}]' \
    >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ---- 3.1 実行中の Job を 1 つ作る ----------------------------------------

default_encode_producer() { insert_probe_job "$queue"; }
producer="${E2E_ENCODE_PRODUCER:-default_encode_producer}"
# 打ち間違えた producer は "command not found" として握られ、3.1 が
# 「encode Job が起きない」で赤くなる（原因が出力から読めない）。
if ! command -v "${producer%% *}" >/dev/null 2>&1 && ! declare -F "${producer%% *}" >/dev/null; then
  fail "3.1" "E2E_ENCODE_PRODUCER に指定された '${producer}' が見つからない"
  todo "3.2" "producer が無いので生存を観測していない"
  todo "3.3" "同上"
  todo "3.4" "同上"
  exit 0
fi
log_step "producing an encode job with: ${producer}"
$producer

# **追いかけるのは Job であって Pod ではない。** Pod 名で追うと、
# `backoffLimit > 0` の Job が Pod を作り直しただけ（あるいはノードの eviction）
# でも「殺された」と読む。KEDA がスケールインで消すのは Job なので、Job の
# 生死で見るほうが主張とも一致する。
running_job=""
find_running_job() {
  running_job="$(k get jobs -l "scaledjob.keda.sh/name=${scaledjob}" \
    -o jsonpath='{.items[?(@.status.active)].metadata.name}' 2>/dev/null | awk '{print $1}')"
  [ -n "$running_job" ]
}
if ! retry_until 240 "an encode job to start running" find_running_job; then
  fail "3.1" "encode Job が起きない --- backlog=$(river_backlog "$queue") jobs=$(jobs_owned_by_scaledjob "$scaledjob")"
  todo "3.2" "実行中の Job が無いので生存を観測していない"
  todo "3.3" "実行中の Job が無いので生存を観測していない"
  todo "3.4" "実行中の Job が無いので positive control を実施していない"
  exit 0
fi
pass "3.1" "encode Job が起きて実行中になった（${running_job}）"

job_is_alive() {
  local active deletion
  active="$(k get job "$running_job" -o jsonpath='{.status.active}' 2>/dev/null)" || return 1
  deletion="$(k get job "$running_job" -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null)"
  [ "${active:-0}" -ge 1 ] 2>/dev/null && [ -z "$deletion" ]
}
job_state() {
  k get job "$running_job" -o jsonpath='active={.status.active} deletionTimestamp={.metadata.deletionTimestamp}' 2>&1 \
    || echo "（Job が消えている）"
}

# KEDA が次に判断するまでの窓。既定は 30 秒（ScaledJob.spec.pollingInterval）。
polling="$(k get scaledjob "$scaledjob" -o jsonpath='{.spec.pollingInterval}')"
window=$(( 2 * ${polling:-30} ))

# ---- 3.2 待ち行列が空になっても殺されない ---------------------------------

log_step "draining ${queue} and watching ${running_job} for ${window}s"
drain_queue "$queue"
backlog_empty() { [ "$(river_backlog "$queue")" = "0" ]; }
if ! retry_until 60 "the backlog to reach 0" backlog_empty; then
  log_step "backlog is still $(river_backlog "$queue")"
fi

deadline=$((SECONDS + window))
killed=""
while [ "$SECONDS" -lt "$deadline" ]; do
  if ! job_is_alive; then
    killed="$(job_state)"
    break
  fi
  sleep 2
done
if [ -z "$killed" ]; then
  pass "3.2" "待ち行列が空になっても実行中の encode Job は生きている（${window}s 観測）"
else
  fail "3.2" "待ち行列が空になったら実行中の encode Job が殺された --- ${killed}"
fi

# ---- 3.3 ScaledJob を更新しても殺されない ---------------------------------
#
# `rollout.strategy` の値ごとの挙動は docs/operations.md §5 にまとめてある。
# 運用でこれが起きるのはイメージのタグを上げたときなので、「録画のエンコード中に
# デプロイしたら消える」に直結する。

if ! job_is_alive; then
  todo "3.3" "3.2 の時点で Job が生きていないので更新時の生存を観測していない"
else
  # **Pod テンプレートを変える。** ScaledJob 本体に annotate するだけでは
  # KEDA は rollout と見なさず、`rollout.strategy: immediate` でも実行中の Job は
  # 消えなかった（実測。オラクル検査 O3.mut-rollout がこれで落ちた）。運用で
  # 起きる rollout はイメージのタグの差し替え = Pod テンプレートの変更なので、
  # そちらを再現する。
  log_step "changing ${scaledjob}'s pod template (simulating a rollout)"
  k patch scaledjob "$scaledjob" --type=merge \
    -p "{\"spec\":{\"jobTargetRef\":{\"template\":{\"metadata\":{\"annotations\":{\"rokuban-e2e/probe\":\"$(date +%s)\"}}}}}}" >/dev/null
  deadline=$((SECONDS + window))
  killed=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    if ! job_is_alive; then
      killed="$(job_state)"
      break
    fi
    sleep 2
  done
  if [ -z "$killed" ]; then
    pass "3.3" "ScaledJob を更新しても実行中の encode Job は生きている（rollout.strategy=$(k get scaledjob "$scaledjob" -o jsonpath='{.spec.rollout.strategy}' || true)）"
  else
    fail "3.3" "ScaledJob の更新で実行中の encode Job が殺された --- ${killed}"
  fi
fi

# ---- 3.4 positive control -------------------------------------------------
#
# **「殺されなかった」を主張する前に、「殺されたら分かる」ことを示す。**
# 3.2 / 3.3 は窓を取るだけの negative assertion なので、Pod の観測が壊れて
# いれば（名前が違う・権限が無い・jsonpath が空を返す）黙って緑になる。

if ! job_is_alive; then
  todo "3.4" "positive control: 観測対象の Job が既に居ないので実施していない"
else
  log_step "positive control: deleting ${running_job} on purpose"
  k delete job "$running_job" --grace-period=0 --wait=false >/dev/null 2>&1
  gone() { ! job_is_alive; }
  if retry_until 60 "the observation to notice the kill" gone; then
    pass "3.4" "positive control: 意図的に消した Job を観測が検出した"
  else
    fail "3.4" "positive control: Job を消したのに観測が生存のまま --- 3.2 / 3.3 の緑は信用できない"
  fi
fi
exit 0
