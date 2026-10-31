#!/usr/bin/env bash
# 判定 6: 置き場が 2 つあるとき、原本・派生物・転送・削除・配信が正しい置き場で実際に働く。
#
# **置き場が 1 つの構成では原理的に測れない**ので、`overlays/e2e-two-stores`
# （FS 置き場 tokyo / takamatsu、派生物は MinIO の S3）でだけ走る（run.sh が overlay で振り分ける）。
#
# 判定 6 は「実物の仕事が起きた」ことを、DB の行・KEDA が起こした Job の Pod テンプレート・
# 置き場の PVC の中身（サイズと SHA-256）・配信の応答で見る。probe（`e2e_probe`）は使わない。
#
#   6.1  置き場束縛キュー 7 本 x 置き場 2 つ = 14 本の ScaledJob が 1 本ずつ見つかる（静的）
#   6.2  **encode / thumbnail（実物）**: 原本のある置き場（tokyo）のキューに積んだジョブを、
#        tokyo の ScaledJob が起こした Job（PVC tokyo・--stores tokyo）が完了させ、
#        派生物（encoded / thumbnail）の行が派生物の置き場 s3 にだけ active で載る
#   6.3  **転送（実物）**: 規則（older_than）の desired が takamatsu の録画を、転送 reconcile が
#        takamatsu のキューに載せ、takamatsu の ScaledJob の Job（PVC takamatsu）が書いた
#        コピーが active になり、SHA-256 が原本と一致する。転送の間は元の tokyo を消さない
#   6.4  ingest の Job は両方の置き場を挿す（Pod テンプレート。ingest が録画を書くことは 6.8）
#   6.5  **配信（実物）**: `/api/media/stores/<置き場>/recordings/<id>/file` が、コピーのある置き場で
#        正しい中身を 200 で返し、無い置き場では 404 を返す
#   6.6  置き場束縛の CronJob（catalog-export / delete-reconcile / storage-sync）が置き場ごとに 1 本ある（静的）
#   6.7  **削除（実物）**: 転送先のコピーができ overlap を過ぎると、tokyo の旧コピーが deleted になり、
#        tokyo の PVC からファイルが消える。転送先のコピーと、別録画の tokyo のコピーは残る
#   6.8  **ingest（実物）**: mirakcmock に注入した録画（POST /mock/records。放送は流れない）を、製品の
#        watcher（record-sweep）→ ingest の経路で取り込む。規則どおりの置き場の copy が注入した TS と
#        SIZE・SHA-256 一致で載り、取り込みを書いた ingest の Job が両方の置き場を挿していること
#
# **この判定が見ていないもの**（緑でも保証しない）:
#   - 6.2 / 6.3 / 6.5 / 6.7 の原本は seed Job が PVC に書く（録画の取り込みは通さない。取り込みは 6.8 だけ）。
#     6.8 の取り込みは「書いた Pod がどの録画を書いたか」を DB に持たないので、Job の成功と
#     Pod テンプレートで見る（録画との対応は観測しない）。
#   - Ingress controller を経由した経路。kind には controller が無いので、Ingress の行き先の
#     Service（rokuban-recording-hls-<置き場>）を直接叩く（行と Service 名の対応は stores_test.go）。
#   - 派生物の S3 の中身。派生物の行（copy の state = active）で見る。MinIO の中は覗かない。
#
# **この判定は実行していない版である**（書いた環境に kind / docker が無かった）。初回の実機実行で
# 次が合わなければ、ここを直す。
#   - storage-transfer-reconcile の CLI（`rokuban enqueue storage-transfer-reconcile`）が未マージなら、
#     6.3 は「enqueue に失敗」で赤くなる
#   - bucket の SigV4 PUT（overlays/e2e-two-stores/minio/bucket-job.yaml）が MinIO に受理されないと、
#     派生物の書き込みが失敗して 6.2 が赤くなる
#   - 6.7 は overlap（32 分）の満了を待つので、判定全体で 40 分前後かかる
set -uo pipefail

E2E_LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)"
# shellcheck source=../lib/env.sh
source "$E2E_LIB/env.sh"
# shellcheck source=../lib/log.sh
source "$E2E_LIB/log.sh"
# shellcheck source=../lib/kube.sh
source "$E2E_LIB/kube.sh"

log_section "判定 6: 置き場が 2 つあるとき、原本・派生物・転送・削除・配信が正しい置き場で実際に働く"

plan "6.1" "6.2" "6.3" "6.4" "6.5" "6.6" "6.7" "6.8"

if [ "$E2E_OVERLAY" != "e2e-two-stores" ]; then
  reason="E2E_OVERLAY=${E2E_OVERLAY}（置き場 2 つの overlay ではない）。判定 6 は e2e-two-stores でだけ走る"
  for id in 6.1 6.2 6.3 6.4 6.5 6.6 6.7 6.8; do todo "$id" "$reason"; done
  exit 0
fi

stores="$E2E_STORE_A $E2E_STORE_B"
store_root() { printf '/mnt/stores/%s' "$1"; }
store_pvc() { printf 'rokuban-media-%s' "$1"; }
other_store() { if [ "$1" = "$E2E_STORE_A" ]; then printf '%s' "$E2E_STORE_B"; else printf '%s' "$E2E_STORE_A"; fi; }
sj_name_for() { printf 'rokuban-worker-%s-%s' "$(printf '%s' "$1" | tr '_' '-')" "$2"; }

