# shellcheck shell=bash
#
# クラスタを触るための共通関数。
#
# **時間で待たない。** ここにある待ちはすべて「条件が満たされるまでポーリング /
# watch し、上限時間で諦める」形にしてある。`sleep N` してからアサートする形は
# 書かないこと --- k8s は「まだ何も起きていない」と「正常」の区別が付きにくいので、
# 固定 sleep は非同期の空虚な成功をそのまま通す（CLAUDE.md §テスト規律）。
#
# 唯一の例外は **negative assertion**（「起きないこと」の判定）で、これは
# 原理的に窓を取るしかない。その場合は必ず `positive control`（同じ観測手段で
# 「起きること」も見えると示す）を同じ判定の中に置く。窓だけを見て緑にすると、
# 観測手段が壊れていても緑になる。

# k は名前空間を固定した kubectl。
k() {
  kubectl --context "kind-${E2E_CLUSTER}" -n "$E2E_NAMESPACE" "$@"
}

# kall は名前空間に属さない資源（node / CRD / keda 名前空間）を触る kubectl。
kall() {
  kubectl --context "kind-${E2E_CLUSTER}" "$@"
}

# psql は使い捨て postgres へ SQL を投げる。`-tAc` なので 1 行 1 値で返る。
psql_q() {
  k exec deploy/postgres -- env PGPASSWORD="$E2E_PGPASSWORD" \
    psql -U rokuban -d rokuban -tAc "$1"
}

# tb_curl はツールボックス Pod から HTTP を叩く。
#
# 製品の Pod（api 等）から叩かないのは、判定の観測手段が判定対象の生死に
# 巻き込まれないようにするため。ツールボックスは rokuban のイメージ
# （curl 同梱）を `sleep infinity` で起動したもの。
tb_curl() {
  k exec "$E2E_TOOLBOX" -- curl -sS --max-time 10 "$@"
}

# tb_rokuban はツールボックス Pod で rokuban のサブコマンドを実行する
# （`enqueue` など）。config は製品と同じ ConfigMap を読む。
tb_rokuban() {
  k exec "$E2E_TOOLBOX" -- rokuban "$@" --config /etc/rokuban/config.yml
}

# retry_until <timeout_sec> <description> <cmd...>
#
# cmd が 0 を返すまで 1 秒間隔で繰り返す。返さないまま上限を過ぎたら 1 を返す。
retry_until() {
  local timeout="$1" desc="$2"
  shift 2
  local deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  log_step "timed out after ${timeout}s waiting for: ${desc}"
  return 1
}

# deployment_exists <name>
deployment_exists() {
  k get deployment "$1" >/dev/null 2>&1
}

# ready_replicas <deployment> --- Ready なレプリカ数（無ければ空文字）。
ready_replicas() {
  k get deployment "$1" -o jsonpath='{.status.readyReplicas}' 2>/dev/null
}

# deployments_with_component <component> --- ラベルで役を引く。
#
# **名前ではなくラベルで引く。** マニフェストの名前は、残りのワークロードを書く人が決めることなので、
# ここで名前を固定するとハーネスが「命名の合意」まで要求してしまう
# （不変条件 11: 形を固定する前に判定基準を書く）。ラベル
# `app.kubernetes.io/component` は base が既に採っている規約である。
deployments_with_component() {
  k get deployments -l "app.kubernetes.io/name=rokuban,app.kubernetes.io/component=$1" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null
}

