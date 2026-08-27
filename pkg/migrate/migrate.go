// Package migrate applies the service schemas at startup.
//
// HONEST ABOUT WHAT THIS IS: it applies idempotent CREATE ... IF NOT EXISTS
// statements. It is not a migration tool — there are no versions, no ordering
// guarantees between changes, and no way to alter an existing table. That is
// adequate while the schema only ever grows and every environment is disposable,
// and it stops being adequate the moment a column needs changing under live data.
// Replace it with a real migration tool before that day, not after.
package migrate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// lockID is an arbitrary constant. Any two processes using the same number
// serialise against each other, which is the point.
const lockID = 0x7C1E75

// Apply runs each schema under a Postgres advisory lock.
//
// Concurrent CREATE TABLE IF NOT EXISTS from several replicas can deadlock or
// fail with a duplicate-object error, and a gateway that crash-loops on startup
// because a sibling was doing the same thing is a miserable way to learn that.
// The lock is released automatically when the session ends, including if the
// process dies holding it.
func Apply(ctx context.Context, pool *pgxpool.Pool, schemas ...string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockID)
	}()

	for i, s := range schemas {
		if _, err := conn.Exec(ctx, s); err != nil {
			return fmt.Errorf("schema %d: %w", i, err)
		}
	}
	return nil
}
