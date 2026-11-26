#!/usr/bin/env bash
# ローカルでビルドしたイメージの起動検査。CI の docker ジョブの bake の後の検査と同じものを回す。
#
#   docker buildx bake -f docker-bake.hcl image full --load \
#     --set image.tags=rokuban:test --set full.tags=rokuban:test-full
#   scripts/image-smoke.sh
#
# 検査は次の順。イメージ名は rokuban:test（migrate 用）と rokuban:test-full（server --all 用。ffmpeg 入り）。
#   1. JLSE のバイナリが共有ライブラリを解決できる（full）
#   2. /mnt/media と /var/tmp/rokuban が実行ユーザーから書ける（full）
#   3. compose を上げ、/healthz と SPA の配信を見る
#   4. catalog_export が completed になり、media volume に manifest を書く
# 終了時に compose を片付ける。失敗したときは compose のログを出す。
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

compose=(docker compose -f docker-compose.test.yml)
probe_volume=rokuban-media-probe

cleanup() {
  local status=$?
  if [ "$status" -ne 0 ]; then
    "${compose[@]}" logs || true
  fi
  "${compose[@]}" down -v || true
  docker volume rm -f "$probe_volume" >/dev/null 2>&1 || true
  exit "$status"
}
trap cleanup EXIT

# 1. JLSE のバイナリは実行時のライブラリを持つ。ldd が「not found」を出すなら、そのイメージは壊れている。
docker run --rm --entrypoint sh rokuban:test-full -eu -c '
  for tool in chapter_exe logoframe join_logo_scp; do
    test -x "/usr/local/bin/$tool"
    if ldd "/usr/local/bin/$tool" 2>&1 | grep -F "not found"; then exit 1; fi
  done
'

# 2. /mnt/media（storage.media_dir 既定値）と /var/tmp/rokuban（storage.scratch_dir 既定値）が実行ユーザーから書けること。
#    修正前のイメージでは media 側が Permission denied で落ちる。uid も主張するのは、Dockerfile.full 末尾の
#    `USER nobody` が将来落ちたときにこの検査が名前だけ嘘のまま通らないため（root なら書けてしまう）。
#    scratch_dir 側はベースイメージの /var/tmp が 1777 であることに依存しているだけなので、イメージを差し替えたとき
#    に黙って壊れないよう、ここで一緒に押さえる。
docker volume create "$probe_volume" >/dev/null
docker run --rm --entrypoint sh -v "$probe_volume":/mnt/media rokuban:test-full \
  -c 'test "$(id -u)" = 65534 && mkdir -p /mnt/media/probe/nested && touch /mnt/media/probe/nested/file && mkdir -p /var/tmp/rokuban'

# 3. 起動と配信。SPA の検査はスクリプトで、fallback を含む。
"${compose[@]}" up --wait --wait-timeout 60
curl -sf http://localhost:40773/healthz
node web/scripts/check-spa-served.mjs http://localhost:40773

# 4. catalog_export は RunOnStart の定期ジョブなので、起動直後に実バイナリが nobody として media volume へ
#    世代ディレクトリを書く。上の probe は touch/mkdir であって Go の os.MkdirAll / os.Create を通っていない。
#    healthz はジョブが落ちても通るため、ジョブの状態を明示的に見る。修正前のイメージではここが
#    state=retryable + permission denied になる。
state=""
for _ in $(seq 1 30); do
  state=$("${compose[@]}" exec -T postgres \
    psql -U rokuban -d rokuban -tAc \
    "SELECT state FROM river_job WHERE kind='catalog_export' ORDER BY id DESC LIMIT 1")
  case "$state" in
    completed) break ;;
  esac
  sleep 2
done
"${compose[@]}" exec -T postgres \
  psql -U rokuban -d rokuban -c \
  "SELECT id, kind, state, attempt, errors FROM river_job WHERE kind='catalog_export'"
test "$state" = completed
# 展開はコンテナの中で行わせるので、外側のシェルでは展開しない。
# shellcheck disable=SC2016
"${compose[@]}" exec -T rokuban \
  sh -c 'test -f "$(ls -d /mnt/media/catalog/catalog-*/ | tail -1)/manifest.json"'

echo "image smoke checks passed"