# deployment_for_component_site <component> <site>
#
# 役とサイトで Deployment を引く。サイトの同定は**ラベルではなく argv**
# （`--sites <site>`）で行う --- サイト束縛は argv で決まる（config キーではない）ので、
# ラベルを正にするとラベルと argv がズレたときに判定が argv 側の真実を見失う。
#
# 戻り値の約束は scaledjob_for_queue と同じ（一意なら名前 / 0 件なら空 /
# 複数一致なら AMBIGUOUS）。**ここだけ黙って 1 つ目を選ぶ形にしない** ---
# 3 つある探索関数で規約が揃っていないと、README の「一致が複数あったら
# FAIL にする」が 1 つの関数についてだけ偽になる。
deployment_for_component_site() {
  local component="$1" site="$2" matches count
  matches="$(k get deployments -l "app.kubernetes.io/name=rokuban,app.kubernetes.io/component=$component" \
    -o json 2>/dev/null | python3 -c '
import json, sys
site = sys.argv[1]
doc = json.load(sys.stdin)
for item in doc.get("items", []):
    for c in item["spec"]["template"]["spec"].get("containers", []):
        argv = c.get("command", []) + c.get("args", [])
        # **区切りまで見る。** 部分一致にすると、互いに接頭辞になっている
        # サイト名（home と home2）で取り違える。
        hit = any(
            (a == "--sites" and i + 1 < len(argv) and argv[i + 1] == site)
            or a == f"--sites={site}"
            for i, a in enumerate(argv)
        )
        if hit:
            print(item["metadata"]["name"])
            break
' "$site" | tr '\n' ' ' | sed 's/ *$//')"
  count="$(printf '%s' "$matches" | wc -w | tr -d ' ')"
  if [ "$count" -gt 1 ]; then
    printf '%s%s' "$discoveryAmbiguousPrefix" "$matches"
    return 0
  fi
  printf '%s' "$matches"
}

# keda_installed --- KEDA の CRD が入っているか。
keda_installed() {
  kall get crd scaledjobs.keda.sh >/dev/null 2>&1
}

# scaledjob_for_queue <river_queue>
#
# キュー名（`epg_sitea` 等）に言及している ScaledJob を引く。名前を固定しない
# のは deployments_with_component と同じ理由（命名は書く人が決める）。
#
# **オブジェクト全体を見る。トリガのクエリだけを見ない。** 最初はクエリ本文
# だけで引いていたが、それだと「クエリを壊す」変異を当てた瞬間に
# **対象が見つからなくなり、判定が FAIL ではなく TODO に化けた**（実測。
# オラクル検査 O2.mut-trigger が「判定 2.4 は 'TODO'（期待 'FAIL'）」で落ちた）。
# 壊れているものを「まだ実装されていない」と報告する判定は、壊れていることを
# 隠す。名前・ラベル・argv・クエリのどれか 1 つでも残っていれば拾う。
#
# `-` と `_` は同一視する。River のキュー名は `epg_sitea` だが、k8s の名前は
# `-` しか使えないので `...-epg-sitea` になる。
#
# **一致が複数あったら全部返す。** 呼び出し側（scaledjob_for_queue）が
# それを曖昧として落とす --- 黙って 1 つ目を選ぶと、`encode` のような一般的な
# 語が別の ScaledJob の image 名や last-applied-configuration に紛れていた
# ときに、**無関係なオブジェクトを判定対象にしてそのことを報告しない**。
scaledjobs_matching_queue() {
  local queue="$1"
  keda_installed || return 0
  k get scaledjobs -o json 2>/dev/null | python3 -c '
import json, sys
queue = sys.argv[1].replace("-", "_")
doc = json.load(sys.stdin)
for item in doc.get("items", []):
    item["metadata"].pop("annotations", None)
    item["metadata"].pop("managedFields", None)
    blob = json.dumps(item).replace("-", "_")
    if queue in blob:
        print(item["metadata"]["name"])
' "$queue"
}

# 曖昧さの伝え方。
#
# **戻り値は必ず stdout に載せる。** 最初は大域変数（E2E_DISCOVERY_ERROR）に
# 理由を置いていたが、呼び出しは全部 `x="$(discover ...)"` の形なので
# **代入が子シェルに閉じて親へ 1 度も届いていなかった**（実測:
# `f() { E2E_DISCOVERY_ERROR=boom; }; x="$(f)"` の後、親では未設定）。
# 結果、「複数一致は FAIL」という README の主張は一度も真になっておらず、
# 曖昧なときは空文字＝「まだ実装されていない」として TODO に化けていた。
# 判定側が一番避けたい壊れ方（壊れているのに TODO）そのものだったので、
# 値の通り道を stdout 1 本に統一する。
discoveryAmbiguousPrefix="AMBIGUOUS	"

