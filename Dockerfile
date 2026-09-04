# Stage 1: フロントエンドビルド
FROM node:24.20.0 AS frontend
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
# 空の named volume をマウントすると Docker がマウント点の所有権を volume 側へ
# コピーするので、ここで nobody 所有にしておけば media_dir に書く全ループ
# （最初に走るのは RunOnStart の catalog_export）が nobody から書ける。
# 実測: CI の "Media and scratch dirs are writable as nobody" と
# "catalog_export wrote to the media volume" ステップ。
# 効かない条件（書き込み済み volume / bind mount）と復旧手順は
# docs/runbook/troubleshooting.md の「media_dir に書けない」。
RUN mkdir -p /mnt/media && chown nobody:nogroup /mnt/media
USER nobody
EXPOSE 40773
ENTRYPOINT ["rokuban"]
