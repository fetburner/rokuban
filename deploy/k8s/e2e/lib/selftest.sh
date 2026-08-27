#!/usr/bin/env bash
# lib/ の純粋な部分（対象の探索と watch ログの読み取り）のユニットテスト。
#
#   ./deploy/k8s/e2e/lib/selftest.sh
#
# **クラスタを立てずに回る。** `k` を差し替えて用意した JSON を食わせ、
# watch ログはファイルで与える。CI で回すのはここまで（kind を立てる本体は
# 回さない。docs/runbook/k8s.md §CI では回さない）。
#
# **なぜ要るか。** 判定の「対象を探す」層はクラスタ側の状態を必要としないのに、
# 一度もテストが無かった。そのせいで**曖昧な一致を FAIL にする経路が、
# 大域変数の代入が `$( )` の子シェルに閉じるせいで一度も動いていなかった**
# のを、実機を 2 周してもオラクル自己検査でも捕まえられなかった（変異が
# その状態を作らなかったため）。ここはその層を直接叩く。
set -uo pipefail

E2E_LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
selftest_results="$(mktemp)"
E2E_RESULTS="$selftest_results"
export E2E_RESULTS
# shellcheck source=env.sh
source "$E2E_LIB/env.sh"
# shellcheck source=log.sh
source "$E2E_LIB/log.sh"
# shellcheck source=kube.sh
source "$E2E_LIB/kube.sh"

failures=0

check() {
  local name="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    printf 'ok   %s\n' "$name"
  else
    printf 'FAIL %s\n  want: [%s]\n  got:  [%s]\n' "$name" "$want" "$got"
    failures=$((failures + 1))
  fi
}

# k / kall を差し替える。$K_FIXTURE の中身をそのまま返す。
K_FIXTURE=""
k() { printf '%s' "$K_FIXTURE"; }
kall() { return 0; }
keda_installed() { return 0; }

scaledjob_json() {
  python3 -c '
import json, sys
items = []
for spec in sys.argv[1:]:
    name, queue = spec.split("=", 1)
    items.append({
        "metadata": {"name": name, "annotations": {"note": "ignored blob"}},
        "spec": {"triggers": [{"metadata": {"query": f"... queue = \x27{queue}\x27 ..."}}]},
    })
print(json.dumps({"items": items}))
' "$@"
}

# ---- scaledjob_for_queue ---------------------------------------------------

K_FIXTURE="$(scaledjob_json "w-epg-sitea=epg_sitea" "w-epg-siteb=epg_siteb")"
check "一意に決まれば名前を返す" "w-epg-sitea" "$(scaledjob_for_queue epg_sitea)"
check "0 件なら空を返す" "" "$(scaledjob_for_queue encode)"

# **この 3 行が今回の本題。** 複数一致は「空（= まだ実装されていない）」では
# なく、曖昧であることが呼び出し側に届かなければならない。
K_FIXTURE="$(scaledjob_json "w-a=epg_sitea" "w-a-dup=epg_sitea")"
got="$(scaledjob_for_queue epg_sitea)"
if discovery_is_ambiguous "$got"; then
  check "複数一致は曖昧として一致名を返す" "w-a w-a-dup" "$(discovery_detail "$got")"
else
  check "複数一致は曖昧として報告される" "AMBIGUOUS..." "$got"
fi

# ---- cronjob_enqueueing ----------------------------------------------------

cronjob_json() {
  python3 -c '
import json, sys
items = []
for spec in sys.argv[1:]:
    name, argv = spec.split("=", 1)
    items.append({
        "metadata": {"name": name},
        "spec": {"jobTemplate": {"spec": {"template": {"spec": {
            "containers": [{"args": argv.split(",")}]}}}}},
    })
print(json.dumps({"items": items}))
' "$@"
}

K_FIXTURE="$(cronjob_json "cron-a=enqueue,epg-sync,--site,sitea" "cron-b=enqueue,epg-sync,--site,siteb")"
check "argv で site を厳密照合する" "cron-a" "$(cronjob_enqueueing epg-sync sitea)"

# 接頭辞が重なるサイト名で取り違えないこと（home が home2 を拾わない）。
K_FIXTURE="$(cronjob_json "cron-home2=enqueue,epg-sync,--site,home2")"
check "接頭辞が重なるサイト名を拾わない" "" "$(cronjob_enqueueing epg-sync home)"

# ジョブ名も要素単位。`epg-sync` を探して `epg-sync-extra` を拾わない。
K_FIXTURE="$(cronjob_json "cron-x=enqueue,epg-sync-extra,--site,sitea")"
check "ジョブ名も要素単位で照合する" "" "$(cronjob_enqueueing epg-sync sitea)"

K_FIXTURE="$(cronjob_json "cron-a=enqueue,epg-sync,--site,sitea" "cron-dup=enqueue,epg-sync,--site,sitea")"
got="$(cronjob_enqueueing epg-sync sitea)"
check "CronJob の複数一致も曖昧として報告される" "0" "$(discovery_is_ambiguous "$got"; echo $?)"

# ---- deployment_for_component_site -----------------------------------------

