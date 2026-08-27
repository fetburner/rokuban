# shellcheck shell=bash
#
# **判定そのものを検査する。**「判定が存在すること」と「判定が効くこと」は
# 違う。ここでは判定 1〜5 のそれぞれについて
#
#   1. 正しく動く身代わり（fixtures/）を立てて、判定が緑になることを見る
#   2. **それを 1 か所だけ壊して**（mutants/）、判定が赤になることを見る
#
# の 2 本立てで確かめる。**2 が無ければ 1 は何も保証しない** --- 対象を
# 見つけられていないだけで緑になる判定（TODO 扱いのつもりが PASS）や、
# 窓を取るだけの negative assertion は、壊しても緑のままになりうる。
#
# 身代わりは製品のワークロードではない。**製品のマニフェストをここに
# 書かない**（書くと「自分で書いた正解」と突き合わせるだけになる）。
# 詳細は README.md §オラクルの自己検査。

# run_check_into <n> <results_file> --- 判定 n を専用の結果ファイルで回す。
run_check_into() {
  local n="$1" results="$2" script
  script="$(ls "$E2E_DIR_SELF"/checks/0"$n"-*.sh)"
  E2E_RESULTS="$results" bash "$script"
}

# expect_status <oracle_id> <results_file> <assert_id> <PASS|FAIL> <説明>
#
# 判定の結果を読み、期待どおりの status だったかを**このハーネス自身の結果**
# として記録する。
expect_status() {
  local oracle_id="$1" results="$2" assert_id="$3" want="$4"
  shift 4
  local got
  # PLAN 行（判定が「これを記録する」と宣言した行）は結果ではないので飛ばす。
  got="$(awk -F'\t' -v id="$assert_id" '$1 != "PLAN" && $2 == id {print $1; exit}' "$results")"
  if [ "$got" = "$want" ]; then
    pass "$oracle_id" "$* (判定 $assert_id は $want)"
  else
    fail "$oracle_id" "$* --- 判定 $assert_id は '$got'（期待 '$want'）。$(grep -c . "$results") 件の結果: $(tr '\n' ';' <"$results")"
  fi
}

# reset_fixture_state --- 判定と判定の間で、前の回の残りを消す。
#
# **消さないと次の回が前の回の Job を見て緑になる**（判定 2 の 0 → 1 → 0 は
# 「開始時点で 0」を前提にしている）。
reset_fixture_state() {
  k delete jobs -l 'rokuban-e2e/fixture=true' --ignore-not-found >/dev/null 2>&1 || true
  drain_queue "epg_${E2E_SITE_A}"
  drain_queue "epg_${E2E_SITE_B}"
  drain_queue encode
}

# build_mutant_image --- 変異を当てたリポジトリの複製からイメージを焼く。
#
# **`git stash` を使わない。** ワークツリーは他の作業と共有されうるし、
# 隔離 worktree と併用すると stash が互いに干渉する。rsync した複製に
# 当てれば、元のツリーは一度も変異しない。
build_mutant_image() {
  local mutation="$1" tag="$2" copy
  copy="$(mktemp -d)"
  log_step "copying the tree for mutant ${tag}"
  rsync -a --exclude .git --exclude node_modules --exclude web/node_modules \
    --exclude web/dist "$E2E_ROOT/" "$copy/"
  if ! python3 "$E2E_DIR/mutants/$mutation" "$copy"; then
    rm -rf "$copy"
    return 1
  fi
  log_step "building mutant image ${tag}"
  if ! docker build -t "$tag" "$copy" >/dev/null; then
    rm -rf "$copy"
    return 1
  fi
  # load の失敗を見ないと、存在しないタグへ `k set image` して
  # 「rollout できない」で赤くなり、原因が出力から読めなくなる。
  if ! kind load docker-image "$tag" --name "$E2E_CLUSTER" >/dev/null; then
    rm -rf "$copy"
    return 1
  fi
  rm -rf "$copy"
}

# ---- 判定 1 ---------------------------------------------------------------
#
# **いま唯一 PASS を出している判定なので、ここにこそ変異が要る。** 身代わりは
# 不要（対象は製品の api そのもの）。

