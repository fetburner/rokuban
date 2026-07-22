package db

import (
	"database/sql"
	"embed"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

func MigrateUp(dbURL string) error {
	return runMigration(dbURL, func(db *sql.DB) error {
		return goose.Up(db, "migrations")
	})
}

func MigrateDown(dbURL string) error {
	return runMigration(dbURL, func(db *sql.DB) error {
		return goose.Down(db, "migrations")
	})
}

func runMigration(dbURL string, fn func(*sql.DB) error) error {
	goose.SetBaseFS(migrations)

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("setting dialect: %w", err)
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return fmt.Errorf("opening database for migration: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := fn(db); err != nil {
		return fmt.Errorf("running migration: %w", err)
	}

	return nil
}
