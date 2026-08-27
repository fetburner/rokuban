#!/usr/bin/env bash
# 判定 4: watcher を 2 レプリカにしても二重に動かない（advisory lock の実効確認）。
#
# **何を「二重に動く」と定義するか。** watcher の singleton 性が主張している
# のは「mirakc に N 本の SSE を張らない」ことであって、処理の冪等性ではない
# （processRecord は record_sync の行ロックで冪等。internal/role/leader.go の
# パッケージコメント）。したがって判定は **mirakc 側で開いている /events の
# 本数**で行う。ログの「リーダーになった」行を数える形にすると、ログの文言を
# 変えただけで判定が黙って通らなくなる。
#
# この判定は kind でしか出ない --- 単体テストは advisory lock 単体
# （internal/role の TestTryAcquire_Exclusive）までで、**2 つの Pod が同じ DB を
# 見ている**状況は作れない。
set -uo pipefail

E2E_LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)"
# shellcheck source=../lib/env.sh
source "$E2E_LIB/env.sh"
# shellcheck source=../lib/log.sh
source "$E2E_LIB/log.sh"
# shellcheck source=../lib/kube.sh
source "$E2E_LIB/kube.sh"

log_section "判定 4: watcher を 2 レプリカにしても二重に動かない"

plan "4.1" "4.2"

site="$E2E_SITE_A"
deploy="$(deployment_for_component_site watcher "$site")"
if discovery_is_ambiguous "$deploy"; then
  reason="site ${site} に束縛された watcher Deployment が複数ある（$(discovery_detail "$deploy")）--- どれを判定すべきか決まらない"
  fail "4.1" "$reason"
  fail "4.2" "$reason"
  exit 0
fi
if [ -z "$deploy" ]; then
  todo "4.1" "site ${site} に束縛された watcher Deployment がまだ無い（component=watcher かつ argv に --sites ${site}）"
  todo "4.2" "同上（2 レプリカでの SSE 本数を観測していない）"
  exit 0
fi

# 読めないまま既定 1 に潰すと、**製品の watcher を 1 に縮めて**終わる。
if ! original="$(k get deployment "$deploy" -o jsonpath='{.spec.replicas}')" || [ -z "$original" ]; then
  fail "4.1" "watcher (${deploy}) の spec.replicas を読めない --- 元に戻せないので触らない"
  todo "4.2" "レプリカ数を変えられないので SSE 本数を観測していない"
  exit 0
fi
restore() {
  log_step "restoring ${deploy} to ${original} replica(s)"
  k scale deployment "$deploy" --replicas="${original:-1}" >/dev/null 2>&1 || true
}
trap restore EXIT

log_step "scaling ${deploy} to 2 replicas"
k scale deployment "$deploy" --replicas=2 >/dev/null
if ! retry_until 240 "both watcher replicas to become ready" \
  k rollout status "deployment/$deploy" --timeout=10s; then
  fail "4.1" "watcher を 2 レプリカにできない --- $(k get pods -l "app.kubernetes.io/component=watcher" --no-headers | tr '\n' ';')"
  todo "4.2" "レプリカが揃っていないので SSE 本数を観測していない"
  exit 0
fi
assert_eq "4.1" "2" "$(k get deployment "$deploy" -o jsonpath='{.status.readyReplicas}')" \
  "watcher (${deploy}) が 2 レプリカとも Ready"

# ---- 4.2 SSE の本数 -------------------------------------------------------
#
# **positive control を先に取る。** eventsTotal が 0 のままなら、誰も繋いで
# いないだけで eventsOpen も 0 になり、「1 本しか張っていない」に見えてしまう。
connected() { [ "$(mock_stat "$site" eventsTotal)" -ge 1 ]; }
if ! retry_until 120 "the watcher to open an SSE connection" connected; then
  fail "4.2" "watcher が mirakc に一度も繋いでいない（eventsTotal=0）--- 本数の判定が成立しない"
  exit 0
fi

# 2 本目が遅れて張られる場合を取りこぼさないよう、窓を取って**最大値**を見る。
# 「今 1 本」を 1 回見るだけだと、リーダー交代の隙間を見ただけでも緑になる。
#
# **読めなかったサンプルを 0 として飲み込まない。** 潰すと、窓の途中でモックや
# ツールボックスが落ちた場合に「以降ずっと 0」＝最大値 1 のまま緑になり、
# 2 本張られていても分からない（窓の前の positive control はこの経路を
# カバーしない）。
log_step "sampling mirakc-${site} /events connections for 30s"
max_open=0
samples=0
unreadable=0
deadline=$((SECONDS + 30))
while [ "$SECONDS" -lt "$deadline" ]; do
  open="$(mock_stat "$site" eventsOpen)"
  if [ -z "$open" ]; then
    unreadable=$((unreadable + 1))
  else
    samples=$((samples + 1))
    if [ "$open" -gt "$max_open" ]; then
      max_open="$open"
    fi
  fi
  sleep 2
done
if [ "$unreadable" -gt 0 ] || [ "$samples" -eq 0 ]; then
  fail "4.2" "測定できない: 30s の間に ${unreadable} 回 /mock/stats を読めなかった（成功 ${samples} 回）--- 本数の判定は成立しない"
else
  assert_eq "4.2" "1" "$max_open" "2 レプリカでも mirakc への SSE は 1 本（30s / ${samples} サンプルの最大値）"
fi
exit 0
