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
  k get deployments -l "app.kubernetes.io/name=rokuban,app.kubernetes.io/component=$1${E2E_FIXTURE_SCOPE:+,rokuban-e2e/fixture=true}" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null
}

# E2E_FIXTURE_SCOPE が立っている間、探索は**身代わりだけ**を見る。
#
# オラクル自己検査の身代わりは製品と同じ役ラベル・同じキュー名を名乗るので、
# 製品のワークロードが入ると両方が一致して全部 AMBIGUOUS になる ---
# **判定の有効性を確かめる唯一の手段が、確かめたい時（ワークロードを足す PR）に
# 使えなくなる。** 常時除外にはしない（それをすると通常実行が身代わりを
# 見落とし、preflight 0.5 の意味が消える）。

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
  local json
  if ! json="$(k get deployments -l "app.kubernetes.io/name=rokuban,app.kubernetes.io/component=$component${E2E_FIXTURE_SCOPE:+,rokuban-e2e/fixture=true}" -o json 2>/dev/null)" || [ -z "$json" ]; then
    printf '%s%s' "$discoveryUnreadablePrefix" "Deployment（component=${component}）を引けない"
    return 0
  fi
  matches="$(printf '%s' "$json" | python3 -c '
import json, sys
site = sys.argv[1]
doc = json.load(sys.stdin)
for item in doc.get("items", []):
    for c in item["spec"]["template"]["spec"].get("containers", []):
        argv = c.get("command", []) + c.get("args", [])
        # **区切りまで見る。** 部分一致にすると、互いに接頭辞になっている
        # サイト名（home と home2）で取り違える。
        # --sites は StringSlice なので --sites=a,b も受ける
        # （cmd/rokuban/server.go）。要素完全一致だけ見ると、2 サイトを
        # 1 プロセスに束ねた watcher が「まだ実装されていない」に化ける。
        def values(a, i):
            if a == "--sites" and i + 1 < len(argv):
                return argv[i + 1].split(",")
            if a.startswith("--sites="):
                return a[len("--sites="):].split(",")
            return []
        hit = any(site in values(a, i) for i, a in enumerate(argv))
        if hit:
            print(item["metadata"]["name"])
            break
' "$site" | tr '\n' ' ')"
  matches="${matches% }"   # tr で付いた末尾の空白を落とす
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
  local json
  if ! json="$(k get scaledjobs ${E2E_FIXTURE_SCOPE:+-l rokuban-e2e/fixture=true} -o json 2>/dev/null)" || [ -z "$json" ]; then
    printf '%s' "$discoveryUnreadablePrefix"
    return 0
  fi
  printf '%s' "$json" | python3 -c '
import json, re, sys
queue = sys.argv[1].replace("-", "_")
doc = json.load(sys.stdin)
for item in doc.get("items", []):
    item["metadata"].pop("annotations", None)
    item["metadata"].pop("managedFields", None)
    blob = json.dumps(item).replace("-", "_")
    # **語境界まで見る。** 裸の部分一致だと encode が image 名
    # ghcr.io/x/encoder:1 に、epg_sitea が epg_siteaa に当たる。
    # 1 件しか一致しなければ曖昧にもならないので、**無関係な ScaledJob に
    # 対して patch と delete を撃ってその結果を判定として報告する**。
    #
    # 境界クラスにアンダースコアを入れないこと。blob はハイフンを
    # アンダースコアへ正規化した後なので、k8s の慣用名
    # rokuban-worker-epg-sitea は rokuban_worker_epg_sitea になる。
    # 外さないと**名前では一度も拾えず**、キュー名がトリガのクエリにしか
    # 無い製品の ScaledJob は、クエリを typo した瞬間に FAIL ではなく
    # TODO（まだ実装されていない）に化ける。
    if re.search(r"(?<![A-Za-z0-9])" + re.escape(queue) + r"(?![A-Za-z0-9])", blob):
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
# **「読めなかった」も空にしない。** kubectl が失敗したときに空を返すと、
# 呼び出し側は「対象がまだ無い」（TODO）と読む --- preflight を置いた理由
# （環境の破損を「まだ実装されていない」にしない）が、起動時 1 回の検査では
# 途中の API 断を拾えないので探索層でも守る。
discoveryUnreadablePrefix="UNREADABLE	"