oracle_check1() {
  log_section "オラクル検査 1: api が上がって Service 経由で届く"
  # 判定側と同じく、記録するはずの id を先に宣言する（黙って死んだら
  # summary が FAIL を書き足す。lib/log.sh）。
  plan "O1.good" "O1.control" "O1.mut-selector"

  local r
  r="$(mktemp)"
  run_check_into 1 "$r"
  expect_status "O1.good" "$r" "1.1" PASS "api Deployment が Ready と判定される"
  expect_status "O1.control" "$r" "1.5" PASS "Service 経由で api に届くと判定される"

  # 変異: Service のセレクタを外す。Pod は生きているが Endpoints が空になるので、
  # 「Deployment は Ready」と「Service 経由で届く」が分かれていることも同時に見る。
  log_step "mutant: breaking the Service selector"
  k patch service rokuban-api --type=merge \
    -p '{"spec":{"selector":{"app.kubernetes.io/name":"rokuban","app.kubernetes.io/component":"nonexistent"}}}' >/dev/null
  r="$(mktemp)"
  run_check_into 1 "$r"
  expect_status "O1.mut-selector" "$r" "1.5" FAIL "Service が Pod を指さなくなると赤くなる"
  k patch service rokuban-api --type=merge \
    -p '{"spec":{"selector":{"app.kubernetes.io/name":"rokuban","app.kubernetes.io/component":"api"}}}' >/dev/null
}

# ---- 判定 2 ---------------------------------------------------------------

oracle_check2() {
  log_section "オラクル検査 2: worker 0 でも CronJob が投入し、KEDA が消化する"
  plan "O2.good" "O2.mut-trigger" "O2.mut-cron"
  apply_template "$E2E_DIR/fixtures/worker-scaledjobs.yaml"
  apply_template "$E2E_DIR/fixtures/cron-epg-sync.yaml"
  reset_fixture_state

  local r
  r="$(mktemp)"
  run_check_into 2 "$r"
  expect_status "O2.good" "$r" "2.4" PASS "身代わりの ScaledJob + CronJob で 0 → 1 → 0 が観測できる"

  # 変異 1: **トリガが滞留を見なくなる**（クエリが常に 0 を返す）。
  # 実際に踏みうる形は「キュー名の typo」「state の綴り違い」で、症状は
  # 同じ「いつまでも Job が起きない」。
  log_step "mutant: trigger query always returns 0"
  k patch scaledjob "e2e-fx-worker-epg-${E2E_SITE_A}" --type=json \
    -p '[{"op":"replace","path":"/spec/triggers/0/metadata/query","value":"SELECT 0"}]' >/dev/null
  reset_fixture_state
  r="$(mktemp)"
  run_check_into 2 "$r"
  expect_status "O2.mut-trigger" "$r" "2.4" FAIL "トリガが滞留を見なくなると 0 → 1 → 0 が赤くなる"
  apply_template "$E2E_DIR/fixtures/worker-scaledjobs.yaml"

  # 変異 2: **投入側が止まる**（CronJob を suspend）。KEDA 側は正しいまま。
  log_step "mutant: CronJob suspended"
  k patch cronjob "e2e-fx-cron-epg-sync-${E2E_SITE_A}" \
    -p '{"spec":{"suspend":true}}' >/dev/null
  reset_fixture_state
  r="$(mktemp)"
  run_check_into 2 "$r"
  expect_status "O2.mut-cron" "$r" "2.1" FAIL "投入側が止まると赤くなる"
  k patch cronjob "e2e-fx-cron-epg-sync-${E2E_SITE_A}" \
    -p '{"spec":{"suspend":false}}' >/dev/null
}

# ---- 判定 3 ---------------------------------------------------------------