deployment_json() {
  python3 -c '
import json, sys
items = []
for spec in sys.argv[1:]:
    name, argv = spec.split("=", 1)
    items.append({
        "metadata": {"name": name},
        "spec": {"template": {"spec": {"containers": [{"args": argv.split(",")}]}}},
    })
print(json.dumps({"items": items}))
' "$@"
}

K_FIXTURE="$(deployment_json "w-sitea=server,--roles,watcher,--sites,sitea" "w-siteb=server,--roles,watcher,--sites,siteb")"
check "argv の --sites で Deployment を引く" "w-sitea" "$(deployment_for_component_site watcher sitea)"

K_FIXTURE="$(deployment_json "w-home2=server,--roles,watcher,--sites,home2")"
check "Deployment 側も接頭辞で取り違えない" "" "$(deployment_for_component_site watcher home)"

K_FIXTURE="$(deployment_json "w-a=server,--sites,sitea" "w-a2=server,--sites=sitea")"
got="$(deployment_for_component_site watcher sitea)"
check "Deployment の複数一致も曖昧として報告される" "0" "$(discovery_is_ambiguous "$got"; echo $?)"

# ---- observed_new_jobs -----------------------------------------------------

w="$(mktemp)"
printf '# sentinel\nold-job\n' >"${w}.pre"
cat >"$w" <<'EOF'
ADDED old-job active= succeeded=1 failed=
ADDED new-job active= succeeded= failed=
MODIFIED new-job active=1 succeeded= failed=
MODIFIED new-job active= succeeded=1 failed=
EOF
check "観測開始前から居た Job は数えない" "new-job" "$(observed_new_jobs "$w")"
check "active で絞れる" "new-job" "$(observed_new_jobs "$w" active)"
check "succeeded で絞れる" "new-job" "$(observed_new_jobs "$w" succeeded)"
check "failed は無い" "" "$(observed_new_jobs "$w" failed)"

# **番兵が無いと awk の 2 ファイル読み分けが壊れる**（空の .pre だと watch の
# 中身がまるごと「既存」に飲み込まれる）。watch_jobs_start が必ず番兵を書く
# ことの裏返しとして、ここで壊れ方を固定しておく。
printf '' >"${w}.pre"
check "番兵が無い .pre では全部が既存扱いになる（この形にしないこと）" "" "$(observed_new_jobs "$w")"

# kubectl の診断行を Job 名として数えない。
printf '# sentinel\n' >"${w}.pre"
cat >"$w" <<'EOF'
E0827 12:00:00.000 reflector.go:1 watch of *v1.Job ended with: an error
ADDED real-job active=1 succeeded= failed=
EOF
check "watch イベント以外の行は無視する" "real-job" "$(observed_new_jobs "$w")"

rm -f "$w" "${w}.pre"

# ---- plan / summary と終了コード -------------------------------------------
#
# **受け入れの完了条件そのもの**（1 コマンドで 0 / 1 / 2 が区別できて出る）を
# 担う層。しかもクラスタが要らない。とくに「宣言したのに記録が無い」を拾う
# 経路は、5 本の判定がどの分岐でも宣言 id を全部埋めているので**実機では
# 一度も発火しない** --- 日常的に発火しない検出器を無検証で置くのは、
# 「複数一致は FAIL が一度も動いていなかった」のと同じ族の事故になる。

summary_case() {
  local name="$1" want_exit="$2" want_extra_fail="$3"
  shift 3
  local results out code
  results="$(mktemp)"
  out="$(mktemp)"
  (
    E2E_RESULTS="$results"
    E2E_PARTIAL_RUN="${E2E_PARTIAL_RUN_CASE:-}"
    # shellcheck source=log.sh
    source "$E2E_LIB/log.sh"
    "$@" >/dev/null
    summary >"$out" 2>&1
  )
  code=$?
  check "${name}（終了コード）" "$want_exit" "$code"
  if [ -n "$want_extra_fail" ]; then
    if grep -q "$want_extra_fail" "$out"; then
      printf 'ok   %s（%s を報告）\n' "$name" "$want_extra_fail"
    else
      printf 'FAIL %s（%s を報告しない）\n' "$name" "$want_extra_fail"
      failures=$((failures + 1))
    fi
  fi
  rm -f "$results" "$out"
}

all_pass()   { plan 1.1 1.2; pass 1.1 ok; pass 1.2 ok; }
with_fail()  { plan 1.1;      fail 1.1 broken; }
with_todo()  { plan 1.1;      todo 1.1 "not yet"; }
missing_one(){ plan 1.1 1.2;  pass 1.1 ok; }

summary_case "全部 PASS なら 0" 0 "" all_pass
summary_case "FAIL があれば 1" 1 "" with_fail
summary_case "TODO があれば 2" 2 "" with_todo
summary_case "宣言したのに記録が無ければ FAIL を書き足して 1" 1 "記録しなかった" missing_one
E2E_PARTIAL_RUN_CASE="--only 4" summary_case "一部だけ走らせたなら 0 を返さない" 2 "" all_pass

rm -f "$selftest_results"

if [ "$failures" -gt 0 ]; then
  printf '\n%d test(s) failed\n' "$failures"
  exit 1
fi
printf '\nall lib tests passed\n'
