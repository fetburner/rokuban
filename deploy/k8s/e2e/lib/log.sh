# shellcheck shell=bash
#
# 判定結果の記録と出力。
#
# **3 値である**（PASS / FAIL / TODO）ことがこのハーネスの要点。2 値にすると
# 「まだ実装されていない」と「壊れている」が同じ赤になり、残りのワークロードを
# 足していく途中で
# 何が退行したのか分からなくなる。
#
#   PASS  判定が走り、期待どおりだった
#   FAIL  判定が走り、期待と違った（= 壊れている）
#   TODO  判定の**対象が無い**ので走らせていない（= まだ実装されていない）
#
# TODO は「判定を諦めた」ではない。対象が現れれば同じスクリプトがそのまま
# 判定に入る。したがって TODO のメッセージには**何が見つからなかったか**を
# 書く（「未実装」だけ書かない）。

E2E_RESULTS="${E2E_RESULTS:?E2E_RESULTS must be set}"

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  _c_pass=$'\033[32m'; _c_fail=$'\033[31m'; _c_todo=$'\033[33m'
  _c_dim=$'\033[2m'; _c_bold=$'\033[1m'; _c_off=$'\033[0m'
else
  _c_pass=''; _c_fail=''; _c_todo=''; _c_dim=''; _c_bold=''; _c_off=''
fi

# log_step は判定の進行そのものを出す（結果ではない）。
log_step() {
  printf '%s   ..%s %s\n' "$_c_dim" "$_c_off" "$*"
}

log_section() {
  printf '\n%s== %s%s\n' "$_c_bold" "$*" "$_c_off"
}

# record <PASS|FAIL|TODO> <id> <message>
# `status` という名前は使わない --- zsh では読み取り専用なので、このファイルを
# zsh から source すると「read-only variable: status」で死ぬ。ハーネス自体は
# 常に bash で走るが、手元で関数だけ試すときに踏む。
record() {
  local verdict="$1" id="$2"
  shift 2
  local message="$*"
  printf '%s\t%s\t%s\n' "$verdict" "$id" "$message" >>"$E2E_RESULTS"
  case "$verdict" in
    PASS) printf '%s[ PASS ]%s %s %s\n' "$_c_pass" "$_c_off" "$id" "$message" ;;
    FAIL) printf '%s[ FAIL ]%s %s %s\n' "$_c_fail" "$_c_off" "$id" "$message" ;;
    TODO) printf '%s[ TODO ]%s %s %s\n' "$_c_todo" "$_c_off" "$id" "$message" ;;
    *) printf '[ ???? ] %s %s\n' "$id" "$message" ;;
  esac
}

pass() { record PASS "$@"; }
fail() { record FAIL "$@"; }
todo() { record TODO "$@"; }

# plan <id>... --- この判定が記録するはずの assert id を先に宣言する。
#
# **宣言が無いと「判定が黙って死んだ」を検出できない。** 判定スクリプトが
# 1 行も記録せずに落ちても（unbound variable / source 失敗 / kubectl のハング）、
# 結果ファイルからは「何も言うことが無かった」と区別が付かない。いまは TODO が
# 多いので必ず 2 が返るが、**全部 PASS になった後は「判定 5 が起動直後に落ちた」
# が exit 0 になる** --- それは「このコマンドが 0 を返すこと」という完了条件
# そのものを空虚にする。
plan() {
  local id
  for id in "$@"; do
    printf 'PLAN\t%s\t\n' "$id" >>"$E2E_RESULTS"
  done
}

# assert_eq <id> <want> <got> <message> --- 一致なら PASS、違えば FAIL。
# **期待値はリテラルで渡す**（実装の定数を読んで比べると何も主張しない）。
assert_eq() {
  local id="$1" want="$2" got="$3"
  shift 3
  if [ "$want" = "$got" ]; then
    pass "$id" "$* (= $got)"
  else
    fail "$id" "$* --- got '$got', want '$want'"
  fi
}

# summary は結果を集計して表を出し、終了コードを決める。
#
#   0  全部 PASS
#   1  1 つでも FAIL（= 壊れている）
#   2  FAIL は無いが TODO がある（= まだ全部は実装されていない）
#
# **2 を 0 にしない。** ワークロードが揃ったと言えるのは 5 項目が全部緑になったときなので、TODO が
# 残っている限りこのハーネスは成功を返してはならない。**1 と 2 を同じにも
# しない**（上記の 3 値の理由）。
summary() {
  # 宣言されたのに記録が無い id を先に FAIL として書き足す（plan のコメント）。
  local id
  while IFS= read -r id; do
    [ -n "$id" ] || continue
    if ! awk -F'\t' -v id="$id" '$1 != "PLAN" && $2 == id {found=1} END {exit !found}' "$E2E_RESULTS"; then
      fail "$id" "この判定は結果を記録しなかった（判定スクリプトが途中で落ちた可能性）"
    fi
  done < <(awk -F'\t' '$1 == "PLAN" {print $2}' "$E2E_RESULTS")

  local pass_n fail_n todo_n
  pass_n=$(grep -c '^PASS' "$E2E_RESULTS" || true)
  fail_n=$(grep -c '^FAIL' "$E2E_RESULTS" || true)
  todo_n=$(grep -c '^TODO' "$E2E_RESULTS" || true)

  log_section "結果"
  if [ "$fail_n" -gt 0 ]; then
    printf '%s壊れている（FAIL）%s\n' "$_c_fail" "$_c_off"
    grep '^FAIL' "$E2E_RESULTS" | while IFS=$'\t' read -r _ id msg; do
      printf '  %s %s\n' "$id" "$msg"
    done
  fi
  if [ "$todo_n" -gt 0 ]; then
    printf '%sまだ実装されていない（TODO）%s\n' "$_c_todo" "$_c_off"
    grep '^TODO' "$E2E_RESULTS" | while IFS=$'\t' read -r _ id msg; do
      printf '  %s %s\n' "$id" "$msg"
    done
  fi
  printf '\nPASS %d / FAIL %d / TODO %d\n' "$pass_n" "$fail_n" "$todo_n"

  if [ "$fail_n" -gt 0 ]; then
    printf '%s=> 壊れている判定がある（exit 1）%s\n' "$_c_fail" "$_c_off"
    return 1
  fi
  if [ "$todo_n" -gt 0 ]; then
    printf '%s=> 壊れてはいないが、まだ全部は実装されていない（exit 2）%s\n' "$_c_todo" "$_c_off"
    return 2
  fi
  # **「全部緑」を名乗るのは全部走らせたときだけ。** 一部だけ回して 0 を
  # 返すと、その 0 が完了条件（`run.sh` が 0 を返すこと）と区別が付かない。
  if [ -n "${E2E_PARTIAL_RUN:-}" ]; then
    printf '%s=> 走らせたものはすべて緑。ただし一部しか走らせていない（%s。exit 2）%s\n' \
      "$_c_todo" "$E2E_PARTIAL_RUN" "$_c_off"
    return 2
  fi
  # 主語はモードで変える。オラクル自己検査の結果に「5 項目すべて緑」と
  # 出すと、受け入れの完了条件（run.sh が 0 を返すこと）と読み違える。
  printf '%s=> %sすべて緑（exit 0）%s\n' "$_c_pass" "${E2E_SUMMARY_SUBJECT:-5 項目}" "$_c_off"
  return 0
}