oracle_check3() {
  log_section "オラクル検査 3: 実行中の encode Job がスケールインで殺されない"
  plan "O3.good" "O3.control" "O3.mut-rollout" "O3.mut-omitted"
  apply_template "$E2E_DIR/fixtures/encode-scaledjob.yaml"
  reset_fixture_state

  local r
  r="$(mktemp)"
  run_check_into 3 "$r"
  expect_status "O3.good" "$r" "3.3" PASS "rollout.strategy=gradual なら更新しても生き残る"
  expect_status "O3.control" "$r" "3.4" PASS "positive control（意図的に消した Pod を検出できる）"

  # 変異: `rollout.strategy` を `immediate` にする（値ごとの挙動は
  # docs/operations.md §5「worker: KEDA ScaledJob」）。
  log_step "mutant: rollout.strategy=immediate"
  k patch scaledjob e2e-fx-worker-encode --type=json \
    -p '[{"op":"replace","path":"/spec/rollout/strategy","value":"immediate"}]' >/dev/null
  reset_fixture_state
  k delete jobs -l 'rokuban-e2e/fixture=true' --ignore-not-found >/dev/null 2>&1 || true
  r="$(mktemp)"
  run_check_into 3 "$r"
  expect_status "O3.mut-rollout" "$r" "3.3" FAIL "rollout.strategy=immediate なら更新で殺されて赤くなる"
  apply_template "$E2E_DIR/fixtures/encode-scaledjob.yaml"

  # 変異: **`rollout` ごと省略する**（実際に踏むのは「書き忘れ」の方）。
  # 省略時の挙動は上流の既定なので、書かずに済ませずここで測る --- 測らない
  # まま「明示しないと消える」と書くと、一度も真でなかった記述になる。
  # 結果は docs/operations.md §5 の表に記録してある。
  log_step "mutant: rollout omitted entirely"
  k patch scaledjob e2e-fx-worker-encode --type=json \
    -p '[{"op":"remove","path":"/spec/rollout"}]' >/dev/null
  reset_fixture_state
  k delete jobs -l 'rokuban-e2e/fixture=true' --ignore-not-found >/dev/null 2>&1 || true
  r="$(mktemp)"
  run_check_into 3 "$r"
  log_step "rollout omitted -> 判定 3.3 は $(awk -F'\t' '$1 != "PLAN" && $2 == "3.3" {print $1; exit}' "$r")"
  expect_status "O3.mut-omitted" "$r" "3.3" FAIL "rollout を書き忘れると更新で殺されて赤くなる"
  apply_template "$E2E_DIR/fixtures/encode-scaledjob.yaml"
}

# ---- 判定 4 ---------------------------------------------------------------

oracle_check4() {
  log_section "オラクル検査 4: watcher を 2 レプリカにしても二重に動かない"
  plan "O4.good" "O4.mut-lock"
  apply_template "$E2E_DIR/fixtures/watcher.yaml"
  k rollout status "deployment/e2e-fx-watcher-${E2E_SITE_A}" --timeout=180s || true

  local r
  r="$(mktemp)"
  run_check_into 4 "$r"
  expect_status "O4.good" "$r" "4.2" PASS "本物の watcher 2 レプリカで SSE が 1 本"

  # 変異: `pg_try_advisory_lock` の戻り値を無視したイメージ。
  local tag="rokuban:e2e-mutant-lock"
  if ! build_mutant_image ignore-advisory-lock.py "$tag"; then
    fail "O4.mut-lock" "変異イメージをビルドできない --- 判定 4 の有効性を確かめていない"
    return
  fi
  k set image "deployment/e2e-fx-watcher-${E2E_SITE_A}" "watcher=$tag" >/dev/null
  k rollout status "deployment/e2e-fx-watcher-${E2E_SITE_A}" --timeout=180s || true
  r="$(mktemp)"
  run_check_into 4 "$r"
  expect_status "O4.mut-lock" "$r" "4.2" FAIL "ロックを無視したイメージでは SSE が 2 本になって赤くなる"

  k set image "deployment/e2e-fx-watcher-${E2E_SITE_A}" "watcher=$E2E_IMAGE" >/dev/null
  k rollout status "deployment/e2e-fx-watcher-${E2E_SITE_A}" --timeout=180s || true
}

