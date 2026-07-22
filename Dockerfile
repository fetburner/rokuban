# Stage 1: フロントエンドビルド
FROM node:22 AS frontend
RUN corepack enable && corepack install -g pnpm@9
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
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -ldflags "-s -w -X github.com/fetburner/rokuban/internal/api.version=${VERSION}" -o /rokuban ./cmd/rokuban

# Stage 3: Debian slim — curl (healthcheck) と ca-certificates を含む
FROM debian:bookworm-slim
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl
COPY --from=backend /rokuban /usr/local/bin/rokuban
USER nobody
EXPOSE 40773
ENTRYPOINT ["rokuban"]