# 派生物の置き場（overlays/e2e-two-stores/config.yml の storage.derived と同じ名前）。
E2E_DERIVED_STORE="s3"
# 判定 6.2 が使う速い encode プロファイル（同 config.yml の encode.profiles）。
E2E_FAST_PROFILE="e2e-fast"
# 判定 6.7 の待ち上限。overlap（config.yml の 32 分）+ 削除 reconcile の周期 + 余裕。
E2E_DELETE_TIMEOUT=2700

# この周回の印。録画の title と原本のファイル名に使う（文字種は数字と `$$` だけ）。
run_id="$(date -u +%Y%m%d%H%M%S)$$"

cleanup() {
  for s in $stores; do
    for q in $E2E_STORE_QUEUES; do drain_queue "${q}_${s}"; done
  done
  drain_queue "ingest_${E2E_SITE_A}"
  drain_queue "ingest_${E2E_SITE_B}"
  k delete jobs -l app.kubernetes.io/component=store-seed --ignore-not-found >/dev/null 2>&1 || true
  k delete jobs -l app.kubernetes.io/component=store-check --ignore-not-found >/dev/null 2>&1 || true
  restore_cronjobs
}
trap cleanup EXIT

# ---- 補助 -----------------------------------------------------------------

psql_scalar() { psql_q "$1" | head -1 | tr -d '[:space:]'; }

# rand_event_id --- 録画の event_id に使う乱数。周回をまたいで同じ値を使わない。
rand_event_id() { printf '%s' $((RANDOM * 32768 + RANDOM)); }

# wait_job_done <job名> <秒> --- Job が成功か失敗のどちらかになるまで待つ（成功なら 0）。
# `kubectl wait --for=condition=complete` だと、失敗した Job の完了を上限まで待つことになる。
wait_job_done() {
  local name="$1" timeout="$2" deadline
  deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if [ -n "$(k get job "$name" -o jsonpath='{.status.succeeded}' 2>/dev/null)" ]; then
      return 0
    fi
    if [ -n "$(k get job "$name" -o jsonpath='{.status.failed}' 2>/dev/null)" ]; then
      return 1
    fi
    sleep 2
  done
  return 1
}

# insert_recording <title> <site> <program_start の SQL 式> <event_id> → 録画 id
#
# 放送は 60 秒。title は周回の印を含むので、後から見分けられる。
insert_recording() {
  local id
  id="$(psql_scalar "INSERT INTO recordings
      (source, site, network_id, service_id, event_id, service_name,
       channel_type, channel, title, program_start_at, program_duration_ms, status)
    VALUES ('manual', '$2', 32736, 1, $4, 'e2e', 'GR', '27', '$1', $3, 60000, 'finished')
    RETURNING id")"
  case "$id" in ''|*[!0-9]*) return 1 ;; esac
  printf '%s' "$id"
}

# insert_original <録画 id> <サイズ> <SHA-256 hex> <置き場> <相対パス> → 論理資産 id
#
# 原本の論理資産と、置き場のコピー（active）を 1 組作る。SHA-256 は ingest が書く値と同じ列に入れる
# （転送は既知の SHA-256 で照合する）。
insert_original() {
  local id
  id="$(psql_scalar "INSERT INTO media_assets (recording_id, kind, size_bytes, sha256)
    VALUES ($1, 'original', $2, decode('$3', 'hex'))
    RETURNING id")"
  case "$id" in ''|*[!0-9]*) return 1 ;; esac
  psql_q "INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
          VALUES (${id}, '$4', '$5', 0, 'active')" >/dev/null || return 1
  printf '%s' "$id"
}

# insert_river_job <キュー> <kind> <args の SQL 式> → ジョブ id（max_attempts 1）
#
# 失敗を再試行で隠さない（discarded で止まる）。投入は DB へ直接。製品の投入経路は
# encode / thumbnail に CLI を持たない（判定 3 の produce_real_encode_job と同じ理由）。
insert_river_job() {
  psql_scalar "INSERT INTO river_job (state, queue, kind, args, max_attempts, priority, scheduled_at)
    VALUES ('available', '$1', '$2', $3, 1, 1, now())
    RETURNING id"
}

# wait_river_job <id> <秒> --- completed なら 0。discarded / cancelled は理由を出して 1。
wait_river_job() {
  local id="$1" timeout="$2" deadline state
  deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    state="$(psql_scalar "SELECT state FROM river_job WHERE id = ${id}")"
    case "$state" in
      completed) return 0 ;;
      discarded|cancelled)
        log_step "river job ${id} は ${state}: $(psql_q "SELECT left(coalesce(errors::text, ''), 400) FROM river_job WHERE id = ${id}" | head -1)"
        return 1 ;;
    esac
    sleep 2
  done
  log_step "river job ${id} が ${timeout}s で完了しない（state=${state}）"
  return 1
}

