# 公式イメージ（image）と ffmpeg 入りイメージ（full）を 1 回の bake で組む。
# full は image の出力を BASE に取るので、image を push する前に full を組める。
# 両方とも amd64 runner 1 台で組めるように、重いステージは $BUILDPLATFORM で動く
# （Dockerfile / Dockerfile.full の冒頭）。
#
# タグとラベルは CI が docker/metadata-action の bake ファイルで与える
# （bake-target で下の 2 つの空 target を上書きする）。手元では --set で与える:
#
#   docker buildx bake --set '*.platform=linux/amd64' --load \
#     --set image.tags=rokuban:dev --set full.tags=rokuban-full:dev

variable "VERSION" {
  default = "dev"
}

# Actions の runner では "true" になる（bake は同名の環境変数を変数に読む）。
# gha キャッシュは runner の外では使えないので、手元の bake ではキャッシュ指定を空にする。
variable "GITHUB_ACTIONS" {
  default = ""
}

# gha_cache は scope ごとのキャッシュ指定を返す。scope 名は target 名と同じにして、
# 同じ target を組む workflow（ci / publish-sha / release）がキャッシュを共有する。
function "gha_cache" {
  params = [scope, extra]
  result = GITHUB_ACTIONS == "true" ? ["type=gha,scope=${scope}${extra}"] : []
}

target "docker-metadata-action" {}

target "docker-metadata-full" {}

target "image" {
  inherits   = ["docker-metadata-action"]
  context    = "."
  dockerfile = "Dockerfile"
  args       = { VERSION = VERSION }
  platforms  = ["linux/amd64", "linux/arm64"]
  cache-from = gha_cache("image", "")
  cache-to   = gha_cache("image", ",mode=max")
}

target "full" {
  inherits   = ["docker-metadata-full"]
  context    = "."
  dockerfile = "Dockerfile.full"
  args       = { BASE = "base" }
  contexts   = { base = "target:image" }
  platforms  = ["linux/amd64", "linux/arm64"]
  cache-from = gha_cache("full", "")
  cache-to   = gha_cache("full", ",mode=max")
}

group "default" {
  targets = ["image", "full"]
}
