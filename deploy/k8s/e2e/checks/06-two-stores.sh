#!/usr/bin/env bash
# 判定 6: 置き場が 2 つあるとき、置き場ごとの仕事が正しい置き場の Pod で動く。
#
# **置き場が 1 つの構成では原理的に測れない**ので、`overlays/e2e-two-stores`
# （tokyo / takamatsu）でだけ走る（run.sh が overlay で振り分ける）。判定 5（サイト）と
# 同じ形で、置き場の取り違えが起きたときの症状は「別の置き場の PVC を挿した Pod が起きて
# 目印が合わず何もせず終わる」か「そもそも誰も起きない」である。単体テスト
# （deploy/k8s/stores_test.go）が見ているのはマニフェストの宣言までで、**KEDA が実際に
# 置き場ごとの Job を起こし、その Pod テンプレートが正しい PVC と argv を持つか**はここでしか出ない。
#
#   6.1  置き場束縛キュー 7 本 x 置き場 2 つ = 14 本の ScaledJob が 1 本ずつ見つかる
#   6.2  tokyo のキューにだけ滞留を作ると、tokyo の 7 本の ScaledJob だけが Job を起こし、
#        その Job は tokyo の PVC を tokyo の root に挿し `--stores tokyo` を持つ
#        （takamatsu の 7 本は Job を 1 つも起こさない = negative。positive control は tokyo の 7 本）
#   6.3  同じことを takamatsu で
#   6.4  ingest（site 束縛）の Job は両方の置き場を root に挿す
#   6.5  置き場ごとの配信 Service（Ingress の `/api/media/stores/<置き場>` の行き先）が、
#        その置き場の原本だけを返し、別の置き場の Service は返さない
#   6.6  置き場束縛の CronJob（catalog-export / delete-reconcile / storage-sync）が置き場ごとに 1 本ある
#
# **この判定が見ていないもの**（緑でも保証しない）:
#   - 実際の転送・エンコード・削除の中身。6.2 / 6.3 が入れるのは `e2e_probe`（実物の worker には
#     未登録の kind。lib/kube.sh の insert_probe_job）なので、Job は 1 回失敗して終わる。
#     「正しい Pod が起きた」までで、「その Pod が正しく仕事をした」ではない
#   - ingest が録画ごとに規則（`storage.originals`）どおりの置き場へ書くこと（mirakc の録画を通していない）
#   - Ingress controller を経由した経路。kind のハーネスには controller が無いので、Ingress の
#     行き先 Service を直接叩く（行と Service 名の対応は stores_test.go が見る）
#
# **この判定は実行していない版である**（書いた環境に kind / docker が無かった）。初回の実機実行で
# 6.2 / 6.3 の待ち時間（KEDA の pollingInterval 10 秒 + Pod 起動）と 6.5 の DB 行の形
# （配信が `media_assets` のどの列を要求するか）が合わなければ、ここを直す。
set -uo pipefail

E2E_LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)"
# shellcheck source=../lib/env.sh
source "$E2E_LIB/env.sh"
# shellcheck source=../lib/log.sh
source "$E2E_LIB/log.sh"
# shellcheck source=../lib/kube.sh
source "$E2E_LIB/kube.sh"

log_section "判定 6: 置き場が 2 つあるとき、置き場ごとの仕事が正しい置き場の Pod で動く"

plan "6.1" "6.2" "6.3" "6.4" "6.5" "6.6"

if [ "$E2E_OVERLAY" != "e2e-two-stores" ]; then
  reason="E2E_OVERLAY=${E2E_OVERLAY}（置き場 2 つの overlay ではない）。判定 6 は e2e-two-stores でだけ走る"
  for id in 6.1 6.2 6.3 6.4 6.5 6.6; do todo "$id" "$reason"; done
  exit 0
fi

