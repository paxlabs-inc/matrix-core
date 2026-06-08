package store

import (
	"context"
	"os"
	"testing"
)

func testURI() string {
	if v := os.Getenv("DEUS_POSTGRES_URI"); v != "" {
		return v
	}
	return "postgres://deus:deus@127.0.0.1:5432/deus?sslmode=disable"
}

func testMigrationsDir() string {
	if v := os.Getenv("DEUS_MIGRATIONS_DIR"); v != "" {
		return v
	}
	return "../../migrations"
}

func TestMigrateCreatesSchema(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, testURI())
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer s.Close()

	if err := s.Migrate(ctx, testMigrationsDir()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Idempotent second run.
	if err := s.Migrate(ctx, testMigrationsDir()); err != nil {
		t.Fatalf("Migrate second run: %v", err)
	}

	var n int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(1) FROM information_schema.tables WHERE table_name = 'services'`).Scan(&n); err != nil {
		t.Fatalf("query services table: %v", err)
	}
	if n != 1 {
		t.Fatalf("services table missing")
	}
}