# seed_original <置き場> <相対パス> <mpeg|text> <印> → stdout "SIZE SHA256"
#
# 置き場の PVC に原本を書く（cluster/store-original-seed-job.yaml）。目印の無い PVC には書かない。
seed_original() {
  local s="$1" rel="$2" kind="$3" name="e2e-06-seed-$4-${1}" out size sha
  k delete job "$name" --ignore-not-found >/dev/null 2>&1 || true
  if ! apply_template "$E2E_DIR/cluster/store-original-seed-job.yaml" \
      -e "s|__JOB__|${name}|g" -e "s|__PVC__|$(store_pvc "$s")|g" \
      -e "s|__ROOT__|$(store_root "$s")|g" -e "s|__REL_PATH__|${rel}|g" -e "s|__KIND__|${kind}|g"; then
    log_step "seed Job ${name} を apply できない"
    return 1
  fi
  if ! wait_job_done "$name" 600; then
    log_step "seed Job ${name} が完了しない: $(k logs "job/${name}" 2>&1 | tail -3 | tr '\n' ' ')"
    return 1
  fi
  out="$(k logs "job/${name}" 2>/dev/null)"
  size="$(printf '%s\n' "$out" | sed -n 's/^SIZE=//p' | tr -d '[:space:]')"
  sha="$(printf '%s\n' "$out" | sed -n 's/^SHA256=//p' | tr -d '[:space:]')"
  case "$size" in ''|*[!0-9]*|0) log_step "seed ${name}: SIZE を読めない（'${size}'）"; return 1 ;; esac
  case "$sha" in ''|*[!0-9a-f]*) log_step "seed ${name}: SHA256 を読めない（'${sha}'）"; return 1 ;; esac
  [ "${#sha}" -eq 64 ] || { log_step "seed ${name}: SHA256 の長さが 64 でない"; return 1; }
  printf '%s %s' "$size" "$sha"
}

# store_file_state <置き場> <相対パス> → stdout "PRESENT SIZE=n SHA256=h" か "ABSENT"
#
# 置き場の PVC を check Job で覗く。目印が無ければ check Job は失敗し、戻り値 1 になる。
store_file_state() {
  local s="$1" rel="$2" name="e2e-06-check-${1}-${RANDOM}${RANDOM}" out
  if ! apply_template "$E2E_DIR/cluster/store-check-job.yaml" \
      -e "s|__JOB__|${name}|g" -e "s|__PVC__|$(store_pvc "$s")|g" \
      -e "s|__ROOT__|$(store_root "$s")|g" -e "s|__REL_PATH__|${rel}|g"; then
    return 1
  fi
  if ! wait_job_done "$name" 300; then
    log_step "check Job ${name} が成功しない: $(k logs "job/${name}" 2>&1 | tail -2 | tr '\n' ' ')"
    return 1
  fi
  out="$(k logs "job/${name}" 2>/dev/null | tail -1)"
  case "$out" in
    ABSENT|PRESENT\ SIZE=*) printf '%s' "$out" ;;
    *) log_step "check Job ${name} の出力を読めない: ${out}"; return 1 ;;
  esac
}

# store_job_evidence <ScaledJob 名> <置き場>
#
# その ScaledJob が起こした Job の**すべて**が、この置き場の PVC を、この置き場の root に挿し、
# `--stores <置き場>` を持つこと。加えて、成功した Job が 1 つ以上あること。
# 1 つでも別の置き場を向いていれば失敗する（ScaledJob のテンプレートの取り違えを捕まえる）。
store_job_evidence() {
  local sj="$1" s="$2" ok facts row name queues stores_arg claims mounts bad=""
  ok="$(k get jobs -l "scaledjob.keda.sh/name=${sj}" -o jsonpath='{.items[?(@.status.succeeded>=1)].metadata.name}' 2>/dev/null)"
  if [ -z "$ok" ]; then
    printf '%s: 成功した Job がない' "$sj"
    return 1
  fi
  if ! facts="$(jobs_pod_facts "$sj")"; then
    printf '%s: Job の Pod テンプレートを読めない' "$sj"
    return 1
  fi
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    IFS=$'\t' read -r name queues stores_arg claims mounts <<<"$row"
    [ "$stores_arg" = "$s" ] || bad="${bad} ${name}:--stores=${stores_arg}"
    [ "$claims" = "$(store_pvc "$s")" ] || bad="${bad} ${name}:PVC=${claims}"
    [ "$mounts" = "$(store_root "$s")" ] || bad="${bad} ${name}:mount=${mounts}"
  done <<<"$facts"
  if [ -n "$bad" ]; then
    printf '%s: 別の置き場を向いた Job がある:%s' "$sj" "$bad"
    return 1
  fi
  printf '%s 成功 Job=%s' "$sj" "$(printf '%s' "$ok" | tr ' ' ',')"
}

# copy_state <論理資産 id> <置き場> → コピーの state（無ければ空）
copy_state() { psql_scalar "SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = '$2'"; }
# copy_rel <論理資産 id> <置き場> → コピーの相対パス
copy_rel() { psql_scalar "SELECT rel_path FROM media_asset_copies WHERE media_asset_id = $1 AND store = '$2'"; }

# serve_status <置き場> <録画 id> → stdout "HTTP番号 バイト数"
#
# Ingress の行き先（rokuban-recording-hls-<置き場>）へ直接叩く。kind には Ingress controller が無い。
serve_status() {
  tb_curl -H 'Host: rokuban.local' -o /dev/null -w '%{http_code} %{size_download}' \
    "http://rokuban-recording-hls-${1}:40773/api/media/stores/${1}/recordings/${2}/file" 2>/dev/null
}

# serve_body_sha <置き場> <録画 id> → 応答の本体の SHA-256（本体はパイプで渡す。bash の変数に載せない）
serve_body_sha() {
  tb_curl -H 'Host: rokuban.local' \
    "http://rokuban-recording-hls-${1}:40773/api/media/stores/${1}/recordings/${2}/file" 2>/dev/null \
    | python3 -c 'import hashlib, sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())'
}

# ---- 6.1 14 本の ScaledJob が 1 本ずつ見つかる（静的）----------------------

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
else
  pass "6.1" "置き場束縛キュー $(printf '%s' "$E2E_STORE_QUEUES" | wc -w | tr -d ' ') 本 x 置き場 2 つの ScaledJob が 1 本ずつある"
fi

# 測定の間 CronJob を止める（置き場束縛の CronJob が本物のジョブを積むと、判定の前提が崩れる）。
suspend_all_cronjobs

