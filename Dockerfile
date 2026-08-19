# Stage 1: フロントエンドビルド
FROM node:22 AS frontend
RUN corepack enable
WORKDIR /build/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile
COPY web/ ./
COPY openapi.yaml /build/
RUN pnpm build

# Stage 2: Go バイナリビルド
FROM golang:1.26 AS backend
WORKDIR /build
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
COPY --from=frontend /build/web/dist/ web/dist/
ARG VERSION=dev
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOARCH=${TARGETARCH} go build -ldflags "-s -w -X github.com/fetburner/rokuban/internal/api.version=${VERSION}" -o /rokuban ./cmd/rokuban

# Stage 3: Debian slim — curl (healthcheck) と ca-certificates を含む
FROM debian:bookworm-slim
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl
COPY --from=backend /rokuban /usr/local/bin/rokuban
# storage.media_dir の既定値（config.example.yml）に合わせたマウント点。
# Docker は「空の named volume をマウントするとき、マウント点の内容
# （所有権含む）をボリュームにコピーする」ため、ここで nobody 所有にして
# おけば空の named volume でも nobody から書き込める。
# これが無いと media_dir に書く全ループが落ちる。**フレッシュな構成で最初に
# 落ちるのは catalog_export**（RunOnStart の定期ジョブなので、録画が 1 件も
# 無い起動直後に走る）。ingest / サムネイル生成 / エンコードも同じ理由で
# 落ちるが、そちらは録画ができるまで走らない。
# 実測: CI の "catalog_export wrote to the media volume" ステップ（実バイナリが
# nobody として os.MkdirAll + os.Create する経路）と、その前段の
# "Media and scratch dirs are writable as nobody" probe。
# 一度でも内容が書き込まれた volume にはこのコピーアップは効かない
# （復旧手順は docs/runbook/troubleshooting.md の
# 「media_dir に書けない」。chown は -R が必要）。bind mount や
# ホスト側で chown 済みの volume にも効かないが、上書きしないので無害。
RUN mkdir -p /mnt/media && chown nobody:nogroup /mnt/media
USER nobody
EXPOSE 40773
ENTRYPOINT ["rokuban"]