stores="$E2E_STORE_A $E2E_STORE_B"
store_root() { printf '/mnt/stores/%s' "$1"; }
store_pvc() { printf 'rokuban-media-%s' "$1"; }
other_store() { if [ "$1" = "$E2E_STORE_A" ]; then printf '%s' "$E2E_STORE_B"; else printf '%s' "$E2E_STORE_A"; fi; }
# `cm_detect` → `rokuban-worker-cm-detect-<store>`。探索はキュー名で行い（scaledjob_for_queue）、
# 名前は「見つかったものが期待の名前か」を見るためだけに使う。
sj_name_for() { printf 'rokuban-worker-%s-%s' "$(printf '%s' "$1" | tr '_' '-')" "$2"; }

cleanup() {
  watch_jobs_stop
  for s in $stores; do
    for q in $E2E_STORE_QUEUES; do drain_queue "${q}_${s}"; done
  done
  drain_queue "ingest_${E2E_SITE_A}"
  drain_queue "ingest_${E2E_SITE_B}"
  for s in $stores; do
    k delete job "e2e-store-seed-${s}" --ignore-not-found >/dev/null 2>&1 || true
  done
  restore_cronjobs
}
trap cleanup EXIT

# ---- 6.1 14 本の ScaledJob が 1 本ずつ見つかる -----------------------------

problems=""
seen=""
for s in $stores; do
  for q in $E2E_STORE_QUEUES; do
    sj="$(scaledjob_for_queue "${q}_${s}")"
    if discovery_is_unusable "$sj"; then
      problems="${problems} ${q}_${s}:$(discovery_detail "$sj")"
      continue
    fi
    if [ -z "$sj" ]; then
      problems="${problems} ${q}_${s}:なし"
      continue
    fi
    if [ "$sj" != "$(sj_name_for "$q" "$s")" ]; then
      problems="${problems} ${q}_${s}:名前が想定と違う(${sj})"
    fi
    case " $seen " in
      *" $sj "*) problems="${problems} ${q}_${s}:${sj}が別のキューと重複" ;;
    esac
    seen="${seen} ${sj}"
  done
done
if [ -n "$problems" ]; then
  fail "6.1" "置き場束縛の ScaledJob が揃っていない:${problems}"
  for id in 6.2 6.3 6.4; do todo "$id" "6.1 が成立していないので観測していない"; done
else
  pass "6.1" "置き場束縛キュー $(printf '%s' "$E2E_STORE_QUEUES" | wc -w | tr -d ' ') 本 x 置き場 2 つの ScaledJob が 1 本ずつある"
fi

# 測定の間 CronJob を止める（置き場束縛の CronJob が original_<置き場> / storage_<置き場> に
# 本物のジョブを積むと、「滞留を作った置き場でしか Job が起きない」を壊す）。
suspend_all_cronjobs

# ---- 6.2 / 6.3 片方の置き場にだけ滞留を作る --------------------------------

