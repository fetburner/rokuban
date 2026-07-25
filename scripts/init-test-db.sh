#!/bin/sh
# go test ./... 用のデータベースを作る（初回の initdb 時にのみ実行される）。
#
# テストは ROKUBAN_TEST_DATABASE_URL の DB に対してマイグレーションを張り替えるため、
# 運用データと同じ DB を使わせない。CI も同名の DB を使う（.github/workflows/ci.yml）。
set -eu

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
	CREATE DATABASE rokuban_test OWNER $POSTGRES_USER;
EOSQL
