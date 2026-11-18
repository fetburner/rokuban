package testutil

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RequireNoError は err が nil でなければ、what を前置きして t.Fatal する。
// 失敗行はこの関数ではなく呼び出し側を指す。
func RequireNoError(t testing.TB, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// MustExec は pool で sql を実行し、失敗したら what を前置きして t.Fatal する。
// テストのフィクスチャを SQL で直接作る・書き換えるときに使う。
func MustExec(t testing.TB, pool *pgxpool.Pool, what, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}