# ---- 判定 5 ---------------------------------------------------------------

oracle_check5() {
  log_section "オラクル検査 5: サイト B の滞留でサイト A の Job が起きない"
  plan "O5.good" "O5.control" "O5.mut-queue"
  apply_template "$E2E_DIR/fixtures/worker-scaledjobs.yaml"
  reset_fixture_state

  local r
  r="$(mktemp)"
  run_check_into 5 "$r"
  expect_status "O5.good" "$r" "5.2" PASS "site 修飾されたトリガなら B の滞留で A は起きない"
  expect_status "O5.control" "$r" "5.3" PASS "positive control（A 自身の滞留では起きる）"

  # 変異: **キュー名の site 修飾を外す**（`qualifyQueueName` を外したのと
  # 同じ形をトリガ側で再現する）。A のスケーラが B の滞留を見るようになる。
  log_step "mutant: site A's trigger matches every epg_* queue"
  k patch scaledjob "e2e-fx-worker-epg-${E2E_SITE_A}" --type=json \
    -p '[{"op":"replace","path":"/spec/triggers/0/metadata/query","value":"SELECT count(*) FROM river_job WHERE queue LIKE '"'"'epg%'"'"' AND state IN ('"'"'available'"'"','"'"'retryable'"'"')"}]' >/dev/null
  reset_fixture_state
  r="$(mktemp)"
  run_check_into 5 "$r"
  expect_status "O5.mut-queue" "$r" "5.2" FAIL "site 修飾を外すと B の滞留で A が起きて赤くなる"
  apply_template "$E2E_DIR/fixtures/worker-scaledjobs.yaml"
}

# ---- 後片付け -------------------------------------------------------------

oracle_teardown() {
  log_step "removing fixtures"
  delete_template "$E2E_DIR/fixtures/worker-scaledjobs.yaml"
  delete_template "$E2E_DIR/fixtures/cron-epg-sync.yaml"
  delete_template "$E2E_DIR/fixtures/encode-scaledjob.yaml"
  delete_template "$E2E_DIR/fixtures/watcher.yaml"
  k delete jobs -l 'rokuban-e2e/fixture=true' --ignore-not-found >/dev/null 2>&1 || true
}

run_oracles() {
  if ! command -v rsync >/dev/null 2>&1; then
    printf 'rsync is required for the mutant image build\n' >&2
    return 70
  fi
  # **前の回の身代わりを先に消す。** ScaledJob は「作られた時点で env を解決
  # できたか」を状態として持ち、失敗すると controller-runtime の指数バック
  # オフで再試行する。前の回に（Secret がまだ無い等で）失敗した ScaledJob が
  # 残っていると、同じ YAML を apply しても spec が変わらないので再 reconcile
  # されず、**直っているのに Ready=False のまま**になる（KEDA v2.20.2 で実測:
  # 接続文字列を直した後も 3 分間 `ScaledJobCheckFailed` のままだった）。
  oracle_teardown
  export E2E_SUMMARY_SUBJECT="オラクル検査が"
  # **オラクル側も環境の破損を判定結果にしない。** ここを通さないと、KEDA が
  # 入り損ねただけで O2 / O3 / O5 が「変異を当てたら赤くなった」ではなく
  # ただの赤として出る（身代わりの残骸だけは、ここでは立てるのが仕事なので見ない）。
  preflight_environment || return 1
  local only="${E2E_ORACLES_ONLY:-1,2,3,4,5}"
  # **一部だけ回したら 0 を返さない**（--only と同じ理由。lib/log.sh）。
  if [ "$only" != "1,2,3,4,5" ]; then
    export E2E_PARTIAL_RUN="E2E_ORACLES_ONLY=$only"
  fi
  case ",$only," in *,1,*) oracle_check1 ;; esac
  case ",$only," in *,2,*) oracle_check2 ;; esac
  case ",$only," in *,3,*) oracle_check3 ;; esac
  case ",$only," in *,4,*) oracle_check4 ;; esac
  case ",$only," in *,5,*) oracle_check5 ;; esac
  oracle_teardown
  summary
}
