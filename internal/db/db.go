// Package db opens the PostgreSQL connection and applies the schema.
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"os"

	_ "github.com/lib/pq"
)

//go:embed schema.sql
var schemaSQL string

// DSN returns the connection string from $DATABASE_URL, defaulting to a local db.
func DSN() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://recon:recon@localhost:5432/recon?sslmode=disable"
}

// Open connects and pings.
func Open(ctx context.Context) (*sql.DB, error) {
	d, err := sql.Open("postgres", DSN())
	if err != nil {
		return nil, err
	}
	if err := d.PingContext(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// Migrate applies schema.sql (idempotent).
func Migrate(ctx context.Context, d *sql.DB) error {
	_, err := d.ExecContext(ctx, schemaSQL)
	return err
}
