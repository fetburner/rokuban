#!/usr/bin/env bash
# internal/db/schema.sql を再生成する。空の DB に internal/db/migrations/ を全て適用し、
# pg_dump の出力を正規化して書く。CI の generated-diff が、コミット済みとの差分で落とす。
#
# 置き場を internal/db/schema.sql にする理由: migrations/ の中に置くと
# //go:embed migrations/*.sql と goose の読み込みの対象になる。sqlc.yaml の schema は
# migrations/ のままにする（ダンプを sqlc の入力にすると権威が 2 つになる）。
#
# 出力から落とすもの:
# - goose_db_version: goose の管理表でアプリのスキーマではない。適用済みの一覧は migrations/ が権威。
# - SET 行・set_config・pg_dump の版番号ヘッダ: 版ごとに変わるだけで定義を表さない。
# - \restrict / \unrestrict: pg_dump 17.6 以降はダンプごとにランダムな鍵を出すので、残すと毎回差分になる。
#
# 版の pin は PostgreSQL 17。pg_dump と psql は PG_IMAGE（postgres:17）をコンテナで実行し、
# 管理対象サーバーも major 17 でなければ止める。CI の service と同じ版にそろえるため。
# 手元の Postgres.app（16）は pg_dump の版が合わず使えない。手元で走らせるには
# PG 17 のコンテナを Docker の VM の localhost に公開し、ROKUBAN_SCHEMADUMP_ADMIN_DSN で指す。
set -euo pipefail
cd "$(dirname "$0")/.."

ADMIN_DSN=${ROKUBAN_SCHEMADUMP_ADMIN_DSN:-postgres://localhost:5432/postgres?sslmode=disable}
PG_IMAGE=postgres:17
OUT=internal/db/schema.sql

pgtool() { docker run --rm --network host "$PG_IMAGE" "$@"; }

major=$(pgtool psql "$ADMIN_DSN" -tAc 'SHOW server_version_num')
case $major in
  17*) ;;
  *) echo "::error::サーバーは PostgreSQL 17 が要る（server_version_num=$major）。ROKUBAN_SCHEMADUMP_ADMIN_DSN を 17 のサーバーに向ける" >&2; exit 1 ;;
esac

name="rokuban_schemadump_$$"
dsn=$(printf '%s' "$ADMIN_DSN" | sed -E "s#(://[^/]*)/[^?]*#\1/${name}#")
pgtool psql "$ADMIN_DSN" -v ON_ERROR_STOP=1 -qc "CREATE DATABASE \"$name\""
trap 'pgtool psql "$ADMIN_DSN" -qc "DROP DATABASE IF EXISTS \"$name\" WITH (FORCE)"' EXIT

go run ./internal/db/schemadump "$dsn"
pgtool pg_dump --schema-only --no-owner --no-privileges --exclude-table=goose_db_version "$dsn" \
  | grep -v -E '^(SET |SELECT pg_catalog\.set_config|-- Dumped (from|by) |\\(un)?restrict )' \
  | cat -s > "$OUT"
echo "wrote $OUT ($(wc -l < "$OUT") lines)"