# ---- 原本を仕込む ------------------------------------------------------------
#
# 2 本の録画を置く。どちらも ingest を通さない（mirakcmock が録画の実体を配らないため。6.8）。
#   OLD: sitea・放送は 3 日前。規則 `older_than 2d` により desired は takamatsu。
#        原本は tokyo に置く（放送の当時の規則なら tokyo を選ぶ配置）。6.3 が転送し、6.7 が tokyo を消す。
#   ENC: sitea・放送は今。desired は tokyo。原本は tokyo。6.2 が encode / thumbnail を走らせる。
#
# 仕込みに失敗したら、その判定は TODO ではなく FAIL にする（原因の出力が流れた後で赤くなると読めない）。
setup_old_ok=0
setup_enc_ok=0
old_rel="sites/${E2E_SITE_A}/e2e/06-old-${run_id}.bin"
enc_rel="sites/${E2E_SITE_A}/e2e/06-enc-${run_id}.m2ts"
rec_old=""; asset_old=""; old_size=""; old_sha=""
rec_enc=""; asset_enc=""; enc_size=""; enc_sha=""

if old_seed="$(seed_original "$E2E_STORE_A" "$old_rel" text old)"; then
  old_size="${old_seed%% *}"
  old_sha="${old_seed#* }"
  if rec_old="$(insert_recording "e2e 06 old ${run_id}" "$E2E_SITE_A" "now() - interval '3 days'" "$(rand_event_id)")" &&
     asset_old="$(insert_original "$rec_old" "$old_size" "$old_sha" "$E2E_STORE_A" "$old_rel")"; then
    setup_old_ok=1
  fi
fi
if enc_seed="$(seed_original "$E2E_STORE_A" "$enc_rel" mpeg enc)"; then
  enc_size="${enc_seed%% *}"
  enc_sha="${enc_seed#* }"
  if rec_enc="$(insert_recording "e2e 06 encode ${run_id}" "$E2E_SITE_A" "now()" "$(rand_event_id)")" &&
     asset_enc="$(insert_original "$rec_enc" "$enc_size" "$enc_sha" "$E2E_STORE_A" "$enc_rel")"; then
    setup_enc_ok=1
  fi
fi

# ---- 6.4 ingest の Job は両方の置き場を挿す（Pod テンプレート）------------------
#
# 規則が選びうる置き場を全部マウントしていない ingest の Job は、その録画の取り込みだけが失敗する。
# **ここが見るのは Pod テンプレートまで**（取り込みの中身は 6.8）。ingest の Job を起こすために
# probe を 1 つ入れる（e2e_probe は未登録の kind なので、Job は数秒で終わる）。
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

# ---- 6.6 置き場束縛の CronJob（静的）---------------------------------------

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

# ---- 6.2 encode / thumbnail（実物）------------------------------------------
#
# 原本のある置き場（tokyo）の「encode_tokyo」「thumbnail_tokyo」に 1 件ずつ積む。
# 見るもの:
#   - tokyo の ScaledJob が起こした Job（成功したもの）が、すべて tokyo の PVC・tokyo の root・
#     `--stores tokyo` を持つ。**positive**: 成功した Job が存在する。
#   - 派生物（encoded / thumbnail）の行が、派生物の置き場 s3 にだけ active で載る。
#     派生物が FS の置き場へ書かれていれば、FS の copy 行が出て失敗する。
#
# 壊れ方と、それで失敗する場所:
#   - 置き場の取り違え（encode_tokyo の ScaledJob が takamatsu を向く）→ store_job_evidence が失敗
#   - 派生物を FS に書く（storage.derived の取り違え）→ fs_derived_count が 0 でなくなる
#   - S3 の資格情報 / endpoint が違う → worker が派生物の書き込みで失敗し、river job が discarded
#   - encode が原本を見つけられない（copy の store が違う）→ river job が discarded
#   - 派生物の行が作られない（公開を飛ばす）→ derived が 'encoded,thumbnail' にならない
if [ "$setup_enc_ok" != 1 ]; then
  fail "6.2" "原本（ENC）の仕込みに失敗したので encode / thumbnail を測れない"