# discovery_is_ambiguous <戻り値>
discovery_is_ambiguous() {
  case "$1" in "${discoveryAmbiguousPrefix}"*) return 0 ;; *) return 1 ;; esac
}

# discovery_is_unusable <戻り値> --- 曖昧 or 読めない（どちらも TODO ではなく FAIL）。
discovery_is_unusable() {
  case "$1" in
    "${discoveryAmbiguousPrefix}"* | "${discoveryUnreadablePrefix}"*) return 0 ;;
    *) return 1 ;;
  esac
}

# discovery_detail <戻り値> --- 使えなかった理由（一致した名前の並び、または理由）。
discovery_detail() {
  local v="${1#"${discoveryAmbiguousPrefix}"}"
  printf '%s' "${v#"${discoveryUnreadablePrefix}"}"
}

# scaledjob_for_queue <river_queue>
#
# 一意に決まれば名前、0 件なら空、複数一致なら `AMBIGUOUS<TAB><名前...>`。
# 呼び出し側は discovery_is_ambiguous で見て **TODO ではなく FAIL** にすること。
scaledjob_for_queue() {
  local queue="$1" matches count
  matches="$(scaledjobs_matching_queue "$queue" | tr '\n' ' ')"
  case "$matches" in
    "${discoveryUnreadablePrefix}"*)
      printf '%s%s' "$discoveryUnreadablePrefix" "ScaledJob を引けない（キュー ${queue}）"
      return 0 ;;
  esac
  matches="${matches% }"   # tr で付いた末尾の空白を落とす
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
  local json
  if ! json="$(k get cronjobs ${E2E_FIXTURE_SCOPE:+-l rokuban-e2e/fixture=true} -o json 2>/dev/null)" || [ -z "$json" ]; then
    printf '%s%s' "$discoveryUnreadablePrefix" "CronJob を引けない"
    return 0
  fi
  matches="$(printf '%s' "$json" | python3 -c '
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
' "$job" "$site" | tr '\n' ' ')"
  matches="${matches% }"   # tr で付いた末尾の空白を落とす
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
# kind は e2e_probe。**rokuban に登録されたワーカーが無い名前**なので、実物の
# worker が掴んだ場合は River の executor が「未登録 kind」として 1 回失敗させる
# だけで、実データには触れない（`WorkUnit == nil` の早期 return。
# 単体では cmd/rokuban の TestServerCmd_OnceModeExitsOnUnhandledJobKind が同じ形を固定）。
# `--once` の worker はそこで終了する（ログの outcome 属性が job_unhandled）。
#
# 帰結が 2 つある。**このジョブは長時間 Job にならない**ので、判定 3 の既定
# producer は実物の encode ワークロードには使えない（E2E_ENCODE_PRODUCER の
# 差し替えが要る）。もう 1 つは max_attempts=1 との組み合わせで、1 回の失敗で
# discarded になって滞留が消えることである --- 判定 5 が滞留を作れるかは
# KEDA のポーリングとの前後関係次第で、**実物の worker 込みでは未測定**。
insert_probe_job() {
  local queue="$1" count="${2:-1}"
  psql_q "INSERT INTO river_job (state, queue, kind, args, max_attempts, priority, scheduled_at)
          SELECT 'available', '$queue', 'e2e_probe', '{}'::jsonb, 1, 1, now()
          FROM generate_series(1, $count)" >/dev/null
}

