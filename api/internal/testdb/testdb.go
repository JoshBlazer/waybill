// Package testdb gives integration tests a real, migrated PostgreSQL.
//
// One container starts per test binary. Migrations run once into a template
// database; each call to New clones it with CREATE DATABASE … TEMPLATE, so
// every test gets an isolated database that it can commit to. Committing
// matters: the ledger's balance checks are deferred to commit time, so tests
// that roll back would never exercise them.
//
// Import only from _test.go files.
package testdb

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/JoshBlazer/waybill/api/internal/db"
)

const templateDB = "waybill_template"

var (
	once    sync.Once
	baseDSN string // connection to the maintenance database "postgres"
	initErr error
	counter atomic.Int64
)

func start() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase(templateDB),
		postgres.WithUsername("waybill"),
		postgres.WithPassword("waybill"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		initErr = fmt.Errorf("start postgres: %w", err)
		return
	}
	// The container is removed by the testcontainers reaper when the test
	// binary exits.
	tmplDSN, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		initErr = err
		return
	}
	pool, err := db.Open(ctx, tmplDSN)
	if err != nil {
		initErr = err
		return
	}
	err = db.Migrate(ctx, pool)
	pool.Close() // a template must have no open connections
	if err != nil {
		initErr = err
		return
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		initErr = err
		return
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		initErr = err
		return
	}
	baseDSN = fmt.Sprintf("postgres://waybill:waybill@%s:%s/%%s?sslmode=disable", host, port.Port())
}

// New returns a pool connected to a fresh, fully migrated database. It skips
// the test under -short.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: needs Docker")
	}
	once.Do(start)
	if initErr != nil {
		t.Fatalf("testdb: %v", initErr)
	}
	ctx := context.Background()
	name := fmt.Sprintf("t_%d_%d", time.Now().UnixNano(), counter.Add(1))

	admin, err := db.Open(ctx, fmt.Sprintf(baseDSN, "postgres"))
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateDB)); err != nil {
		t.Fatalf("testdb: create database: %v", err)
	}

	pool, err := db.Open(ctx, fmt.Sprintf(baseDSN, name))
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