else
  sj_enc="$(scaledjob_for_queue "encode_${E2E_STORE_A}")"
  sj_thumb="$(scaledjob_for_queue "thumbnail_${E2E_STORE_A}")"
  if discovery_is_unusable "$sj_enc" || discovery_is_unusable "$sj_thumb" || [ -z "$sj_enc" ] || [ -z "$sj_thumb" ]; then
    fail "6.2" "encode_${E2E_STORE_A} / thumbnail_${E2E_STORE_A} の ScaledJob を一意に引けない（enc='${sj_enc}' thumb='${sj_thumb}'）"
  else
    enc_jid="$(insert_river_job "encode_${E2E_STORE_A}" encode \
      "jsonb_build_object('recording_id', ${rec_enc}, 'profile', '${E2E_FAST_PROFILE}', 'store', '${E2E_STORE_A}')")"
    thumb_jid="$(insert_river_job "thumbnail_${E2E_STORE_A}" thumbnail \
      "jsonb_build_object('recording_id', ${rec_enc}, 'store', '${E2E_STORE_A}')")"
    problems=""
    # 投入の失敗を待ちに持ち込まない（空の id で待つと、SQL が壊れたまま上限まで待つ）。
    case "$enc_jid" in ''|*[!0-9]*) problems="${problems} encode の投入に失敗（'${enc_jid}'）" ;; esac
    case "$thumb_jid" in ''|*[!0-9]*) problems="${problems} thumbnail の投入に失敗（'${thumb_jid}'）" ;; esac
    enc_ok=1; thumb_ok=1
    if [ -z "$problems" ]; then
      wait_river_job "$enc_jid" 1200 || enc_ok=0
      wait_river_job "$thumb_jid" 900 || thumb_ok=0
    fi
    if [ "$enc_ok" != 1 ]; then
      problems="${problems} encode(river ${enc_jid}) が完了しない"
    fi
    if [ "$thumb_ok" != 1 ]; then
      problems="${problems} thumbnail(river ${thumb_jid}) が完了しない"
    fi
    ev_enc="$(store_job_evidence "$sj_enc" "$E2E_STORE_A")" || problems="${problems} ${ev_enc}"
    ev_thumb="$(store_job_evidence "$sj_thumb" "$E2E_STORE_A")" || problems="${problems} ${ev_thumb}"
    derived="$(psql_q "SELECT coalesce(string_agg(DISTINCT a.kind, ',' ORDER BY a.kind), '')
        FROM media_asset_copies c JOIN media_assets a ON a.id = c.media_asset_id
        WHERE a.recording_id = ${rec_enc} AND a.kind <> 'original'
          AND c.store = '${E2E_DERIVED_STORE}' AND c.state = 'active'" | head -1 | tr -d '[:space:]')"
    fs_derived="$(psql_scalar "SELECT count(*) FROM media_asset_copies c JOIN media_assets a ON a.id = c.media_asset_id
        WHERE a.recording_id = ${rec_enc} AND a.kind <> 'original'
          AND c.store IN ('${E2E_STORE_A}', '${E2E_STORE_B}')")"
    if [ "$derived" != "encoded,thumbnail" ]; then
      problems="${problems} 派生物（${E2E_DERIVED_STORE}）の行が 'encoded,thumbnail' でない（'${derived}'）"
    fi
    if [ "$fs_derived" != "0" ]; then
      problems="${problems} 派生物の行が FS の置き場に ${fs_derived} 件ある"
    fi
    if [ -n "$problems" ]; then
      fail "6.2" "encode / thumbnail が原本の置き場で動かない:${problems}"
    else
      pass "6.2" "encode / thumbnail は原本の置き場（${E2E_STORE_A}）の Job が実行し（${ev_enc}; ${ev_thumb}）、派生物は ${E2E_DERIVED_STORE} に active で載る（${derived}）"
    fi
  fi
fi

# ---- 6.3 転送（実物）---------------------------------------------------------
#
# 規則で desired が takamatsu になった OLD を、転送 reconcile が takamatsu のキューに載せる。
# 見るもの:
#   - takamatsu の ScaledJob が起こした Job（成功）が、takamatsu の PVC・root・`--stores takamatsu`
#   - takamatsu に active のコピーができ、その中身の SHA-256 と SIZE が原本と一致
#   - 転送の間（overlap の内）は tokyo のコピーが active のまま（転送の完了で元を消さない）
#
# 壊れ方と、それで失敗する場所:
#   - desired の評価が違う（規則を読まない）→ takamatsu のコピーができない（タイムアウト）
#   - 転送の書き先が tokyo → store_job_evidence が「別の置き場を向いた Job」で失敗
#   - 中身が壊れる（コピーが途中で切れる・別の録画の中身）→ SHA-256 / SIZE が一致しない
#   - 転送が元を先に消す → tokyo のコピーが active でなくなる
#   - 正の対照: tokyo の原本の観測（store_file_state）が本物の SHA を返すこと。ここが壊れていれば
#     後段の「中身が一致」は意味を持たない
xfer_ok=0
if [ "$setup_old_ok" != 1 ]; then
  fail "6.3" "原本（OLD）の仕込みに失敗したので転送を測れない"
else
  problems=""
  pos="$(store_file_state "$E2E_STORE_A" "$old_rel")" || pos=""
  if [ "$pos" != "PRESENT SIZE=${old_size} SHA256=${old_sha}" ]; then
    problems="${problems} 正の対照: tokyo の原本の観測が仕込んだ値と一致しない（'${pos}'）"
  fi
  sj_xfer="$(scaledjob_for_queue "transfer_${E2E_STORE_B}")"
  if discovery_is_unusable "$sj_xfer" || [ -z "$sj_xfer" ]; then
    problems="${problems} transfer_${E2E_STORE_B} の ScaledJob を一意に引けない（'${sj_xfer}'）"
  fi
  if ! tb_rokuban enqueue storage-transfer-reconcile >/dev/null 2>&1; then
    problems="${problems} storage-transfer-reconcile を投入できない（CLI が無い版か）"
  fi
  copy_takamatsu_active() { [ "$(copy_state "$asset_old" "$E2E_STORE_B")" = "active" ]; }
  if [ -z "$problems" ] && ! retry_until 1200 "takamatsu copy of asset ${asset_old} to become active" copy_takamatsu_active; then
    problems="${problems} takamatsu にコピーができない（tokyo=$(copy_state "$asset_old" "$E2E_STORE_A") takamatsu=$(copy_state "$asset_old" "$E2E_STORE_B") backlog=$(river_backlog "transfer_${E2E_STORE_B}")）"
  fi
  if [ -z "$problems" ]; then
    ev_xfer="$(store_job_evidence "$sj_xfer" "$E2E_STORE_B")" || problems="${problems} ${ev_xfer}"
    new_rel="$(copy_rel "$asset_old" "$E2E_STORE_B")"
    got="$(store_file_state "$E2E_STORE_B" "$new_rel")" || got=""
    if [ "$got" != "PRESENT SIZE=${old_size} SHA256=${old_sha}" ]; then
      problems="${problems} takamatsu のコピーの中身が原本と一致しない（'${got}'。期待 SIZE=${old_size} SHA256=${old_sha}）"
    fi
    src_state="$(copy_state "$asset_old" "$E2E_STORE_A")"
    if [ "$src_state" != "active" ]; then
      problems="${problems} overlap の内なのに tokyo のコピーが ${src_state} になった"
    fi
  fi
  if [ -n "$problems" ]; then
    fail "6.3" "転送が desired の置き場で動かない:${problems}"
  else
    xfer_ok=1
    pass "6.3" "older_than の録画を転送 reconcile が takamatsu に載せ、takamatsu の Job（${ev_xfer}）がコピーを書き、中身が原本と一致（SIZE=${old_size}）。tokyo は overlap の内なので残る"
  fi
