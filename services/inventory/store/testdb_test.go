package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/slash3b/tickets/pkg/pgtest"
)

// newTestStore applies the schema to THIS PACKAGE'S OWN database and returns a
// store for it. See pkg/pgtest for why the database is per-package rather than
// the one DATABASE_URL names directly.
func newTestStore(t *testing.T) (*Store, uuid.UUID) {
	t.Helper()

	dsn := pgtest.DSN(t, "inventory")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := pool.Exec(ctx, SchemaSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	// A PER-TEST EVENT ID IS NOT ENOUGH, because the sweeper is GLOBAL. SweepExpired
	// reclaims every expired hold in the table and returns how many seats it
	// touched — that is what it is for — so an expired hold left by another test,
	// or by a previous RUN of this package against the same database, is counted
	// here and the assertion reads `swept 6 seats, want 2`.
	//
	// It fails intermittently, which is the worst version: green on a fresh
	// database, red on the second run, and the difference is invisible in the
	// diff. Same shape as the payments reconciler, and the same honest fix.
	for _, q := range []string{
		`TRUNCATE inventory.holds CASCADE`,
		`TRUNCATE inventory.event_seats`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("truncate: %v", err)
		}
	}
	t.Cleanup(pool.Close)

	// Each test still gets its own event id, so seats seeded by one are invisible
	// to the per-event assertions of another.
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