# discovery_is_ambiguous <戻り値>
discovery_is_ambiguous() {
  case "$1" in "${discoveryAmbiguousPrefix}"*) return 0 ;; *) return 1 ;; esac
}

# discovery_detail <戻り値> --- 曖昧だったときの説明（一致した名前の並び）。
discovery_detail() {
  printf '%s' "${1#"${discoveryAmbiguousPrefix}"}"
}

# scaledjob_for_queue <river_queue>
#
# 一意に決まれば名前、0 件なら空、複数一致なら `AMBIGUOUS<TAB><名前...>`。
# 呼び出し側は discovery_is_ambiguous で見て **TODO ではなく FAIL** にすること。
scaledjob_for_queue() {
  local queue="$1" matches count
  matches="$(scaledjobs_matching_queue "$queue" | tr '\n' ' ' | sed 's/ *$//')"
  count="$(printf '%s' "$matches" | wc -w | tr -d ' ')"
  if [ "$count" -gt 1 ]; then
    printf '%s%s' "$discoveryAmbiguousPrefix" "$matches"
    return 0
  fi
  printf '%s' "$matches"
}

# cronjob_enqueueing <job_name> [site]
#
# `rokuban enqueue <job_name> --site <site>` を打つ CronJob を argv で引く
# （名前で引かないのは scaledjob_for_queue と同じ理由）。戻り値の約束も同じ
# （一意なら名前 / 0 件なら空 / 複数一致なら AMBIGUOUS）。
#
# **argv を連結した文字列への部分一致にしない。** 連結すると
# `--sites home` の判定が `--sites home2` の CronJob を拾い、`enqueue` を
# 含むだけの無関係な CronJob も拾う。要素単位で照合する。
cronjob_enqueueing() {
  local job="$1" site="${2:-}" matches count
  matches="$(k get cronjobs -o json 2>/dev/null | python3 -c '
import json, sys
job, site = sys.argv[1], sys.argv[2]
doc = json.load(sys.stdin)
for item in doc.get("items", []):
    spec = item["spec"]["jobTemplate"]["spec"]["template"]["spec"]
    for c in spec.get("containers", []):
        argv = c.get("command", []) + c.get("args", [])
        if "enqueue" not in argv or job not in argv:
            continue
        if site:
            ok = any(
                (a == "--site" and i + 1 < len(argv) and argv[i + 1] == site)
                or a == f"--site={site}"
                for i, a in enumerate(argv)
            )
            if not ok:
                continue
        print(item["metadata"]["name"])
        break
' "$job" "$site" | tr '\n' ' ' | sed 's/ *$//')"
  count="$(printf '%s' "$matches" | wc -w | tr -d ' ')"
  if [ "$count" -gt 1 ]; then
    printf '%s%s' "$discoveryAmbiguousPrefix" "$matches"
    return 0
  fi
  printf '%s' "$matches"
}

# jobs_owned_by_scaledjob <scaledjob> --- KEDA が起こした Job の名前一覧。
#
# KEDA は起こした Job に `scaledjob.keda.sh/name` ラベルを付ける。
jobs_owned_by_scaledjob() {
  k get jobs -l "scaledjob.keda.sh/name=$1" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null
}