# store_phase <id> <active> <idle>
store_phase() {
  local id="$1" active="$2" idle="$3" q sj_a sj_i
  # 前の周回の Job と滞留を消す。**消してから積む**: 残った Job を「今回起きた Job」と
  # 読まないため（negative の根拠が「idle 側の Job が 0 個」なので、ここで 0 にしておく）。
  for q in $E2E_STORE_QUEUES; do
    sj_a="$(scaledjob_for_queue "${q}_${active}")"
    sj_i="$(scaledjob_for_queue "${q}_${idle}")"
    k delete jobs -l "scaledjob.keda.sh/name=${sj_a}" --ignore-not-found >/dev/null 2>&1 || true
    k delete jobs -l "scaledjob.keda.sh/name=${sj_i}" --ignore-not-found >/dev/null 2>&1 || true
    drain_queue "${q}_${active}"
    drain_queue "${q}_${idle}"
  done
  for q in $E2E_STORE_QUEUES; do
    insert_probe_job "${q}_${active}" 1
  done

  # **全部の active 側 ScaledJob が Job を起こすまで待つ**（positive control を兼ねる）。
  all_spawned() {
    local qq sj
    for qq in $E2E_STORE_QUEUES; do
      sj="$(scaledjob_for_queue "${qq}_${active}")"
      [ -n "$(jobs_owned_by_scaledjob "$sj")" ] || return 1
    done
  }
  if ! retry_until 360 "every ${active} ScaledJob to spawn a job" all_spawned; then
    local missing=""
    for q in $E2E_STORE_QUEUES; do
      sj="$(scaledjob_for_queue "${q}_${active}")"
      [ -n "$(jobs_owned_by_scaledjob "$sj")" ] || missing="${missing} ${q}_${active}"
    done
    fail "$id" "positive control: ${active} に滞留を作っても Job が起きない ScaledJob がある:${missing}（backlog: $(for q in $E2E_STORE_QUEUES; do printf '%s=%s ' "$q" "$(river_backlog "${q}_${active}")"; done)）--- 以下の negative は信用できない"
    return
  fi

  # active 側の Job の Pod テンプレートが、active の PVC・root・argv を持つこと。
  local bad="" row name queues stores_arg claims mounts
  for q in $E2E_STORE_QUEUES; do
    sj_a="$(scaledjob_for_queue "${q}_${active}")"
    if ! facts="$(jobs_pod_facts "$sj_a")" || [ -z "$facts" ]; then
      bad="${bad} ${q}:Job の Pod テンプレートを読めない"
      continue
    fi
    while IFS= read -r row; do
      IFS=$'\t' read -r name queues stores_arg claims mounts <<<"$row"
      [ "$queues" = "$q" ] || bad="${bad} ${name}:--queues=${queues}(想定 ${q})"
      [ "$stores_arg" = "$active" ] || bad="${bad} ${name}:--stores=${stores_arg}(想定 ${active})"
      [ "$claims" = "$(store_pvc "$active")" ] || bad="${bad} ${name}:PVC=${claims}(想定 $(store_pvc "$active"))"
      [ "$mounts" = "$(store_root "$active")" ] || bad="${bad} ${name}:mount=${mounts}(想定 $(store_root "$active"))"
    done <<<"$facts"
  done
  if [ -n "$bad" ]; then
    fail "$id" "${active} の ScaledJob が起こした Job が別の置き場を向いている:${bad}"
    return
  fi

  # negative: idle 側の ScaledJob は Job を 1 つも起こしていない。
  local spawned="" idle_backlog
  for q in $E2E_STORE_QUEUES; do
    sj_i="$(scaledjob_for_queue "${q}_${idle}")"
    if [ -n "$(jobs_owned_by_scaledjob "$sj_i")" ]; then
      spawned="${spawned} ${sj_i}"
    fi
    idle_backlog="$(river_backlog "${q}_${idle}")"
    if [ "$idle_backlog" != "0" ]; then
      spawned="${spawned} (${q}_${idle} に滞留 ${idle_backlog} --- 判定の前提が崩れた)"
    fi
  done
  if [ -n "$spawned" ]; then
    fail "$id" "${active} にだけ滞留があるのに ${idle} 側が動いた:${spawned}"
    return
  fi
  pass "$id" "${active} の滞留では ${active} の ScaledJob だけが Job を起こし、各 Job は PVC $(store_pvc "$active") を $(store_root "$active") に挿し --stores ${active} を持つ（${idle} は 0 個）"
}

