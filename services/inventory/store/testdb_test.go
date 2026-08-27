package store

import (
	"context"
	_ "embed"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// newTestStore connects to the database named by DATABASE_URL and applies the
// schema. Tests SKIP rather than fail when it is unset, so `go test ./...` stays
// green on a machine with no database — the tests that need one say so plainly
// instead of failing for a reason unrelated to the code.
//
//	make pg-up   starts a throwaway Postgres and prints the URL
func newTestStore(t *testing.T) (*Store, uuid.UUID) {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — run `make pg-up` first")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	t.Cleanup(pool.Close)

	// Every test gets its own event id, so tests never collide and nothing has to
	// be torn down between them.
	return New(pool), uuid.New()
}

// seedSeats creates n available seats for an event and returns their ids.
func seedSeats(t *testing.T, s *Store, eventID uuid.UUID, n int) []uuid.UUID {
	t.Helper()

	ctx := context.Background()
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.New()
		if _, err := s.db.Exec(ctx,
			`INSERT INTO inventory.event_seats (event_id, seat_id, status)
			 VALUES ($1, $2, 'available')`, eventID, ids[i]); err != nil {
			t.Fatalf("seed seat %d: %v", i, err)
		}
	}
	return ids
}
