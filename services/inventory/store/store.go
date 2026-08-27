// Package store owns seat state. It is the only writer of inventory.event_seats
// anywhere in the system.
//
// The whole project turns on one guarantee: N buyers, M seats, N >> M, and a seat
// is never sold twice. Everything else is scaffolding around that sentence.
package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSeatsUnavailable means at least one requested seat was not available. The
// caller gets nothing — multi-seat requests are all-or-nothing, because handing
// someone two of the three seats they asked for is worse than handing them none.
var ErrSeatsUnavailable = errors.New("one or more seats are not available")

// serializationFailure is SQLSTATE 40001; deadlockDetected is 40P01. Postgres
// raises the latter when two concurrent statements lock the same rows in a
// different order — which is exactly what two overlapping multi-seat requests do.
const (
	serializationFailure = "40001"
	deadlockDetected     = "40P01"
)

// maxRetries bounds the deadlock retry loop. Deadlocks here are a normal outcome
// of contention, not a fault: one of the two transactions is chosen as victim and
// simply needs to try again.
const maxRetries = 5

type Store struct{ db *pgxpool.Pool }

func New(db *pgxpool.Pool) *Store { return &Store{db: db} }

// Hold claims seats for ttl. It returns the hold id, or ErrSeatsUnavailable if
// any seat was taken — in which case nothing is claimed.
func (s *Store) Hold(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID, ttl time.Duration) (uuid.UUID, error) {
	if len(seatIDs) == 0 {
		return uuid.Nil, errors.New("no seats requested")
	}

	// SORT BEFORE LOCKING. Postgres takes row locks in whatever order it scans,
	// so two concurrent requests over overlapping seat sets can deadlock. Sorting
	// makes every caller acquire in the same order, which removes most of them;
	// the retry below covers what is left.
	seats := slices.Clone(seatIDs)
	// uuid.UUID is [16]byte, so it is not cmp.Ordered — compare the bytes.
	slices.SortFunc(seats, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	seats = slices.Compact(seats)

	var lastErr error
	for attempt := range maxRetries {
		id, err := s.holdOnce(ctx, eventID, seats, ttl)
		if err == nil {
			return id, nil
		}
		if errors.Is(err, ErrSeatsUnavailable) || !isRetryable(err) {
			return uuid.Nil, err
		}

		lastErr = err
		// Back off with a little jitter from the attempt number so two victims do
		// not retry in lockstep and deadlock again.
		select {
		case <-ctx.Done():
			return uuid.Nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 2 * time.Millisecond):
		}
	}

	return uuid.Nil, fmt.Errorf("hold failed after %d attempts: %w", maxRetries, lastErr)
}

func (s *Store) holdOnce(ctx context.Context, eventID uuid.UUID, seats []uuid.UUID, ttl time.Duration) (uuid.UUID, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed

	holdID := uuid.New()
	now := time.Now()

	if _, err := tx.Exec(ctx,
		`INSERT INTO inventory.holds (id, event_id, state, expires_at, hard_deadline)
		 VALUES ($1, $2, 'active', $3, $4)`,
		holdID, eventID, now.Add(ttl), now.Add(15*time.Minute),
	); err != nil {
		return uuid.Nil, fmt.Errorf("insert hold: %w", err)
	}

	// THE CLAIM. One statement, and the reason the whole design works.
	//
	// The `status = 'available'` predicate and the write happen atomically, so
	// there is no window between checking and taking. It needs no SELECT FOR
	// UPDATE and no SERIALIZABLE, which means seats that are NOT contended still
	// proceed fully in parallel — only genuine contention on the same seat
	// serializes. Losing is signalled by the row count, not by an error.
	tag, err := tx.Exec(ctx,
		`UPDATE inventory.event_seats
		    SET status = 'held', hold_id = $1, updated_at = now()
		  WHERE event_id = $2 AND seat_id = ANY($3) AND status = 'available'`,
		holdID, eventID, seats,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("claim seats: %w", err)
	}

	if tag.RowsAffected() != int64(len(seats)) {
		// All-or-nothing. Rolling back also removes the hold row we just inserted.
		return uuid.Nil, ErrSeatsUnavailable
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO inventory.hold_seats (hold_id, event_id, seat_id)
		 SELECT $1, $2, unnest($3::uuid[])`,
		holdID, eventID, seats,
	); err != nil {
		return uuid.Nil, fmt.Errorf("record hold seats: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}

	return holdID, nil
}

// Release returns a hold's seats to the pool. Safe to call twice.
func (s *Store) Release(ctx context.Context, holdID uuid.UUID, reason string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE inventory.event_seats
		    SET status = 'available', hold_id = NULL, updated_at = now()
		  WHERE hold_id = $1 AND status = 'held'`, holdID); err != nil {
		return fmt.Errorf("release seats: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE inventory.holds SET state = 'released', updated_at = now()
		  WHERE id = $1 AND state IN ('active','converting')`, holdID); err != nil {
		return fmt.Errorf("release hold: %w", err)
	}

	return tx.Commit(ctx)
}

// CountByStatus is used by tests and by the invariant checker.
func (s *Store) CountByStatus(ctx context.Context, eventID uuid.UUID, status string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx,
		`SELECT count(*) FROM inventory.event_seats WHERE event_id = $1 AND status = $2`,
		eventID, status).Scan(&n)
	return n, err
}

// CheckInvariants asserts the two rules from DESIGN.md that must never be false.
// It returns one error per violation found, joined. An empty return is the only
// acceptable result, and this is meant to run continuously in production, not
// just in tests — if this system ever oversells, this is how you find out.
func (s *Store) CheckInvariants(ctx context.Context, eventID uuid.UUID) error {
	var problems []error

	// 1. A held seat names exactly one hold, and that hold is still live.
	var orphaned int
	if err := s.db.QueryRow(ctx, `
		SELECT count(*) FROM inventory.event_seats s
		 WHERE s.event_id = $1 AND s.status = 'held'
		   AND NOT EXISTS (
		       SELECT 1 FROM inventory.holds h
		        WHERE h.id = s.hold_id AND h.state IN ('active','converting'))`,
		eventID).Scan(&orphaned); err != nil {
		return fmt.Errorf("invariant query: %w", err)
	}
	if orphaned > 0 {
		problems = append(problems, fmt.Errorf("%d seats held by a hold that is no longer live", orphaned))
	}

	// 2. No seat is claimed by more than one live hold. If this ever returns a
	//    row, the system has oversold and everything else stops.
	var doubled int
	if err := s.db.QueryRow(ctx, `
		SELECT count(*) FROM (
		    SELECT event_id, seat_id FROM inventory.hold_seats hs
		     WHERE hs.event_id = $1
		       AND EXISTS (SELECT 1 FROM inventory.holds h
		                    WHERE h.id = hs.hold_id AND h.state IN ('active','converting','consumed'))
		     GROUP BY event_id, seat_id HAVING count(*) > 1) dup`,
		eventID).Scan(&doubled); err != nil {
		return fmt.Errorf("invariant query: %w", err)
	}
	if doubled > 0 {
		problems = append(problems, fmt.Errorf("OVERSELL: %d seats claimed by more than one live hold", doubled))
	}

	return errors.Join(problems...)
}

func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == deadlockDetected || pgErr.Code == serializationFailure
}