if [ -z "$problems" ]; then
  store_phase "6.2" "$E2E_STORE_A" "$E2E_STORE_B"
  store_phase "6.3" "$E2E_STORE_B" "$E2E_STORE_A"

  # ---- 6.4 ingest は両方の置き場を挿す --------------------------------------
  #
  # ingest は site 束縛で、原本の置き場は規則が録画ごとに決める。規則が選びうる置き場を
  # マウントしていない Job が起きると、その録画の ingest だけが失敗する。
  ingest_bad=""
  want_claims="$(printf '%s\n%s\n' "$(store_pvc "$E2E_STORE_A")" "$(store_pvc "$E2E_STORE_B")" | sort | paste -sd, -)"
  want_mounts="$(printf '%s\n%s\n' "$(store_root "$E2E_STORE_A")" "$(store_root "$E2E_STORE_B")" | sort | paste -sd, -)"
  want_stores="$(printf '%s\n%s\n' "$E2E_STORE_A" "$E2E_STORE_B" | sort | paste -sd, -)"
  for site in "$E2E_SITE_A" "$E2E_SITE_B"; do
    sj="$(scaledjob_for_queue "ingest_${site}")"
    if [ -z "$sj" ] || discovery_is_unusable "$sj"; then
      ingest_bad="${ingest_bad} ingest_${site}:ScaledJob が一意に決まらない"
      continue
    fi
    k delete jobs -l "scaledjob.keda.sh/name=${sj}" --ignore-not-found >/dev/null 2>&1 || true
    drain_queue "ingest_${site}"
    insert_probe_job "ingest_${site}" 1
    spawned_ingest() { [ -n "$(jobs_owned_by_scaledjob "$sj")" ]; }
    if ! retry_until 360 "ingest ScaledJob ${sj} to spawn a job" spawned_ingest; then
      ingest_bad="${ingest_bad} ${sj}:滞留を作っても Job が起きない"
      continue
    fi
    facts="$(jobs_pod_facts "$sj")" || facts=""
    while IFS= read -r row; do
      [ -n "$row" ] || continue
      IFS=$'\t' read -r name queues stores_arg claims mounts <<<"$row"
      got_stores="$(printf '%s' "$stores_arg" | tr ',' '\n' | sort | paste -sd, -)"
      [ "$got_stores" = "$want_stores" ] || ingest_bad="${ingest_bad} ${name}:--stores=${stores_arg}"
      [ "$claims" = "$want_claims" ] || ingest_bad="${ingest_bad} ${name}:PVC=${claims}"
      [ "$mounts" = "$want_mounts" ] || ingest_bad="${ingest_bad} ${name}:mount=${mounts}"
    done <<<"$facts"
  done
  if [ -n "$ingest_bad" ]; then
    fail "6.4" "ingest の Job が両方の置き場を挿していない:${ingest_bad}"
  else
    pass "6.4" "ingest の Job は site ごとに両方の置き場（${want_claims}）を root（${want_mounts}）に挿す"
  fi
fi

# ---- 6.5 置き場ごとの配信 Service -------------------------------------------
#
# Ingress は `/api/media/stores/<置き場>` を置き場ごとの Service へ送る（行と Service 名の
# 対応は stores_test.go）。ここではその Service に直接 curl して、置き場の原本だけが返ることを見る。
#
# 原本は置き場ごとの Job が PVC に書く（cluster/store-seed-job.yaml）。recording の行は
# 配信が引く最小の形（lib/kube.sh の produce_real_encode_job と同じ列）。**配信がこの行の形を
# 受け付けるかは未実測**で、受け付けなければ positive control（正しい置き場で 200 + 中身）が
# 落ちて FAIL になる（黙って緑にはならない）。