fi

# ---- 6.7 削除（実物）---------------------------------------------------------
#
# OLD の tokyo のコピーは、転送先のコピーができ overlap（config.yml の 32 分）を過ぎると削除対象になる。
# 削除の reconcile は `delete-reconcile --store tokyo` を投入して促す（周期の CronJob は止めてある）。
# 見るもの:
#   - tokyo のコピーが deleted になり、tokyo の PVC からファイルが消えた（ABSENT）
#   - 負の対照: 転送先（takamatsu）のコピーは残り、中身が原本と一致する（desired のコピーを消さない）
#   - 負の対照: 別の録画の tokyo のコピー（ENC。desired は tokyo）は残る
#
# 壊れ方と、それで失敗する場所:
#   - 削除されない（overlap の式・削除 reconcile が動かない）→ 上限まで待って失敗
#   - DB は deleted だが PVC に残る（unlink を省く）→ ABSENT にならない
#   - 転送先を消す（最後のコピーを消す）→ takamatsu のコピーの中身の観測が失敗する
#   - 別録画のコピーを巻き込む → ENC の tokyo のファイルが消える
#   - 目印の無い PVC を見てしまう → store_file_state が失敗（ABSENT と読まない）
if [ "$setup_old_ok" != 1 ]; then
  fail "6.7" "原本（OLD）の仕込みに失敗したので削除を測れない"
elif [ "$xfer_ok" != 1 ]; then
  # 転送先のコピーが無いと、旧コピーは削除対象にならない。上限まで待つと判定の時間を浪費する。
  fail "6.7" "6.3 の転送が成立していないので、旧コピーの削除を測れない（転送先が無ければ tokyo は消えない）"
else
  deadline=$((SECONDS + E2E_DELETE_TIMEOUT))
  last_enqueue=-1000
  state=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    state="$(copy_state "$asset_old" "$E2E_STORE_A")"
    [ "$state" = "deleted" ] && break
    if [ $((SECONDS - last_enqueue)) -ge 120 ]; then
      tb_rokuban enqueue delete-reconcile --store "$E2E_STORE_A" >/dev/null 2>&1 || log_step "delete-reconcile の投入に失敗（待ちは続ける）"
      last_enqueue=$SECONDS
    fi
    sleep 5
  done
  problems=""
  if [ "$state" != "deleted" ]; then
    problems="${problems} tokyo の旧コピーが ${E2E_DELETE_TIMEOUT}s で deleted にならない（state='${state}'）"
  else
    gone="$(store_file_state "$E2E_STORE_A" "$old_rel")" || gone="(観測できない)"
    if [ "$gone" != "ABSENT" ]; then
      problems="${problems} tokyo の PVC に旧コピーが残っている（'${gone}'）"
    fi
    kept="$(store_file_state "$E2E_STORE_B" "$(copy_rel "$asset_old" "$E2E_STORE_B")")" || kept=""
    if [ "$kept" != "PRESENT SIZE=${old_size} SHA256=${old_sha}" ]; then
      problems="${problems} 転送先（takamatsu）のコピーが残っていない、または中身が違う（'${kept}'）"
    fi
    if [ "$setup_enc_ok" = 1 ]; then
      enc_kept="$(store_file_state "$E2E_STORE_A" "$enc_rel")" || enc_kept=""
      if [ "$enc_kept" != "PRESENT SIZE=${enc_size} SHA256=${enc_sha}" ]; then
        problems="${problems} 別録画（ENC）の tokyo のコピーが巻き込まれた（'${enc_kept}'）"
      fi
    fi
  fi
  if [ -n "$problems" ]; then
    fail "6.7" "旧コピーの削除が成立しない:${problems}"
  else
    pass "6.7" "overlap を過ぎた tokyo の旧コピーが deleted になり、PVC からファイルが消えた。takamatsu のコピーと別録画の tokyo のコピーは残った"
  fi
fi

# ---- 6.5 配信（実物）---------------------------------------------------------
#
# Ingress の行き先（置き場ごとの Service）を直接叩く。見るもの:
#   - OLD: takamatsu は 200 で中身の SHA-256 が原本と一致。tokyo は 404（旧コピーは deleted）
#   - ENC: tokyo は 200 でバイト数が原本と一致。takamatsu は 404（コピーが無い置き場）
#
# 壊れ方と、それで失敗する場所:
#   - 配信が別の置き場の原本を返す（Service の取り違え）→ 404 の側が 200 になって失敗
#   - 配信が中身を読み違える（コピーの rel_path を取り違える）→ SHA-256 / バイト数が一致しない
#   - 配信がコピーの無い置き場で 200 を返す（状態を見ない）→ 404 の期待で失敗
if [ "$setup_old_ok" != 1 ] && [ "$setup_enc_ok" != 1 ]; then
  fail "6.5" "原本の仕込みに失敗したので配信を測れない"
