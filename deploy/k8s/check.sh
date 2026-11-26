#!/usr/bin/env bash
# deploy/k8s の検査。CI の manifests ジョブが呼ぶのと同じものを、手元でも呼べる。
#
#   deploy/k8s/check.sh                    全部回す（下の 4 つ）
#   deploy/k8s/check.sh --list-overlays    検査対象の overlay を並べて終わる（deploy/k8s の Go テストが使う）
#
# 検査するのは次の 4 つ。
#   1. kustomize build: base・site・store と、overlays/*/（名前の列挙をせず、ディレクトリを走査する）
#   2. kubeconform: 1 の出力と、e2e/cluster・e2e/fixtures の YAML をスキーマに掛ける
#   3. e2e/lib/selftest.sh: クラスタを要らない判定ハーネスの純粋な部分
#   4. shellcheck: e2e の判定ハーネスの .sh（設定は e2e/.shellcheckrc）
#
# **回さないもの。** 受け入れ判定ハーネス（e2e の run.sh・checks・faults）は CI では回さない。回すのは人間で、
# タイミングと理由は docs/runbook/k8s.md §受け入れ判定ハーネス。ここで見るのは、ハーネスが持つマニフェスト
# （overlays/e2e・足場・CRD を含まない fixture）が組めてスキーマに合うことまで。回さないものが腐るのを、
# せめてここで止める。ConfigMap の中身（overlay の config.yml）は kustomize にも kubeconform にも不透明なので、
# go test ./deploy/k8s/（TestOverlayConfigsLoad）が見る。
#
# 必要なツール: kustomize・kubeconform・shellcheck。版は .tool-versions の値と揃える（CI は同じ値を入れる）。
# 冒頭で版を表示し、.tool-versions と違えば警告する（失敗はさせない。手元の版が違うのは許すため）。
#
# **build と検証はパイプで繋がず、中間ファイルを経由する。** run: の既定シェルは pipefail を付けないので、
# パイプの終了コードは kubeconform のものになる。kubeconform は標準入力が空でも exit 0 を返すので、
# kustomize build が何も出さずに失敗しても緑になってしまう（実測で確かめた）。
#
# **スキーマの版を固定する。** 既定は master なので、リポジトリ側の変更ゼロで上流の k8s のリリースだけで
# 検査内容が動く。-kubernetes-version の値は docs/runbook/k8s.md で実機確認した版のマイナー（スキーマは
# マイナー単位で公開されている）。上げるときは両方を同じ PR で動かす。
#
# **CRD（KEDA の ScaledJob）はリポジトリに置いたスキーマで検査する。** -ignore-missing-schemas は採らない。
# 未知の kind を黙って飛ばすので、`kind: ScaledJobb` のような typo も一緒に飛ばすことになる。スキーマの出どころと、
# -strict を有効にするための加工（additionalProperties: false）は deploy/k8s/schemas/README.md。
# ファイル名が小文字なのは {{.ResourceKind}} が小文字化した名前を埋めるため。大文字にすると macOS では通って
# Linux の CI だけが落ちる。
#
# **overlay は glob で拾う。** 名前で列挙すると、overlay を 1 つ足した日にそれだけが検査から漏れる。
#
# bash 3.2 でも動くように書く（mapfile と連想配列を使わない）。
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd "$script_dir/../.." && pwd)"
# K8S_DIR は、検査する deploy/k8s の場所を差し替える（テストが偽の overlay を並べるために使う）。
k8s_dir="${K8S_DIR:-$script_dir}"

# list_overlays は overlays/ の直下のディレクトリを、deploy/k8s からの相対パスで並べる。
list_overlays() {
  local d
  for d in "$k8s_dir"/overlays/*/; do
    if [ -d "$d" ]; then
      d="${d%/}"
      printf 'overlays/%s\n' "${d##*/}"
    fi
  done
}

# pinned は .tool-versions の tool の版を返す。
pinned() {
  awk -v tool="$1" '$1 == tool { print $2; exit }' "$repo_dir/.tool-versions"
}

# show_version は入っているツールの版を表示し、.tool-versions と違えば警告する。
show_version() {
  local tool="$1" want installed
  want="$(pinned "$tool")"
  installed="$2"
  echo "$tool: $installed (.tool-versions: ${want:-未定義})"
  if [ -z "$want" ] || [ "${installed#*"$want"}" = "$installed" ]; then
    echo "warning: $tool の版が .tool-versions（${want:-未定義}）と違う。CI と結果が違うかもしれない" >&2
  fi
}

if [ "${1:-}" = "--list-overlays" ]; then
  list_overlays
  exit 0
fi
if [ "$#" -ne 0 ]; then
  echo "usage: $0 [--list-overlays]" >&2
  exit 2
fi

show_version kustomize "$(kustomize version 2>/dev/null | head -n 1)"
show_version kubeconform "$(kubeconform -v 2>/dev/null | head -n 1)"
show_version shellcheck "$(shellcheck --version | awk '/^version:/ { print $2 }')"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

overlays=()
while IFS= read -r ov; do
  overlays+=("$ov")
done < <(list_overlays)
if [ "${#overlays[@]}" -eq 0 ]; then
  echo "error: $k8s_dir/overlays に overlay が 1 つも無い" >&2
  exit 1
fi

# 1. kustomize build。site/ だけを組むのは、overlay 経由でしか組まないと、site/ が壊れているのか
#    overlay の patch が壊れているのかを出力から切り分けられないため。
manifests=()
for name in base site store; do
  kustomize build "$k8s_dir/$name" > "$work/$name.yaml"
  manifests+=("$work/$name.yaml")
done
for ov in "${overlays[@]}"; do
  out="$work/${ov//\//-}.yaml"
  kustomize build "$k8s_dir/$ov" > "$out"
  manifests+=("$out")
done

# 2. kubeconform。e2e の cluster と fixtures は、足場も fixture も glob で拾う（名前で列挙すると、
#    ハーネスにマニフェストを 1 つ足した日にそれだけが検査から漏れる）。
kubeconform -strict -summary -kubernetes-version 1.36.0 \
  -schema-location default \
  -schema-location "$k8s_dir/schemas/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json" \
  "${manifests[@]}" \
  "$k8s_dir"/e2e/cluster/*.yaml \
  "$k8s_dir"/e2e/fixtures/*.yaml

# 3. ハーネスの純粋な部分（対象の探索と watch ログの読み取り）。1 巡目のレビュー修正がここにテストを持たず、
#    「複数一致は FAIL」の経路が一度も動いていなかったので、ここで回す。
"$k8s_dir/e2e/lib/selftest.sh"

# 4. shellcheck。除外の番号と理由は e2e/.shellcheckrc に書く（ここでフラグを渡さない）。
(cd "$k8s_dir/e2e" && shellcheck run.sh oracles.sh faults/*.sh lib/*.sh checks/*.sh)

echo "deploy/k8s checks passed"