run_id="${E2E_FAULT_RUN_ID:-$(date -u +%Y%m%d%H%M%S)}"
serve_bad=""
serve_note=""
serve_site_of() { if [ "$1" = "$E2E_STORE_A" ]; then printf '%s' "$E2E_SITE_A"; else printf '%s' "$E2E_SITE_B"; fi; }
for s in $stores; do
  site="$(serve_site_of "$s")"
  title="e2e store probe ${s}"
  rel="sites/${site}/e2e/store-probe.txt"
  payload="e2e-payload-${s}-${run_id}"
  # **文字種を絞る**（sed と SQL と printf に埋める）。
  case "$payload" in *[!A-Za-z0-9._-]*) serve_bad="${serve_bad} ${s}:payload の文字種"; continue ;; esac

  k delete job "e2e-store-seed-${s}" --ignore-not-found >/dev/null 2>&1 || true
  if ! apply_template "$E2E_DIR/cluster/store-seed-job.yaml" \
      -e "s|__JOB__|e2e-store-seed-${s}|g" -e "s|__PVC__|$(store_pvc "$s")|g" \
      -e "s|__ROOT__|$(store_root "$s")|g" -e "s|__REL_PATH__|${rel}|g" -e "s|__PAYLOAD__|${payload}|g"; then
    serve_bad="${serve_bad} ${s}:seed Job を apply できない"
    continue
  fi
  if ! k wait --for=condition=complete "job/e2e-store-seed-${s}" --timeout=240s >/dev/null 2>&1; then
    serve_bad="${serve_bad} ${s}:seed Job が完了しない($(k logs "job/e2e-store-seed-${s}" 2>&1 | tail -2 | tr '\n' ' '))"
    continue
  fi
  size="$(printf '%s' "$payload" | wc -c | tr -d ' ')"

  psql_q "DELETE FROM media_assets WHERE recording_id IN (SELECT id FROM recordings WHERE title = '${title}')" >/dev/null
  psql_q "DELETE FROM recordings WHERE title = '${title}'" >/dev/null
  rec_id="$(psql_q "INSERT INTO recordings
      (source, site, network_id, service_id, event_id, service_name,
       channel_type, channel, title, program_start_at, program_duration_ms, status)
    VALUES ('manual', '${site}', 32736, 1, 1, 'e2e', 'GR', '27', '${title}', now(), 60000, 'finished')
    RETURNING id" | head -1 | tr -d '[:space:]')"
  case "$rec_id" in ''|*[!0-9]*) serve_bad="${serve_bad} ${s}:recording を作れない(${rec_id})"; continue ;; esac
  if ! psql_q "WITH asset AS (
                 INSERT INTO media_assets (recording_id, kind, size_bytes)
                 VALUES (${rec_id}, 'original', ${size}) RETURNING id)
               INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
               SELECT id, '${s}', '${rel}', 0, 'active' FROM asset" >/dev/null; then
    serve_bad="${serve_bad} ${s}:media_assets の行を作れない"
    continue
  fi

  # positive control: その置き場の Service は原本の中身を返す。
  url_path="/api/media/stores/${s}/recordings/${rec_id}/file"
  out="$(tb_curl -H 'Host: rokuban.local' -w '\n%{http_code}' "http://rokuban-recording-hls-${s}:40773${url_path}" 2>&1)"
  code="${out##*$'\n'}"
  body="${out%$'\n'*}"
  if [ "$code" != "200" ] || [ "$body" != "$payload" ]; then
    serve_bad="${serve_bad} ${s}:positive control が落ちた(HTTP ${code}, body='${body}')"
    continue
  fi
  # negative: 別の置き場の Service は、同じ録画 id の原本を返さない（その置き場には無い）。
  o="$(other_store "$s")"
  for u in "/api/media/stores/${s}/recordings/${rec_id}/file" "/api/media/stores/${o}/recordings/${rec_id}/file"; do
    out="$(tb_curl -H 'Host: rokuban.local' -w '\n%{http_code}' "http://rokuban-recording-hls-${o}:40773${u}" 2>&1)"
    code="${out##*$'\n'}"
    body="${out%$'\n'*}"
    if [ "$code" = "200" ] || [ "$body" = "$payload" ]; then
      serve_bad="${serve_bad} ${s}の原本を ${o} の Service が返した(${u} HTTP ${code})"
    fi
  done
  serve_note="${serve_note} ${s}=OK"
done
if [ -n "$serve_bad" ]; then
  fail "6.5" "置き場ごとの配信が成立していない:${serve_bad}"
else
  pass "6.5" "置き場ごとの配信 Service はその置き場の原本だけを返す（${serve_note# }）"
fi

# ---- 6.6 置き場束縛の CronJob -------------------------------------------------

cron_bad=""
for s in $stores; do
  for job in catalog-export delete-reconcile storage-sync; do
    if ! names="$(cronjobs_enqueueing_store "$job" "$s")"; then
      cron_bad="${cron_bad} ${job}/${s}:CronJob を引けない"
      continue
    fi
    n="$(printf '%s' "$names" | wc -w | tr -d ' ')"
    [ "$n" = "1" ] || cron_bad="${cron_bad} ${job}/${s}:${n} 本(${names})"
  done
done
if [ -n "$cron_bad" ]; then
  fail "6.6" "置き場束縛の CronJob が置き場ごとに 1 本ずつ無い:${cron_bad}"
else
  pass "6.6" "catalog-export / delete-reconcile / storage-sync の CronJob が置き場ごとに 1 本ずつある"
fi
exit 0