# watch_jobs_start <scaledjob> <outfile>
#
# **観測を先に始めるための関数。** KEDA が Job を起こして消える 0 → 1 → 0 は
# 速いことがあるので、引き金を引く前にこれを呼び、後から outfile を読む。
# 戻り値として watch プロセスの PID を大域変数 E2E_WATCH_PID に置く。
#
# **`--watch-only` にしてある（`--watch` ではない）。** `--watch` は開始時に
# 既存オブジェクトを ADDED として全部吐くので、KEDA が残した前回の完走 Job
# （`successfulJobsHistoryLimit` の既定は 100）がそのまま「今回起きた Job」に
# 見える。クラスタを使い回す設計なので、2 回目以降の実行では
# **「KEDA が Job を起こしたが消化できずに固まっている」という判定 2 が
# 捕まえるべき失敗が、watch 開始直後に緑になってしまう。**
#
# それでも取りこぼしを防ぐため、開始時点の Job 名を `<outfile>.pre` に保存し、
# 判定は「pre に無い名前」だけを新規として数える（observed_new_*）。
watch_jobs_start() {
  local scaledjob="$1" outfile="$2"
  : >"$outfile"
  # **番兵を 1 行置く。** observed_new_jobs は awk の `NR == FNR` で 1 つ目の
  # ファイル（この .pre）を読み分けるが、**空ファイルだとその判定が 2 つ目の
  # ファイルの 1 行目にも当たり、watch の中身がまるごと「既存」として
  # 飲み込まれる**。実測: この番兵が無いとき、KEDA が起こした Job が
  # ADDED → active=1 → succeeded=1 と watch に出ているのに
  # observed_new_jobs が空を返し、判定 2.4 が誤 FAIL した。
  printf '# pre-existing jobs (sentinel keeps this file non-empty)\n' >"${outfile}.pre"
  k get jobs -l "scaledjob.keda.sh/name=${scaledjob}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null >>"${outfile}.pre"
  # **stderr を混ぜない。** 混ぜると kubectl の診断行（`E0827 ... watch of
  # *v1.Job ended with: ...`）の 2 列目が Job 名として読まれ、判定 5.2
  # （「A の Job が起きないこと」）が幻の Job で誤 FAIL する（実測で再現）。
  k get jobs -l "scaledjob.keda.sh/name=${scaledjob}" \
    --watch-only --output-watch-events \
    -o 'jsonpath={.type}{" "}{.object.metadata.name}{" active="}{.object.status.active}{" succeeded="}{.object.status.succeeded}{" failed="}{.object.status.failed}{"\n"}' \
    >>"$outfile" 2>>"${outfile}.err" &
  E2E_WATCH_PID=$!
}

watch_jobs_stop() {
  if [ -n "${E2E_WATCH_PID:-}" ]; then
    kill "$E2E_WATCH_PID" 2>/dev/null || true
    wait "$E2E_WATCH_PID" 2>/dev/null || true
    E2E_WATCH_PID=""
  fi
}

# observed_new_jobs <outfile> [field]
#
# watch のログから、**観測開始時点に無かった** Job の名前を返す。field を渡すと
# その項目が 1 以上になった Job だけに絞る（`active` / `succeeded` / `failed`）。
observed_new_jobs() {
  local outfile="$1" field="${2:-}"
  [ -f "$outfile" ] && [ -f "${outfile}.pre" ] || return 0
  # **行の型を固定する**（1 列目が watch イベント種別であることを要求）。
  # 万一 stderr や別形式の行が混ざっても Job 名として数えない。
  awk -v field="$field" '
    NR == FNR { pre[$0] = 1; next }
    $1 != "ADDED" && $1 != "MODIFIED" && $1 != "DELETED" { next }
    {
      name = $2
      if (name == "" || name in pre) next
      if (field != "") {
        want = field "=[1-9]"
        if ($0 !~ want) next
      }
      if (!(name in seen)) { seen[name] = 1; print name }
    }
  ' "${outfile}.pre" "$outfile"
}

# observed_new_job_reached <outfile> <field> <name>
observed_new_job_reached() {
  observed_new_jobs "$1" "$2" | grep -qx "$3"
}

# riverBacklogStates は「待っている」とみなす River の状態。
#
# **KEDA のトリガが数える集合とここは同じでなければならない。** ずれると、
# ハーネスだけが待ち続ける（トリガは 0 なのでスケールしないのが正しいのに、
# ハーネスは滞留があると思って 0 → 1 → 0 を待つ）。`scheduled` を入れて
# いたときは、失敗して指数バックオフ中のジョブ 1 件で判定 2 が誤 FAIL、
# 判定 5 が「滞留を作れない」で落ちる形になっていた。
#
# **本物の ScaledJob への契約でもある**: そちらのトリガもこの集合を数えること。
riverBacklogStates="'available','retryable'"

