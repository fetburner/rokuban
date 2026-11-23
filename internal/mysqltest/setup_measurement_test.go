package mysqltest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestMeasureDatabaseSetupStrategies は、表数を変えたときの DB 作成と DDL、および
// 共有 DB の TRUNCATE にかかる時間を比べる。通常のテストでは実行しない。
func TestMeasureDatabaseSetupStrategies(t *testing.T) {
	if os.Getenv("ROKUBAN_TEST_MYSQL_MEASURE_SETUP") != "1" {
		t.Skip("set ROKUBAN_TEST_MYSQL_MEASURE_SETUP=1 to measure MySQL test database strategies")
	}
	dsn := TestDSN(t)
	tableCount, err := strconv.Atoi(os.Getenv("ROKUBAN_MYSQL_MEASURE_TABLES"))
	if err != nil || tableCount < 1 {
		t.Fatalf("ROKUBAN_MYSQL_MEASURE_TABLES must be a positive integer, got %q", os.Getenv("ROKUBAN_MYSQL_MEASURE_TABLES"))
	}
	const rounds = 3
	ctx := context.Background()

	perTestStarted := time.Now()
	for range rounds {
		db, name, err := createMeasurementDatabase(ctx, dsn, tableCount)
		if err != nil {
			t.Fatalf("creating per-test database: %v", err)
		}
		if err := finishMeasurementDatabase(ctx, dsn, db, name); err != nil {
			t.Fatalf("cleaning up per-test database: %v", err)
		}
	}
	perTestElapsed := time.Since(perTestStarted)

	packageStarted := time.Now()
	sharedDB, sharedName, err := createMeasurementDatabase(ctx, dsn, tableCount)
	if err != nil {
		t.Fatalf("creating package database: %v", err)
	}
	var truncateErr error
	for range rounds {
		for index := range tableCount {
			if _, err := sharedDB.ExecContext(ctx, fmt.Sprintf("TRUNCATE TABLE `%s`", measurementTableName(index))); err != nil {
				truncateErr = fmt.Errorf("truncating measurement table %d: %w", index, err)
				break
			}
		}
		if truncateErr != nil {
			break
		}
	}
	cleanupErr := finishMeasurementDatabase(ctx, dsn, sharedDB, sharedName)
	if err := errors.Join(truncateErr, cleanupErr); err != nil {
		t.Fatalf("cleaning up package database after measurement error: %v", err)
	}
	packageElapsed := time.Since(packageStarted)

	t.Logf("table_count=%d rounds=%d per-test create+migrate+synthetic DDL+drop average=%s", tableCount, rounds, perTestElapsed/rounds)
	t.Logf("table_count=%d rounds=%d shared create+migrate+synthetic DDL once, then TRUNCATE all tables+drop average=%s", tableCount, rounds, packageElapsed/rounds)
}

func createMeasurementDatabase(ctx context.Context, dsn string, tableCount int) (*sql.DB, string, error) {
	db, name, err := newDatabase(ctx, dsn)
	if err != nil {
		return nil, "", err
	}
	for index := range tableCount {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"CREATE TABLE `%s` (id BIGINT NOT NULL PRIMARY KEY) ENGINE=InnoDB",
			measurementTableName(index),
		)); err != nil {
			cleanupErr := finishMeasurementDatabase(ctx, dsn, db, name)
			return nil, "", errors.Join(fmt.Errorf("creating synthetic measurement table %d: %w", index, err), cleanupErr)
		}
	}
	return db, name, nil
}

func finishMeasurementDatabase(ctx context.Context, dsn string, db *sql.DB, name string) error {
	closeErr := db.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("closing measurement pool: %w", closeErr)
	}
	dropErr := dropDatabaseContext(ctx, dsn, name)
	if dropErr != nil {
		dropErr = fmt.Errorf("dropping measurement database: %w", dropErr)
	}
	return errors.Join(closeErr, dropErr)
}

func measurementTableName(index int) string {
	return fmt.Sprintf("measurement_probe_%06d", index)
}