# ---- 判定 3 の producer（実物の encode を 1 件走らせる）--------------------
#
# **既定の `insert_probe_job` は実物のワークロードには使えない。** `e2e_probe` は
# rokuban に登録された kind ではないので、掴んだ worker は 1 回失敗して数秒で
# 終わる --- 判定 3 は「窓の中で完走した」として TODO で抜け、`summary` は
# exit 2 を返す（受け入れの「全部緑」を満たさない）。
#
# 実物の encode を 1 件作るのに要るものは 4 つある。
#
#   1. media ボリュームの上の原本（`cluster/media-seed-job.yaml` が書く）
#   2. その原本を指す `recordings` + `media_assets` の行
#   3. 解決できる encode プロファイル（`overlays/e2e/config.yml` の `e2e-slow`）
#   4. `encode` キューへの投入 --- **`rokuban enqueue` に `encode` は無い**
#      （あるのは DB を読んで投入する `encode-reconcile`）ので直 INSERT する
#
# 判定側（生存の観測と positive control）は身代わりで検証済みなので、
# 差し替えるのはここ（実行中の Job を 1 つ作るところ）だけでよい。

# e2e_seed_media_file は原本を書く Job を回し、そのバイト数を stdout に返す。
e2e_seed_media_file() {
  # **先に消す。** Job の spec は immutable なので、前の周回のものが残っていると
  # apply が `field is immutable` で落ちる（deploy/k8s/README.md と同じ罠）。
  k delete job e2e-media-seed --ignore-not-found >/dev/null 2>&1 || true
  apply_template "$E2E_DIR/cluster/media-seed-job.yaml" || return 1
  if ! k wait --for=condition=complete job/e2e-media-seed --timeout=300s >/dev/null 2>&1; then
    log_step "media seed job did not complete: $(k logs job/e2e-media-seed 2>&1 | tail -3 | tr '\n' ' ')"
    return 1
  fi
  local size
  size="$(k logs job/e2e-media-seed 2>/dev/null | sed -n 's/^SIZE=//p' | tr -d '[:space:]')"
  # **0 を許さない。** ffmpeg が何も書かなくても Job は成功しうる形にはして
  # いないが、ここで潰すと「原本が空でもエンコードは一瞬で終わる」形になり、
  # 判定 3 が TODO に化けて原因が出力から読めなくなる。
  if [ -z "$size" ] || [ "$size" = "0" ]; then
    log_step "media seed job wrote no bytes"
    return 1
  fi
  printf '%s' "$size"
}

produce_real_encode_job() {
  # 前の周回の残骸を落とす。**media_assets を先に消す**（recordings への FK は
  # ON DELETE CASCADE ではない）。
  psql_q "DELETE FROM river_job WHERE queue = 'encode'" >/dev/null || return 1
  psql_q "DELETE FROM media_assets WHERE recording_id IN
            (SELECT id FROM recordings WHERE title = '${E2E_ENCODE_TITLE}')" >/dev/null || return 1
  psql_q "DELETE FROM recordings WHERE title = '${E2E_ENCODE_TITLE}'" >/dev/null || return 1

  local size
  size="$(e2e_seed_media_file)" || return 1

  # **`RETURNING id` の値だけを取る。** `psql -tAc` は行に続けてコマンドの
  # 状態タグ（`INSERT 0 1`）も出すので、`tr -d '[:space:]'` だけで畳むと
  # `1INSERT01` という「数値でも id でもない何か」が出来上がり、次の INSERT が
  # `trailing junk after numeric literal` で落ちる（実測）。
  local rec_id
  rec_id="$(psql_q "INSERT INTO recordings
      (source, site, network_id, service_id, event_id, service_name,
       channel_type, channel, title, program_start_at, program_duration_ms, status)
    VALUES
      ('manual', '${E2E_SITE_A}', 32736, 1, 1, 'e2e',
       'GR', '27', '${E2E_ENCODE_TITLE}', now(), 60000, 'finished')
    RETURNING id" | head -1 | tr -d '[:space:]')" || return 1
  # 数値であることまで見る（空でないだけだと、上の壊れ方をもう一度通す）。
  case "$rec_id" in
    ''|*[!0-9]*)
      log_step "could not read the probe recording id (got '${rec_id}')"
      return 1 ;;
  esac

  psql_q "INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes, state)
          VALUES (${rec_id}, 'original', '${E2E_ENCODE_REL_PATH}', ${size}, 'active')" >/dev/null || return 1

  # **`max_attempts` は 1。** 失敗したら `discarded` になって滞留が消える ---
  # 再試行で新しい Job が起き続けると、判定 3 が「どの Job を見ているのか」を
  # 見失う。
  psql_q "INSERT INTO river_job (state, queue, kind, args, max_attempts, priority, scheduled_at)
          VALUES ('available', 'encode', 'encode',
                  jsonb_build_object('recording_id', ${rec_id}::bigint, 'profile', '${E2E_ENCODE_PROFILE}'),
                  1, 1, now())" >/dev/null || return 1
  log_step "queued a real encode job (recording_id=${rec_id}, profile=${E2E_ENCODE_PROFILE}, source ${size} bytes)"
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
    # patch が失敗したら印を戻す。印だけ付いて止まっていない CronJob が残ると、
    # 判定 5 の前提（A への投入が窓に入らない）が黙って崩れる。
    if ! k patch cronjob "$name" -p '{"spec":{"suspend":true}}' >/dev/null 2>&1; then
      k annotate cronjob "$name" "${suspendedByHarnessAnnotation}-" >/dev/null 2>&1 || true
    fi
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

