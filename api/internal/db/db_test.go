package db_test

import (
	"context"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/JoshBlazer/waybill/api/db/migrations"
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
	if want := latestMigration(t); v != want {
		t.Fatalf("schema version = %d, want %d (the highest embedded migration)", v, want)
	}

	ok, err := store.New(pool).Ping(ctx)
	if err != nil || ok != 1 {
		t.Fatalf("Ping = %d, %v", ok, err)
	}
}

// latestMigration returns the version number of the highest migration file
// embedded in the binary, for example 2 for 00002_ledger.sql.
func latestMigration(t *testing.T) int64 {
	t.Helper()
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no embedded migrations: %v", err)
	}
	var latest int64
	for _, f := range files {
		n, err := strconv.ParseInt(strings.SplitN(f, "_", 2)[0], 10, 64)
		if err != nil {
			t.Fatalf("migration %q has no numeric prefix", f)
		}
		latest = max(latest, n)
	}
	return latest
}
