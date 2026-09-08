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
	"github.com/jackc/pgx/v5"
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
	uniqueViolation      = "23505"
)

// idempotencyIndex is the partial unique index in schema.sql. Named here because
// a duplicate key on THAT index means "another request already used this key" and
// is recoverable by re-reading, while a duplicate on any other index is a bug.
const idempotencyIndex = "holds_idempotency_key_idx"

// maxRetries bounds the deadlock retry loop. Deadlocks here are a normal outcome
// of contention, not a fault: one of the two transactions is chosen as victim and
// simply needs to try again.
const maxRetries = 5

type Store struct{ db *pgxpool.Pool }

func New(db *pgxpool.Pool) *Store { return &Store{db: db} }

// Hold claims seats for ttl. It returns the hold id and the moment the short TTL
// runs out, or ErrSeatsUnavailable if any seat was taken — in which case nothing
// is claimed.
//
// idempotencyKey MAY BE EMPTY, and everything below behaves exactly as it did
// before when it is. When it is not, the key is stored with the hold and a later
// call carrying the same key returns THAT hold instead of claiming again.
//
// WHAT THAT PREVENTS is not a double booking — the claim statement already makes
// that impossible — but something quieter and worse for the person at the seat
// map. Without a key, a retry after a lost response finds the seats already held
// BY THE CALLER ITSELF, and the only thing the store can say is
// ErrSeatsUnavailable. The caller is told it lost a race it actually won, its
// hold id is gone, and the seats it paid the contention cost to win sit locked
// until the sweeper expires them with nobody able to buy them.
func (s *Store) Hold(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID, ttl time.Duration, idempotencyKey string) (uuid.UUID, time.Time, error) {
	if len(seatIDs) == 0 {
		return uuid.Nil, time.Time{}, errors.New("no seats requested")
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
		// THE REPLAY CHECK, INSIDE THE LOOP RATHER THAN BEFORE IT, because it does
		// two jobs. It is the fast path for an ordinary retry, and it is also how
		// the race below resolves: when two requests carry the same key the loser's
		// insert fails on the unique index, and the next pass through here finds
		// the winner's committed row and returns it.
		if idempotencyKey != "" {
			id, expires, state, found, err := s.holdByKey(ctx, idempotencyKey)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "idempotency lookup failed")
				return uuid.Nil, time.Time{}, err
			}
			if found {
				// A KEY NAMES ONE HOLD, FOR GOOD. If that hold has already ended
				// there is nothing left to replay, and claiming fresh seats under
				// the same key would quietly turn it into a request id — the caller
				// would end up with two holds it believes are one. ErrSeatsUnavailable
				// is the honest answer and the one the caller already knows how to
				// handle: refetch the map and choose again.
				if state == "released" || state == "consumed" {
					span.SetAttributes(attribute.String("hold.outcome", "replay_expired"))
					holds.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "replay_expired")))
					return uuid.Nil, time.Time{}, ErrSeatsUnavailable
				}
				// NOT counted as "won". A replay claimed nothing, and folding it
				// into the win rate would overstate how many seats an on-sale
				// actually moved.
				span.SetAttributes(
					attribute.Int("hold.attempts", attempt+1),
					attribute.String("hold.outcome", "replayed"),
				)
				holds.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "replayed")))
				return id, expires, nil
			}
		}

		id, expires, err := s.holdOnce(ctx, eventID, seats, ttl, idempotencyKey)
		if err == nil {
			span.SetAttributes(
				attribute.Int("hold.attempts", attempt+1),
				attribute.String("hold.outcome", "won"),
			)
			holds.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "won")))
			return id, expires, nil
		}

		// LOST THE KEY, NOT THE SEATS. A concurrent request carrying the same key
		// committed first. Postgres blocks the second inserter on the index until
		// the first transaction resolves, so by the time 23505 comes back the
		// winner is committed and the replay check above will find it — loop
		// immediately, with no backoff, because there is nothing to wait for.
		if isDuplicateKey(err) {
			lastErr = err
			continue
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
			return uuid.Nil, time.Time{}, err
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
			return uuid.Nil, time.Time{}, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 2 * time.Millisecond):
		}
	}

	span.SetAttributes(
		attribute.Int("hold.attempts", maxRetries),
		attribute.String("hold.outcome", "exhausted"),
	)
	span.SetStatus(codes.Error, "retries exhausted")
	holds.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "exhausted")))
	return uuid.Nil, time.Time{}, fmt.Errorf("hold failed after %d attempts: %w", maxRetries, lastErr)
}

