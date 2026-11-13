#!/usr/bin/env bash
# internal/db/schema.sql を再生成する。使い捨ての PostgreSQL コンテナに internal/db/migrations/ を
# 全て適用し、pg_dump の出力を正規化して書く。CI の generated-diff が、コミット済みとの差分で落とす。
#
# 置き場を internal/db/schema.sql にする理由: migrations/ の中に置くと
# //go:embed migrations/*.sql と goose の読み込みの対象になる。sqlc.yaml の schema は
# migrations/ のままにする（ダンプを sqlc の入力にすると権威が 2 つになる）。
#
# 出力から落とすもの:
# - goose_db_version: goose の管理表でアプリのスキーマではない。適用済みの一覧は migrations/ が権威。
# - SET 行・set_config・pg_dump の版番号ヘッダ: 版ごとに変わるだけで定義を表さない。
# - \restrict / \unrestrict: pg_dump 17.6 以降はダンプごとにランダムな鍵を出すので、残すと毎回差分になる
#   （17.12 で出ることを確認済み）。
#
# 版はマイナー版まで固定する（PG_IMAGE の digest も一緒に）。pg_dump の出力はマイナー版でも変わり
# （17.6 以降の \restrict 等）、Docker は手元にキャッシュしたタグを pull し直さないので、タグだけだと
# 手元と CI で版がずれる。サーバーも pg_dump も PG_IMAGE のコンテナ 1 つで動かす。上げるときは digest も
# 一緒に変える。要るのは Docker と Go だけ。
set -euo pipefail
cd "$(dirname "$0")/.."

PG_IMAGE=postgres:17.12@sha256:dbb9623d5121b5d2c2c9e2fba56e40193cbb3510d4af7f035b1ab1823aff3205
OUT=internal/db/schema.sql

cid=$(docker run -d --rm -e POSTGRES_HOST_AUTH_METHOD=trust -p 127.0.0.1::5432 "$PG_IMAGE")
trap 'docker rm -f "$cid" >/dev/null' EXIT
# TCP で待つ。初期化中の一時サーバーは unix socket しか開かないので、socket で待つと再起動前に通ってしまう。
# 起動に失敗するとコンテナは --rm で消えて docker exec が失敗し続けるので、回数で打ち切る。
ready=0
for _ in $(seq 60); do
  if docker exec "$cid" pg_isready -q -h 127.0.0.1 -U postgres; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "PostgreSQL コンテナが 60 秒以内に起動しなかった（image: $PG_IMAGE）" >&2
  exit 1
fi
port=$(docker port "$cid" 5432/tcp | head -n1 | sed 's/.*://')

go run ./internal/db/schemadump "postgres://postgres@127.0.0.1:${port}/postgres?sslmode=disable"
docker exec "$cid" pg_dump -U postgres --schema-only --no-owner --no-privileges \
  --exclude-table=goose_db_version postgres \
  | grep -v -E '^(SET |SELECT pg_catalog\.set_config|-- Dumped (from|by) |\\(un)?restrict )' \
  | cat -s > "$OUT"
echo "wrote $OUT ($(wc -l < "$OUT" | tr -d " ") lines)"
