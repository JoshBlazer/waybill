// Package db opens PostgreSQL connections and applies migrations.
package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/JoshBlazer/waybill/api/db/migrations"
)

// Open creates a connection pool and verifies it can reach the database.
func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		// pgx errors can echo the connection string; do not wrap them.
		return nil, fmt.Errorf("parse database config: invalid DATABASE_URL")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}

// Migrate applies every pending migration embedded in the binary.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	return migrate(ctx, sqlDB)
}

// Version reports the current goose schema version.
func Version(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	p, err := newProvider(sqlDB)
	if err != nil {
		return 0, err
	}
	return p.GetDBVersion(ctx)
}

func migrate(ctx context.Context, sqlDB *sql.DB) error {
	p, err := newProvider(sqlDB)
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

func newProvider(sqlDB *sql.DB) (*goose.Provider, error) {
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		return nil, fmt.Errorf("load migrations: %w", err)
	}
	return p, nil
}
