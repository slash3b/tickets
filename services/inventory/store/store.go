// Package store is the INVENTORY service: what is AVAILABLE, as opposed to what
// exists, which is catalog's job.
//
// Not a deployed process — it is compiled into gateway (which holds and commits
// seats) and into workers (whose sweepers reclaim them). See services/README.md
// for which directories are binaries and which are not.
//
// It owns seat state and is the only writer of inventory.event_seats anywhere in
// the system.
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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ErrSeatsUnavailable means at least one requested seat was not available. The
// caller gets nothing — multi-seat requests are all-or-nothing, because handing
// someone two of the three seats they asked for is worse than handing them none.
var ErrSeatsUnavailable = errors.New("one or more seats are not available")

// ErrHoldReleased means the seats went back to the pool before the commit landed.
// If money has already moved, this is the refund path — the only case in the
// whole design where the system takes payment it cannot honour, which is why the
// hard deadline that produces it is meant to be rare enough to alert on.
var ErrHoldReleased = errors.New("hold was already released; seats are gone")

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

	// THE SPAN THAT MATTERS. Everything else in this system is plumbing around
	// this call, so it gets recorded with what actually explains its behaviour:
	// how many seats were asked for, how many attempts it took, and whether it
	// won or lost. Retries are invisible from the outside - the caller only sees
	// one slow call - and this is the only place they can be seen at all.
	ctx, span := tracer.Start(ctx, "inventory.Hold",
		trace.WithAttributes(attribute.Int("seats.requested", len(seats))))
	defer span.End()

	var lastErr error
	for attempt := range maxRetries {
		id, err := s.holdOnce(ctx, eventID, seats, ttl)
		if err == nil {
			span.SetAttributes(
				attribute.Int("hold.attempts", attempt+1),
				attribute.String("hold.outcome", "won"),
			)
			holds.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "won")))
			return id, nil
		}
		if errors.Is(err, ErrSeatsUnavailable) || !isRetryable(err) {
			// LOSING A RACE IS NOT AN ERROR. It is the expected outcome for most
			// callers on a contended seat, so the span is not marked failed and
			// nothing here is a red trace. Recording it as an error would make a
			// healthy on-sale look like an outage.
			outcome := "lost"
			if !errors.Is(err, ErrSeatsUnavailable) {
				outcome = "error"
				span.RecordError(err)
				span.SetStatus(codes.Error, "hold failed")
			}
			span.SetAttributes(
				attribute.Int("hold.attempts", attempt+1),
				attribute.String("hold.outcome", outcome),
			)
			holds.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
			return uuid.Nil, err
		}

		// A retryable failure is a real deadlock or serialization failure. Counted
		// separately because a rising rate here is the earliest signal that
		// contention is becoming a problem, well before anyone sees a 409.
		contention.Add(ctx, 1)
		lastErr = err
		// Back off with a little jitter from the attempt number so two victims do
		// not retry in lockstep and deadlock again.
		select {
		case <-ctx.Done():
			return uuid.Nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 2 * time.Millisecond):
		}
	}

	span.SetAttributes(
		attribute.Int("hold.attempts", maxRetries),
		attribute.String("hold.outcome", "exhausted"),
	)
	span.SetStatus(codes.Error, "retries exhausted")
	holds.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "exhausted")))
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

// OpenEvent makes seats available for sale.
//
// Catalog knows which seats a venue has; inventory decides what "available"
// means. Keeping the write here preserves the rule that inventory is the ONLY
// writer of seat status anywhere in the system — including the initial load,
// where it would be tempting to let catalog do it directly.
//
// Idempotent: opening an event twice does not reset seats that are already held
// or sold, which matters because "was this event already opened?" is exactly the
// question a retried admin action cannot answer.
func (s *Store) OpenEvent(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID) (int, error) {
	if len(seatIDs) == 0 {
		return 0, errors.New("no seats to open")
	}

	tag, err := s.db.Exec(ctx,
		`INSERT INTO inventory.event_seats (event_id, seat_id, status)
		 SELECT $1, unnest($2::uuid[]), 'available'
		 ON CONFLICT (event_id, seat_id) DO NOTHING`,
		eventID, seatIDs)
	if err != nil {
		return 0, fmt.Errorf("open event: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// SeatIDsForHold returns which seats a hold covers.
//
// Needed because a seat change message names SEATS, while release and commit take
// a HOLD. Read before committing: commit consumes the hold, and afterwards there
// is no way back from a hold id to the seats it held.
func (s *Store) SeatIDsForHold(ctx context.Context, holdID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.db.Query(ctx,
		`SELECT seat_id FROM inventory.hold_seats WHERE hold_id = $1`, holdID)
	if err != nil {
		return nil, fmt.Errorf("seats for hold: %w", err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SeatStatuses returns the current status of specific seats, for the read model.
func (s *Store) SeatStatuses(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT seat_id, status FROM inventory.event_seats
		  WHERE event_id = $1 AND seat_id = ANY($2)`, eventID, seatIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[uuid.UUID]string, len(seatIDs))
	for rows.Next() {
		var id uuid.UUID
		var status string
		if err := rows.Scan(&id, &status); err != nil {
			return nil, err
		}
		out[id] = status
	}
	return out, rows.Err()
}

// Convert moves a hold from `active` to `converting`, which STOPS THE SHORT TTL.
//
// Called when payment goes in flight. From here the expiry sweeper will not touch
// the hold; only the hard deadline can release it. This is the single most
// important state transition in the system — without it a slow bank expires the
// hold, the seats go back to the pool, someone else buys them, and the payment
// then succeeds against seats that are gone.
func (s *Store) Convert(ctx context.Context, holdID uuid.UUID) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE inventory.holds SET state = 'converting', updated_at = now()
		  WHERE id = $1 AND state = 'active'`, holdID)
	if err != nil {
		return fmt.Errorf("convert hold: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("hold %s is not active", holdID)
	}
	return nil
}

// Commit turns a converting hold's seats into sold. Terminal and irreversible
// for the life of the event.
//
// IDEMPOTENT ON PURPOSE. This is the last step of the saga, and it runs after
// money has already moved — so it will be retried by the resumer after a crash,
// possibly several times. It must be safe to call on an already-consumed hold,
// because "did my commit land before I died?" is a question the caller often
// cannot answer.
func (s *Store) Commit(ctx context.Context, holdID uuid.UUID) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var state string
	if err := tx.QueryRow(ctx,
		`SELECT state FROM inventory.holds WHERE id = $1 FOR UPDATE`, holdID).Scan(&state); err != nil {
		return fmt.Errorf("load hold: %w", err)
	}

	switch state {
	case "consumed":
		return nil // already done; saying so is not an error
	case "released":
		// The seats are gone. Money may have moved against them, so this is the
		// case that needs a refund rather than a retry.
		return ErrHoldReleased
	case "active", "converting":
	default:
		return fmt.Errorf("hold %s in unexpected state %q", holdID, state)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE inventory.event_seats SET status = 'sold', updated_at = now()
		  WHERE hold_id = $1 AND status = 'held'`, holdID); err != nil {
		return fmt.Errorf("sell seats: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE inventory.holds
		    SET state = 'consumed', released_reason = 'consumed', updated_at = now()
		  WHERE id = $1`, holdID); err != nil {
		return fmt.Errorf("consume hold: %w", err)
	}

	return tx.Commit(ctx)
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