# river_backlog <queue> --- 待ち行列にいるジョブ数（KEDA のトリガと同じ条件）。
river_backlog() {
  psql_q "SELECT count(*) FROM river_job WHERE queue = '$1' AND state IN (${riverBacklogStates})" \
    | tr -d '[:space:]'
}

# insert_probe_job <queue> [count]
#
# River のキューに「待っているジョブ」を直に作る。**製品の投入経路
# （`rokuban enqueue`）ではなく DB へ直接 INSERT する**のは、判定の対象が
# 「キューに滞留があるときのスケーラの挙動」であって投入経路ではないため。
# 経路を通すと UniqueOpts（epg_sync は ByQueue）で 2 件目以降が黙って消え、
# 「滞留を作ったつもりで 1 件しか無い」になる。
#
# kind は e2e_probe。rokuban に登録されたワーカーが無い名前なので、実物の
# worker が掴んでも実データには触れない**はず**だが、**未検証**である
# （実物の worker が居る状態でハーネスを回したことがまだ無い）。worker の
# ワークロードを足すときに最初に確かめること --- 破れると判定 5 の前提（A の待ち行列が空）が崩れる。
insert_probe_job() {
  local queue="$1" count="${2:-1}"
  psql_q "INSERT INTO river_job (state, queue, kind, args, max_attempts, priority, scheduled_at)
          SELECT 'available', '$queue', 'e2e_probe', '{}'::jsonb, 1, 1, now()
          FROM generate_series(1, $count)" >/dev/null
}

# drain_queue <queue> --- そのキューの**待ち**を completed にして消す。
#
# running は触らない。触ると判定 3（実行中の Job が殺されないこと）で、
# ハーネス自身が実行中のジョブを終わったことにしてしまう。
drain_queue() {
  psql_q "UPDATE river_job SET state = 'completed', finalized_at = now()
          WHERE queue = '$1' AND state IN ('available','retryable','scheduled')" >/dev/null
}

# suspend_all_cronjobs / restore_cronjobs
#
# 測定の間だけ CronJob を止める。
#
# **止めないと「起きないこと」の判定が測れない。** 判定 5 はサイト A の
# 待ち行列が空であることを前提にしているが、A 向けの CronJob が動いていると
# 測定窓の中で A が正当に投入され、正当に Job が起きる --- それを
# 「B の滞留で A が起きた」と読んでしまう（実測: オラクル検査 O5.good が、
# 判定 2 の fixture CronJob（毎分 epg-sync）に反応して赤くなった）。
#
# **元から suspend されていたものは戻さない**（戻すと、判定が止めたつもりの
# ない CronJob を動かし始める）。区別は**クラスタ側の annotation**
# `rokuban-e2e/suspended-by-harness` で持つ --- シェル変数に持つと、判定 5 の
# 途中で中断されたときに restore が走らず、名前空間のすべての CronJob が
# 止まったまま残る。しかも次回の実行は「自分が止めたもの」と見なさないので
# 復旧もせず、判定 2 が人間の手が入るまで赤くなり続ける。
suspendedByHarnessAnnotation="rokuban-e2e/suspended-by-harness"

suspend_all_cronjobs() {
  local name
  for name in $(k get cronjobs -o jsonpath='{.items[?(@.spec.suspend!=true)].metadata.name}' 2>/dev/null); do
    k annotate cronjob "$name" "${suspendedByHarnessAnnotation}=true" --overwrite >/dev/null 2>&1 || continue
    k patch cronjob "$name" -p '{"spec":{"suspend":true}}' >/dev/null 2>&1 || true
  done
  local suspended
  suspended="$(k get cronjobs -o json 2>/dev/null | python3 -c '
import json, sys
doc = json.load(sys.stdin)
print(" ".join(i["metadata"]["name"] for i in doc.get("items", [])
                if i["metadata"].get("annotations", {}).get(sys.argv[1]) == "true"))
' "$suspendedByHarnessAnnotation")"
  if [ -n "$suspended" ]; then
    log_step "suspended cronjobs for the measurement: ${suspended}"
  fi
}

