package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// SweepExpired releases holds whose short TTL has run out, returning how many.
//
// IT DELIBERATELY IGNORES `converting` HOLDS. That single WHERE clause is the
// most important line in this file. A hold enters `converting` when payment goes
// in flight, and from that moment the short TTL stops applying — otherwise a slow
// bank would let this sweeper return the seats, someone else would buy them, and
// only then would the payment succeed. Taking money for a seat you cannot deliver
// is the worst outcome the system can produce, and it would be entirely
// self-inflicted.
//
// MUST RUN AS A SINGLETON. See Sweeper.
func (s *Store) SweepExpired(ctx context.Context) (int, error) {
	return s.sweep(ctx, "active", "expires_at", "expired")
}

// SweepHardDeadline releases holds stuck in `converting` past their hard deadline
// and returns their ids.
//
// A hold cannot be immortal: if orders dies permanently mid-checkout, those seats
// must eventually come back. But crossing this deadline is NOT routine — a
// payment may have succeeded against seats that no longer belong to it, so every
// id returned here needs reconciliation and probably a refund. This should be
// rare enough to alert on; if it is not, something upstream is broken.
//
// MUST RUN AS A SINGLETON. See Sweeper.
func (s *Store) SweepHardDeadline(ctx context.Context) ([]uuid.UUID, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`UPDATE inventory.holds
		    SET state = 'released', released_reason = 'hard_deadline', updated_at = now()
		  WHERE state = 'converting' AND hard_deadline < now()
		  RETURNING id`)
	if err != nil {
		return nil, fmt.Errorf("sweep hard deadline: %w", err)
	}

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(ids) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE inventory.event_seats
			    SET status = 'available', hold_id = NULL, updated_at = now()
			  WHERE hold_id = ANY($1) AND status = 'held'`, ids); err != nil {
			return nil, fmt.Errorf("release hard-deadline seats: %w", err)
		}
	}

	return ids, tx.Commit(ctx)
}

func (s *Store) sweep(ctx context.Context, fromState, deadlineCol, reason string) (int, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Seats first, then holds. Doing it the other way round would leave a window
	// where a seat still points at a hold that is already 'released', which the
	// invariant checker would correctly report as a violation.
	tag, err := tx.Exec(ctx, fmt.Sprintf(
		`UPDATE inventory.event_seats
		    SET status = 'available', hold_id = NULL, updated_at = now()
		  WHERE status = 'held' AND hold_id IN (
		      SELECT id FROM inventory.holds
		       WHERE state = '%s' AND %s < now())`, fromState, deadlineCol))
	if err != nil {
		return 0, fmt.Errorf("release swept seats: %w", err)
	}

	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`UPDATE inventory.holds
		    SET state = 'released', released_reason = '%s', updated_at = now()
		  WHERE state = '%s' AND %s < now()`, reason, fromState, deadlineCol)); err != nil {
		return 0, fmt.Errorf("mark holds released: %w", err)
	}

	return int(tag.RowsAffected()), tx.Commit(ctx)
}

// Sweeper runs the two sweeps and the invariant check on a ticker.
//
// RUN EXACTLY ONE OF THESE PER CLUSTER, as its own single-replica Deployment —
// not one per API replica. Nothing here is unsafe concurrently, but N replicas do
// N times the work on the same rows and manufacture the lock contention the rest
// of the design works to avoid.
type Sweeper struct {
	store    *Store
	interval time.Duration
	// OnHardDeadline is called with holds that crossed the hard deadline. These
	// need reconciliation - money may have moved. nil is allowed while orders
	// does not exist yet.
	OnHardDeadline func(ids []uuid.UUID)
	// OnError is called for sweep failures. A sweep failing is not fatal - the
	// next tick retries - but silence would hide a sweeper that has been dead for
	// a week, which presents as seats mysteriously never coming back.
	OnError func(err error)
}

func NewSweeper(s *Store, interval time.Duration) *Sweeper {
	return &Sweeper{store: s, interval: interval}
}

// Run blocks until ctx is cancelled.
func (w *Sweeper) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()

	for {
		w.once(ctx)

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Sweeper) once(ctx context.Context) {
	// A SPAN PER SWEEP. Background work is the least observable thing in any
	// system — nobody is waiting on it, so nothing complains when it stops — and
	// until this existed the workers process emitted no spans at all and did not
	// appear in the service list, despite running the three loops that keep the
	// data consistent.
	//
	// It records how much it actually reclaimed, which is the number that matters:
	// a sweeper releasing zero holds forever is either idle or broken, and those
	// look identical from outside.
	ctx, span := tracer.Start(ctx, "sweep")
	defer span.End()

	released, err := w.store.SweepExpired(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "sweep expired")
		w.report(fmt.Errorf("sweep expired: %w", err))
	}
	span.SetAttributes(attribute.Int("holds.expired", released))
	if released > 0 {
		swept.Add(ctx, int64(released), metric.WithAttributes(attribute.String("reason", "expired")))
	}

	ids, err := w.store.SweepHardDeadline(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "sweep hard deadline")
		w.report(fmt.Errorf("sweep hard deadline: %w", err))
	} else if len(ids) > 0 {
		span.SetAttributes(attribute.Int("holds.hard_deadline", len(ids)))
		swept.Add(ctx, int64(len(ids)), metric.WithAttributes(attribute.String("reason", "hard_deadline")))
		if w.OnHardDeadline != nil {
			w.OnHardDeadline(ids)
		}
	}
}

func (w *Sweeper) report(err error) {
	if w.OnError != nil {
		w.OnError(err)
	}
}