// holdByKey finds the hold a previous request created under this key.
//
// It reads state as well as the id because a key that names a hold which has
// since been released or consumed is NOT a replayable request — see the caller.
func (s *Store) holdByKey(ctx context.Context, key string) (uuid.UUID, time.Time, string, bool, error) {
	var (
		id      uuid.UUID
		expires time.Time
		state   string
	)
	err := s.db.QueryRow(ctx,
		`SELECT id, expires_at, state FROM inventory.holds WHERE idempotency_key = $1`,
		key).Scan(&id, &expires, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, time.Time{}, "", false, nil
	}
	if err != nil {
		return uuid.Nil, time.Time{}, "", false, fmt.Errorf("idempotency lookup: %w", err)
	}
	return id, expires, state, true, nil
}

func (s *Store) holdOnce(ctx context.Context, eventID uuid.UUID, seats []uuid.UUID, ttl time.Duration, idempotencyKey string) (uuid.UUID, time.Time, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return uuid.Nil, time.Time{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed

	holdID := uuid.New()
	now := time.Now()
	expiresAt := now.Add(ttl)

	// NULLIF, AND IT IS LOAD-BEARING. The unique index covers rows WHERE
	// idempotency_key IS NOT NULL, and an empty string is not null — storing ''
	// for every keyless hold would let exactly ONE of them exist and fail every
	// other hold in the system with a duplicate key.
	//
	// RETURNING expires_at, rather than reporting the Go value computed above.
	// timestamptz keeps MICROSECONDS and time.Time keeps nanoseconds, so the two
	// disagree in the last three digits — and a replay, which reads the column
	// back, would then report a different instant than the original call did for
	// the same hold. Taking the stored value in both paths makes them identical
	// by construction instead of nearly identical.
	if err := tx.QueryRow(ctx,
		`INSERT INTO inventory.holds (id, event_id, state, expires_at, hard_deadline, idempotency_key)
		 VALUES ($1, $2, 'active', $3, $4, NULLIF($5, ''))
		 RETURNING expires_at`,
		holdID, eventID, expiresAt, now.Add(15*time.Minute), idempotencyKey,
	).Scan(&expiresAt); err != nil {
		return uuid.Nil, time.Time{}, fmt.Errorf("insert hold: %w", err)
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
		return uuid.Nil, time.Time{}, fmt.Errorf("claim seats: %w", err)
	}

	if tag.RowsAffected() != int64(len(seats)) {
		// All-or-nothing. Rolling back also removes the hold row we just inserted.
		return uuid.Nil, time.Time{}, ErrSeatsUnavailable
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO inventory.hold_seats (hold_id, event_id, seat_id)
		 SELECT $1, $2, unnest($3::uuid[])`,
		holdID, eventID, seats,
	); err != nil {
		return uuid.Nil, time.Time{}, fmt.Errorf("record hold seats: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, time.Time{}, fmt.Errorf("commit: %w", err)
	}

	return holdID, expiresAt, nil
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

// SeatIDsForHold returns which event a hold belongs to and which seats it covers.
//
// Needed because a seat change message names an EVENT and its SEATS, while
// release and commit take a HOLD. Read before committing: commit consumes the
// hold, and afterwards there is no way back from a hold id to either.
//
// IT RETURNS THE EVENT ID AND THAT IS NOT INCIDENTAL. The first version returned
// only seats, so the sold and released messages went out with an empty event id
// — and the gateway fans out to browsers BY event id, so those two never reached
// anyone. A hold appearing on the seat map and never clearing is a worse bug than
// no live updates at all.
func (s *Store) SeatIDsForHold(ctx context.Context, holdID uuid.UUID) (uuid.UUID, []uuid.UUID, error) {
	rows, err := s.db.Query(ctx,
		`SELECT event_id, seat_id FROM inventory.hold_seats WHERE hold_id = $1`, holdID)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("seats for hold: %w", err)
	}
	defer rows.Close()

	var eventID uuid.UUID
	var out []uuid.UUID
	for rows.Next() {
		var ev, id uuid.UUID
		if err := rows.Scan(&ev, &id); err != nil {
			return uuid.Nil, nil, err
		}
		eventID = ev
		out = append(out, id)
	}
	return eventID, out, rows.Err()
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

// isDuplicateKey reports whether err is another request having already used this
// idempotency key. Scoped to the one index by name: a duplicate anywhere else in
// this table is a genuine bug and must not be swallowed as a replay.
func isDuplicateKey(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == uniqueViolation && pgErr.ConstraintName == idempotencyIndex
}

func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == deadlockDetected || pgErr.Code == serializationFailure
}