else
  problems=""
  if [ "$setup_old_ok" = 1 ]; then
    o="$(serve_status "$E2E_STORE_B" "$rec_old")"
    if [ "$o" != "200 ${old_size}" ]; then
      problems="${problems} ${E2E_STORE_B} の録画 ${rec_old}: 期待 200 ${old_size}、実際 '${o}'"
    elif [ "$(serve_body_sha "$E2E_STORE_B" "$rec_old")" != "$old_sha" ]; then
      problems="${problems} ${E2E_STORE_B} の録画 ${rec_old}: 本体の SHA-256 が原本と違う"
    fi
    o="$(serve_status "$E2E_STORE_A" "$rec_old")"
    [ "${o%% *}" = "404" ] || problems="${problems} ${E2E_STORE_A} は旧コピー無しで ${rec_old} に ${o} を返した（期待 404）"
  fi
  if [ "$setup_enc_ok" = 1 ]; then
    e="$(serve_status "$E2E_STORE_A" "$rec_enc")"
    [ "$e" = "200 ${enc_size}" ] || problems="${problems} ${E2E_STORE_A} の録画 ${rec_enc}: 期待 200 ${enc_size}、実際 '${e}'"
    e="$(serve_status "$E2E_STORE_B" "$rec_enc")"
    [ "${e%% *}" = "404" ] || problems="${problems} ${E2E_STORE_B} はコピー無しで ${rec_enc} に ${e} を返した（期待 404）"
  fi
  if [ -n "$problems" ]; then
    fail "6.5" "置き場ごとの配信が原本の置き場の中身を返さない:${problems}"
  else
    pass "6.5" "配信はコピーのある置き場の中身を返し（OLD: ${E2E_STORE_B} 200 + SHA-256 一致 / ENC: ${E2E_STORE_A} 200 + バイト数一致）、無い置き場は 404 を返す"
  fi
fi

