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
# おけば空の named volume でも nobody から書き込める（実測: CI の
# "Media mount point is writable as nobody" ステップ。touch のみで
# ingest / サムネイル生成そのものは実行していない）。
# 一度でも内容が書き込まれた volume にはこのコピーアップは効かない
# （実機確認: docs/runbook/troubleshooting.md 参照）。bind mount や
# ホスト側で chown 済みの volume にも効かないが、上書きしないので無害。
RUN mkdir -p /mnt/media && chown nobody:nogroup /mnt/media
USER nobody
EXPOSE 40773
ENTRYPOINT ["rokuban"]
