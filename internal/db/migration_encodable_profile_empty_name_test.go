package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestEncodableProfileEmptyNameMigration は、空の profile_name に対する encodable_profile の
// 戻り値を、マイグレーション 00078 の前後で固定する。known_profiles に NULL を渡すと NULL を
// 返す、という COMMENT の契約に空名も従うこと。00078 が無いと（空名との比較が先頭にあると）
// FALSE になるので、マイグレーションが適用されたかをこの値で見分ける。
func TestEncodableProfileEmptyNameMigration(t *testing.T) {
	ctx := context.Background()
	url := testDatabaseURL(t)
	if err := MigrateUp(ctx, url); err != nil {
		t.Fatal(err)
	}
	migrateDownTo(t, url, 77)
	t.Cleanup(func() {
		if err := MigrateUp(ctx, url); err != nil {
			t.Error(err)
		}
	})
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	const query = `SELECT encodable_profile(1::bigint, '', NULL::text[], '{}'::text[])`
	var before *bool
	if err := pool.QueryRow(ctx, query).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == nil || *before {
		t.Fatalf("before 00078: encodable_profile('', NULL) = %v, want false", before)
	}

	if err := MigrateUp(ctx, url); err != nil {
		t.Fatal(err)
	}
	var after *bool
	if err := pool.QueryRow(ctx, query).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != nil {
		t.Fatalf("after 00078: encodable_profile('', NULL) = %v, want NULL", *after)
	}
}
