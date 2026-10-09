package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/JoshBlazer/waybill/api/internal/db"
	"github.com/JoshBlazer/waybill/api/internal/store"
)

// TestMigrate_AgainstRealPostgres applies the embedded migrations to a fresh
// PostgreSQL in a container, then runs a sqlc-generated query.
func TestMigrate_AgainstRealPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: needs Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ctr, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("waybill"),
		postgres.WithUsername("waybill"),
		postgres.WithPassword("waybill"),
		postgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Running again must be a no-op.
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	v, err := db.Version(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Fatalf("schema version = %d, want 1", v)
	}

	ok, err := store.New(pool).Ping(ctx)
	if err != nil || ok != 1 {
		t.Fatalf("Ping = %d, %v", ok, err)
	}
}