# restore_cronjobs は**ハーネスが止めたものだけ**を戻す。run.sh の preflight
# からも呼ぶので、前回の中断で止まったままの CronJob もここで戻る。
restore_cronjobs() {
  local name
  for name in $(k get cronjobs -o json 2>/dev/null | python3 -c '
import json, sys
doc = json.load(sys.stdin)
print(" ".join(i["metadata"]["name"] for i in doc.get("items", [])
                if i["metadata"].get("annotations", {}).get(sys.argv[1]) == "true"))
' "$suspendedByHarnessAnnotation"); do
    k patch cronjob "$name" -p '{"spec":{"suspend":false}}' >/dev/null 2>&1 || true
    k annotate cronjob "$name" "${suspendedByHarnessAnnotation}-" >/dev/null 2>&1 || true
  done
}

# scaledjob_pause <name> <true|false> --- KEDA のスケーリングを止める / 戻す。
scaledjob_pause() {
  if [ "$2" = "true" ]; then
    k annotate scaledjob "$1" autoscaling.keda.sh/paused=true --overwrite >/dev/null
  else
    k annotate scaledjob "$1" autoscaling.keda.sh/paused- >/dev/null 2>&1 || true
  fi
}

# apply_template <file> [extra sed expr...]
#
# `__SITE_A__` / `__SITE_B__` / `__IMAGE__` / `__MOCK_IMAGE__` を lib/env.sh の値に
# 置き換えて apply
# する。fixture / mutant にサイト名を直書きすると、env.sh を変えたときに
# fixture 側だけが古いサイト名を指し、判定が「対象が無い」（TODO）に化ける。
apply_template() {
  local file="$1"
  shift
  sed -e "s/__SITE_A__/${E2E_SITE_A}/g" \
      -e "s/__SITE_B__/${E2E_SITE_B}/g" \
      -e "s|__IMAGE__|${E2E_IMAGE}|g" \
      -e "s|__MOCK_IMAGE__|${E2E_MOCK_IMAGE}|g" "$@" "$file" | k apply -f - >/dev/null
}

# delete_template <file> --- apply_template で当てたものを消す。
delete_template() {
  local file="$1"
  shift
  sed -e "s/__SITE_A__/${E2E_SITE_A}/g" \
      -e "s/__SITE_B__/${E2E_SITE_B}/g" \
      -e "s|__IMAGE__|${E2E_IMAGE}|g" \
      -e "s|__MOCK_IMAGE__|${E2E_MOCK_IMAGE}|g" "$@" "$file" \
    | k delete --ignore-not-found -f - >/dev/null 2>&1 || true
}

# mock_stat <site> <field> --- mirakc モックの統計値を読む。
#
# **読めなかったときは空文字を返す。** 呼び出し側で `${x:-0}` と潰さないこと
# --- 潰すと「観測できなかった」が「0 だった」になり、判定 4 のように最大値を
# 見る判定が黙って緑になる。
mock_stat() {
  tb_curl "http://mirakc-$1:40772/mock/stats" 2>/dev/null | python3 -c '
import json, sys
print(json.load(sys.stdin)[sys.argv[1]])
' "$2" 2>/dev/null
}

# mock_reset <site> --- モックの録画予約を空に戻す（/events のカウンタは残る）。
#
# **周回ごとに呼ぶ。** モックは Pod が生きている限り予約を持ち続けるので、
# 前回届いた予約が残っていると判定 1.7 が「今回 1 件も送っていないのに緑」に
# なる。
mock_reset() {
  tb_curl -X POST "http://mirakc-$1:40772/mock/reset" >/dev/null
}