# restore_cronjobs は**ハーネスが止めたものだけ**を戻す。run.sh がクラスタを
# 用意した直後にも呼ぶので、前回の中断で止まったままの CronJob もそこで戻る。
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

# pause_product_workloads / resume_product_workloads
#
# **オラクル自己検査の間だけ、製品のワークロードを止める。**
#
# 身代わりは製品と同じキュー・同じ mirakc モックを共有する。探索を身代わりに
# 絞る（`E2E_FIXTURE_SCOPE`）だけでは足りない --- **絞っているのは「どの
# オブジェクトを判定対象にするか」だけ**で、River のキューも mirakc の接続数も
# 共有のままだからである。実測で 2 つ踏んだ:
#
#   - **O3.mut-rollout が FAIL**（判定 3.3 が期待の FAIL ではなく TODO）。
#     製品の `encode` の ScaledJob が、身代わりのために積んだ encode ジョブを
#     先に掴んだ。身代わりの ScaledJob からは Job が起きないので、判定は
#     「壊れている」ではなく「実行中の Job が無い」に化ける
#   - **O4.good が製品の watcher でも緑になる。** 判定 4 は mirakc モックの
#     `/events` の同時接続数を数えるが、製品の watcher が 1 本張っているので、
#     身代わりが 0 本でも「1 本」に見える（身代わりが壊れていても緑）
#
# 止めたものだけを戻すために、**クラスタ側の annotation** に印を置く
# （シェル変数だと中断時に戻らない。restore_cronjobs と同じ理由）。watcher は
# 元のレプリカ数を印の値として持つ。
pausedByHarnessAnnotation="rokuban-e2e/paused-by-harness"

# e2e_non_fixture_names <資源種別> [追加のセレクタ] --- 身代わりラベルを持たない
# オブジェクトの名前を返す。
#
# **除外は kubectl のセレクタでやる**（`!<ラベル>` = そのラベルを持たない）。
# 自前で全件を JSON に落として絞り込むと、同じことを 5 行かけて書き直すことになる。
e2e_non_fixture_names() {
  k get "$1" -l "!rokuban-e2e/fixture${2:+,$2}" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null
}

pause_product_workloads() {
  local name replicas
  for name in $(e2e_non_fixture_names scaledjobs); do
    k annotate scaledjob "$name" "${pausedByHarnessAnnotation}=true" --overwrite >/dev/null 2>&1 || continue
    scaledjob_pause "$name" true
  done
  for name in $(e2e_non_fixture_names cronjobs); do
    k annotate cronjob "$name" "${pausedByHarnessAnnotation}=true" --overwrite >/dev/null 2>&1 || continue
    k patch cronjob "$name" -p '{"spec":{"suspend":true}}' >/dev/null 2>&1 || true
  done
  # watcher だけは Deployment も落とす（上記 O4.good）。**読めなかったら
  # 触らない** --- 元のレプリカ数が分からないまま 0 にすると戻せない。
  for name in $(e2e_non_fixture_names deployments 'app.kubernetes.io/name=rokuban,app.kubernetes.io/component=watcher'); do
    replicas="$(k get deployment "$name" -o jsonpath='{.spec.replicas}' 2>/dev/null)" || continue
    [ -n "$replicas" ] || continue
    k annotate deployment "$name" "${pausedByHarnessAnnotation}=${replicas}" --overwrite >/dev/null 2>&1 || continue
    k scale deployment "$name" --replicas=0 >/dev/null 2>&1 || true
  done
}