# ---- 6.8 ingest（実物）------------------------------------------------------
#
# site B の録画を 1 件注入する（mirakcmock の POST /mock/records。放送は流れないので、終わった録画を
# 置く）。そこから製品の経路をそのまま回す:
#   record-sweep（watcher の sweep ジョブ）→ ProcessRecord（録画行）→ ingest ジョブ → ingest の Job
# 見るもの:
#   - watcher が録画行を作り、record_sync の recording_id が立つ
#   - 原本の copy 行が、規則（siteb → takamatsu）の置き場に active で載り、tokyo には無い
#   - その copy の中身が注入した TS と SIZE・SHA-256 が一致（置き場の PVC を check Job で観測）。
#     media_assets.sha256 の列も同じ値
#   - ingest_siteb の ScaledJob が起こした、成功した Job が存在し、その Pod テンプレートが
#     両方の置き場の PVC と root を挿している（取り込みを書いた Pod がその置き場を挿していた）
#
# 壊れ方と、それで失敗する場所:
#   - 録画が取り込まれない（watcher が録画を Managed と見ない = タグの取り違え）→ recording_id が立たない
#   - 規則を読まない（tokyo に書く）→ copy の置き場が takamatsu:active と違う
#   - 中身が壊れる（途中で切れる・別の録画を書く）→ SIZE / SHA-256 が一致しない
#   - Range の扱いが壊れる（取り込みの差分転送）→ 中身の SIZE / SHA-256 が一致しない
#   - ingest の Pod が両方の置き場を挿していない → 下の Pod テンプレートの照合が失敗
#   - 正の対照: 注入の後で、置き場の観測が注入した値そのものを返すこと（check Job 自体の検証）
#
# **限界**: 「書いた Pod がその置き場を挿していた」は、ingest の Job のテンプレートと Job の成功で見る。
# Job と録画の対応は DB に無いので、1 つの Job がどの録画を書いたかは観測しない。
ing_msg=""
run_ingest_check() {
  local dir="$1" ts size sha pid out rec_mirakc asset_id sj_ing ok facts row name queues stores_arg claims mounts
  ts="${dir}/record.ts"
  if ! docker run --rm --entrypoint ffmpeg -v "${dir}:/out" "$E2E_FULL_IMAGE" -nostdin -loglevel error -y \
      -f lavfi -i "testsrc2=size=320x240:rate=25" \
      -f lavfi -i "sine=frequency=440:sample_rate=48000" \
      -t 15 -c:v mpeg2video -b:v 800k -c:a aac -f mpegts /out/record.ts >/dev/null 2>&1 || [ ! -s "$ts" ]; then
    ing_msg="録画の TS を ffmpeg で作れない（${E2E_FULL_IMAGE} の docker run）"
    return 1
  fi
  size="$(wc -c < "$ts" | tr -d '[:space:]')"
  sha="$(python3 -c 'import hashlib, sys; print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())' "$ts")"

  pid="$(tb_curl "http://mirakc-${E2E_SITE_B}:40772/api/programs" 2>/dev/null |
    python3 -c 'import json, sys; print(json.load(sys.stdin)[0]["id"])' 2>/dev/null)"
  case "$pid" in ''|*[!0-9]*) ing_msg="siteb の番組を mirakc モックから取れない（'${pid}'）"; return 1 ;; esac

  if ! out="$(k exec -i "$E2E_TOOLBOX" -- curl -sS -f -X POST --data-binary @- \
      "http://mirakc-${E2E_SITE_B}:40772/mock/records?programId=${pid}" < "$ts" 2>&1)"; then
    ing_msg="録画の注入（POST /mock/records）が失敗: ${out}"
    return 1
  fi
  rec_mirakc="$(printf '%s' "$out" | python3 -c 'import json, sys; print(json.load(sys.stdin)["id"])' 2>/dev/null)"
  case "$rec_mirakc" in ''|*[!A-Za-z0-9-]*) ing_msg="注入の応答から録画 id を読めない（'${out}'）"; return 1 ;; esac

  # watcher の sweep（level-triggered の真実）を促す。SSE の保存イベントで先に取り込まれても、同じ録画
  # 行に合流するので害は無い。
  if ! tb_rokuban enqueue record-sweep --site "$E2E_SITE_B" >/dev/null 2>&1; then
    log_step "record-sweep の投入に失敗（SSE の保存イベントに任せる）"
  fi

  # 1. 録画行（record_sync の recording_id）
  linked() { [ -n "$(psql_scalar "SELECT coalesce(recording_id::text, '') FROM record_sync WHERE site = '${E2E_SITE_B}' AND record_id = '${rec_mirakc}'")" ]; }
  if ! retry_until 600 "record_sync of ${rec_mirakc} to get a recording" linked; then
    ing_msg="録画 ${rec_mirakc} に録画行ができない（watcher が取り込み対象と見ない: record_sync=$(psql_scalar "SELECT count(*) FROM record_sync WHERE record_id = '${rec_mirakc}'")）"
    return 1
  fi
  ing_rec="$(psql_scalar "SELECT recording_id FROM record_sync WHERE site = '${E2E_SITE_B}' AND record_id = '${rec_mirakc}'")"

  # 2. 原本の copy 行（規則が選んだ置き場に active）。
  asset_id=""
  copy_ready() {
    asset_id="$(psql_scalar "SELECT id FROM media_assets WHERE recording_id = ${ing_rec} AND kind = 'original'")"
    [ -n "$asset_id" ] && [ -n "$(psql_scalar "SELECT 1 FROM media_asset_copies WHERE media_asset_id = ${asset_id} AND state = 'active' LIMIT 1")" ]
  }
  if ! retry_until 900 "original copy of recording ${ing_rec} to be active" copy_ready; then
    ing_msg="録画 ${ing_rec} の原本の copy が active にならない（media_assets=${asset_id:-なし}）"
    return 1
  fi
  copies="$(psql_q "SELECT store || ':' || state FROM media_asset_copies WHERE media_asset_id = ${asset_id} ORDER BY store" | paste -sd, -)"
  if [ "$copies" != "${E2E_STORE_B}:active" ]; then
    ing_msg="原本の copy が規則（${E2E_SITE_B} → ${E2E_STORE_B}）の置き場に無い: '${copies}'"
    return 1
  fi
  if [ "$(psql_scalar "SELECT encode(sha256, 'hex') FROM media_assets WHERE id = ${asset_id}")" != "$sha" ]; then
    ing_msg="media_assets.sha256 が注入した TS の SHA-256 と違う"
    return 1
  fi

  # 3. 中身（置き場の PVC を check Job で観測。正の対照は同じ観測手段の 6.3 が済ませている）。
  rel="$(copy_rel "$asset_id" "$E2E_STORE_B")"
  got="$(store_file_state "$E2E_STORE_B" "$rel")" || got=""
  if [ "$got" != "PRESENT SIZE=${size} SHA256=${sha}" ]; then
    ing_msg="取り込んだ原本の中身が注入した TS と一致しない（'${got}'。期待 SIZE=${size} SHA256=${sha}）"
    return 1
  fi

  # 4. 取り込みを書いた ingest の Job が両方の置き場を挿している。
  sj_ing="$(scaledjob_for_queue "ingest_${E2E_SITE_B}")"
  if [ -z "$sj_ing" ] || discovery_is_unusable "$sj_ing"; then
    ing_msg="ingest_${E2E_SITE_B} の ScaledJob を一意に引けない（'${sj_ing}'）"
    return 1
  fi
  ok="$(k get jobs -l "scaledjob.keda.sh/name=${sj_ing}" -o jsonpath='{.items[?(@.status.succeeded>=1)].metadata.name}' 2>/dev/null)"
  if [ -z "$ok" ]; then
    ing_msg="ingest の Job（${sj_ing}）が成功していない"
    return 1
  fi
  facts="$(jobs_pod_facts "$sj_ing")" || facts=""
  bad=""
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    IFS=$'\t' read -r name queues stores_arg claims mounts <<<"$row"
    [ "$claims" = "$want_claims" ] || bad="${bad} ${name}:PVC=${claims}"
    [ "$mounts" = "$want_mounts" ] || bad="${bad} ${name}:mount=${mounts}"
  done <<<"$facts"
  if [ -n "$bad" ]; then
    ing_msg="ingest の Job が両方の置き場を挿していない:${bad}"
    return 1
  fi

  ing_msg="録画 ${rec_mirakc}（recording ${ing_rec}）の原本は規則どおり ${E2E_STORE_B} に active で載り、中身が注入した TS と一致（SIZE=${size}, SHA-256 一致）。取り込みは ${ok} が書いた"
  return 0
}

ing_dir="$(mktemp -d)" || ing_dir=""
if [ -z "$ing_dir" ]; then
  fail "6.8" "一時ディレクトリを作れない（録画の TS を置けない）"
else
  if run_ingest_check "$ing_dir"; then
    pass "6.8" "$ing_msg"
  else
    fail "6.8" "ingest が録画を規則どおりの置き場へ取り込まない: ${ing_msg}"
  fi
  rm -rf "$ing_dir"
fi

exit 0