resume_product_workloads() {
  local entry name value
  for entry in $(k get scaledjobs,cronjobs,deployments -o json 2>/dev/null | python3 -c '
import json, sys
doc = json.load(sys.stdin)
for i in doc.get("items", []):
    mark = i["metadata"].get("annotations", {}).get(sys.argv[1])
    if mark:
        print(i["kind"].lower() + "/" + i["metadata"]["name"] + "=" + mark)
' "$pausedByHarnessAnnotation"); do
    name="${entry%%=*}"
    value="${entry#*=}"
    case "$name" in
      scaledjob/*) scaledjob_pause "${name#scaledjob/}" false ;;
      cronjob/*)   k patch cronjob "${name#cronjob/}" -p '{"spec":{"suspend":false}}' >/dev/null 2>&1 || true ;;
      deployment/*) k scale deployment "${name#deployment/}" --replicas="$value" >/dev/null 2>&1 || true ;;
    esac
    k annotate "$name" "${pausedByHarnessAnnotation}-" >/dev/null 2>&1 || true
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
# `__SITE_A__` / `__SITE_B__` / `__IMAGE__` / `__FULL_IMAGE__` /
# `__ENCODE_REL_PATH__` / `__MOCK_IMAGE__` を lib/env.sh の値に
# 置き換えて apply
# する。fixture / mutant にサイト名を直書きすると、env.sh を変えたときに
# fixture 側だけが古いサイト名を指し、判定が「対象が無い」（TODO）に化ける。
apply_template() {
  local file="$1"
  shift
  sed -e "s/__SITE_A__/${E2E_SITE_A}/g" \
      -e "s/__SITE_B__/${E2E_SITE_B}/g" \
      -e "s|__IMAGE__|${E2E_IMAGE}|g" \
      -e "s|__FULL_IMAGE__|${E2E_FULL_IMAGE}|g" \
      -e "s|__ENCODE_REL_PATH__|${E2E_ENCODE_REL_PATH}|g" \
      -e "s|__MOCK_IMAGE__|${E2E_MOCK_IMAGE}|g" \
      -e "s|__BUILD_ID__|${E2E_BUILD_ID:-reused}|g" "$@" "$file" | k apply -f - >/dev/null
}

# delete_template <file> --- apply_template で当てたものを消す。
delete_template() {
  local file="$1"
  shift
  sed -e "s/__SITE_A__/${E2E_SITE_A}/g" \
      -e "s/__SITE_B__/${E2E_SITE_B}/g" \
      -e "s|__IMAGE__|${E2E_IMAGE}|g" \
      -e "s|__FULL_IMAGE__|${E2E_FULL_IMAGE}|g" \
      -e "s|__ENCODE_REL_PATH__|${E2E_ENCODE_REL_PATH}|g" \
      -e "s|__MOCK_IMAGE__|${E2E_MOCK_IMAGE}|g" \
      -e "s|__BUILD_ID__|${E2E_BUILD_ID:-reused}|g" "$@" "$file" \
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
  # **失敗を握らない。** reset が効かないと、モックが持つ前回の予約で
  # 判定 1.7 が「今回 1 件も送っていないのに緑」になる（programId は
  # 時刻に依存しないので周回をまたいで同じ）。
  # `-f` を付けて 4xx/5xx も失敗にする（curl は既定でステータスを見ない）。
  tb_curl -f -X POST "http://mirakc-$1:40772/mock/reset" >/dev/null
}
