package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// PaymentOutcome is what orders needs to know about a charge. Three values, not
// two — see payments: `unknown` is not a variant of `failed`.
type PaymentOutcome string

const (
	PaymentSucceeded PaymentOutcome = "succeeded"
	PaymentFailed    PaymentOutcome = "failed"
	PaymentUnknown   PaymentOutcome = "unknown"
)

// ErrHoldGone means the seats were released before the commit landed. If money
// has moved, this is the refund path.
var ErrHoldGone = errors.New("hold released before commit")

// Inventory and Payments are declared HERE, by the consumer, rather than exported
// from those packages. Orders states exactly what it needs; the implementations
// adapt. Today they are direct calls in one binary, tomorrow gRPC, and the saga
// does not change either way.
type Inventory interface {
	Convert(ctx context.Context, holdID uuid.UUID) error
	Commit(ctx context.Context, holdID uuid.UUID) error
	Release(ctx context.Context, holdID uuid.UUID, reason string) error
}

type Payments interface {
	// Charge must be idempotent per order — calling it twice charges once.
	Charge(ctx context.Context, orderID uuid.UUID, amountMinor int64) (PaymentOutcome, string, error)
}

// Saga drives one purchase: convert the hold, charge, commit the seats.
//
// Every step is written to the log BEFORE it is attempted, so a process that dies
// mid-saga leaves evidence of what may have happened rather than a silent gap.
type Saga struct {
	store *Store
	inv   Inventory
	pay   Payments
}

func NewSaga(s *Store, inv Inventory, pay Payments) *Saga {
	return &Saga{store: s, inv: inv, pay: pay}
}

// Run advances an order as far as it can. It is safe to call repeatedly on the
// same order — that is precisely what the resumer does after a crash.
func (sg *Saga) Run(ctx context.Context, orderID uuid.UUID) error {
	// The saga is a state machine that can run in a request OR later in the
	// resumer, so the span records where it ended up rather than how long one HTTP
	// call took. A trace showing created -> awaiting_payment -> paid -> confirmed
	// as nested spans is the clearest picture of this system there is.
	ctx, span := tracer.Start(ctx, "saga.Run")
	defer span.End()

	o, err := sg.store.Get(ctx, orderID)
	if err != nil {
		return err
	}
	if o == nil {
		return fmt.Errorf("order %s not found", orderID)
	}

	// The step the loop was last in, so a failure can say where it died. Read by
	// the deferred metric below, which runs after the loop has exited.
	lastStep := string(o.State)

	// The state it finishes in is the one thing every reader wants, and it is only
	// known once the loop stops.
	defer func() {
		span.SetAttributes(attribute.String("order.state", string(o.State)))
		if !isTerminal(o.State) {
			return
		}

		// failed_at names the step that ended it. Without it every failure is one
		// undifferentiated number and "orders are failing" cannot be narrowed to
		// "the bank is down" or "holds are expiring before checkout".
		attrs := []attribute.KeyValue{attribute.String("state", string(o.State))}
		if o.State == StateFailed {
			attrs = append(attrs, attribute.String("failed_at", lastStep))
		}
		orders.Add(ctx, 1, metric.WithAttributes(attrs...))

		if !o.CreatedAt.IsZero() {
			sagaDuration.Record(ctx, time.Since(o.CreatedAt).Seconds(),
				metric.WithAttributes(attribute.String("state", string(o.State))))
		}
	}()

	for {
		before := o.State
		lastStep = string(o.State)

		// One span per STEP, named after the state being left. This is where the
		// forward-recovery gap becomes visible: a trace that stops at "paid" with
		// no commit span is exactly the crash the resumer exists to finish.
		stepCtx, step := tracer.Start(ctx, "saga."+string(o.State))
		startedAt := time.Now()

		switch o.State {
		case StateCreated:
			err = sg.convert(stepCtx, o)
		case StateAwaitingPayment:
			err = sg.charge(stepCtx, o)
		case StatePaid:
			err = sg.commit(stepCtx, o)
		default:
			step.End()
			return nil // terminal, or waiting on something outside the saga
		}

		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		stepDuration.Record(ctx, time.Since(startedAt).Seconds(), metric.WithAttributes(
			attribute.String("step", before2step(before)),
			attribute.String("outcome", outcome),
		))

		if err != nil {
			step.RecordError(err)
			step.SetStatus(codes.Error, "saga step failed")
			step.End()
			return err
		}
		step.End()

		if o, err = sg.store.Get(ctx, orderID); err != nil {
			return err
		}
		if o.State == before {
			// No progress possible right now — an unknown payment, say. Leave it
			// for the resumer rather than spinning.
			return nil
		}
	}
}

// convert stops the hold's short TTL before any money moves. Doing this first is
// what stops a slow bank costing a customer their seats.
func (sg *Saga) convert(ctx context.Context, o *Order) error {
	if err := sg.store.LogAttempt(ctx, o.ID, "convert_hold"); err != nil {
		return err
	}
	if err := sg.inv.Convert(ctx, o.HoldID); err != nil {
		// The hold is gone or was never active. Nothing has been charged, so
		// failing here is clean.
		return sg.store.Advance(ctx, o.ID, StateFailed, "convert_hold", err.Error())
	}
	return sg.store.Advance(ctx, o.ID, StateAwaitingPayment, "convert_hold", "")
}

func (sg *Saga) charge(ctx context.Context, o *Order) error {
	if err := sg.store.LogAttempt(ctx, o.ID, "charge"); err != nil {
		return err
	}

	outcome, declineCode, err := sg.pay.Charge(ctx, o.ID, o.AmountMinor)
	if err != nil {
		return fmt.Errorf("charge: %w", err)
	}

	switch outcome {
	case PaymentSucceeded:
		return sg.store.Advance(ctx, o.ID, StatePaid, "charge", "")
	case PaymentFailed:
		// A definite decline. Release the seats — nobody was charged.
		if rErr := sg.inv.Release(ctx, o.HoldID, "payment_declined"); rErr != nil {
			return rErr
		}
		return sg.store.Advance(ctx, o.ID, StateFailed, "charge", declineCode)
	default:
		// UNKNOWN. Do NOT release the seats and do NOT fail the order. The money
		// may have moved; releasing here is how a paying customer loses their
		// seats to someone else. The hold is in `converting`, so nobody can take
		// them while payments reconciles.
		return nil
	}
}

// commit is the last step, and it runs AFTER money has moved.
//
// FORWARD RECOVERY, NOT ROLLBACK. If this fails, the correct answer is almost
// always to try again rather than to refund: the seats are still held in
// `converting` and nobody else can take them, so the purchase is still
// completable. Only a hold that has actually been released — which the hard
// deadline can do — turns this into a refund.
func (sg *Saga) commit(ctx context.Context, o *Order) error {
	if err := sg.store.LogAttempt(ctx, o.ID, "commit_seats"); err != nil {
		return err
	}

	err := sg.inv.Commit(ctx, o.HoldID)
	switch {
	case err == nil:
		return sg.store.Advance(ctx, o.ID, StateConfirmed, "commit_seats", "")
	case errors.Is(err, ErrHoldGone):
		// The one case where the system has taken money it cannot honour.
		// Meant to be rare enough to alert on.
		return sg.store.Advance(ctx, o.ID, StateReconciling, "commit_seats",
			"seats released before commit; refund required")
	default:
		// Transient. Leave it in `paid` and let the resumer try again.
		return fmt.Errorf("commit seats: %w", err)
	}
}

// Resumer finds orders stuck mid-saga and drives them forward.
//
// This is what makes a crash survivable rather than merely survivable-in-theory.
// An order left in `paid` by a process that died has money attached and no seats
// yet; nothing else in the system will notice it.
//
// MUST RUN AS A SINGLETON, like the inventory sweepers and the payment
// reconciler.
type Resumer struct {
	saga   *Saga
	store  *Store
	minAge time.Duration
	batch  int

	OnResumed func(orderID uuid.UUID, from State)
	OnError   func(error)
}

func NewResumer(saga *Saga, s *Store, minAge time.Duration) *Resumer {
	return &Resumer{saga: saga, store: s, minAge: minAge, batch: 50}
}

// Once drives one batch forward and returns how many orders it touched.
func (r *Resumer) Once(ctx context.Context) (int, error) {
	// THE RESUMER IS THE FORWARD-RECOVERY MECHANISM and it works in silence: it
	// finishes orders whose request died mid-saga. If it stops, nothing errors —
	// orders just quietly stay unfinished. A span per pass, carrying how many were
	// found and how many it moved, is the only way that becomes visible.
	ctx, span := tracer.Start(ctx, "resume")
	defer span.End()

	stuck, err := r.store.InFlight(ctx, r.minAge, r.batch)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list in-flight")
		return 0, fmt.Errorf("list in-flight: %w", err)
	}
	span.SetAttributes(attribute.Int("orders.in_flight", len(stuck)))

	n := 0
	for _, o := range stuck {
		if o.State == StateReconciling {
			// Needs a refund, which is a human or a refund worker's job, not the
			// saga's. Skip rather than spin on it.
			continue
		}
		from := o.State
		if err := r.saga.Run(ctx, o.ID); err != nil {
			r.report(fmt.Errorf("resume %s: %w", o.ID, err))
			continue
		}
		if r.OnResumed != nil {
			r.OnResumed(o.ID, from)
		}
		n++
	}
	span.SetAttributes(attribute.Int("orders.resumed", n))
	if n > 0 {
		resumed.Add(ctx, int64(n))
	}
	return n, nil
}

func (r *Resumer) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := r.Once(ctx); err != nil {
			r.report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Resumer) report(err error) {
	if r.OnError != nil {
		r.OnError(err)
	}
}

// isTerminal reports whether the saga is finished with this order. Only terminal
// states are counted, so an order is counted ONCE rather than on every pass the
// resumer makes over it.
func isTerminal(st State) bool {
	return st == StateConfirmed || st == StateFailed
}

var (
	tracer = otel.Tracer("orders")
	meter  = otel.Meter("orders")

	orders, _ = meter.Int64Counter("tickets.orders",
		metric.WithDescription("Orders reaching a terminal state"),
		metric.WithUnit("{order}"))

	// HOW LONG A PURCHASE ACTUALLY TAKES, measured from the order row being
	// created to the saga reaching a terminal state — NOT the duration of one Run
	// call. Those are different numbers whenever the resumer finishes an order the
	// original request abandoned, and the customer experienced the first one.
	//
	// Split by state, because a fast failure and a fast confirmation are both
	// "fast" and mean opposite things.
	sagaDuration, _ = meter.Float64Histogram("tickets.saga.duration",
		metric.WithDescription("Order lifetime, creation to terminal state"),
		metric.WithUnit("s"))

	// WHERE THE TIME GOES, per step. The spans already show this for one order;
	// the histogram is what answers "is charge slower this week than last" without
	// finding a representative trace by hand.
	//
	// outcome is on here because a failed step's latency is a different
	// distribution from a successful one and averaging them hides both.
	stepDuration, _ = meter.Float64Histogram("tickets.saga.step.duration",
		metric.WithDescription("Duration of one saga step"),
		metric.WithUnit("s"))

	// Orders the resumer had to finish because their request died mid-saga. A
	// non-zero rate here is not an error — it is the design working — but a
	// RISING one means requests are dying more often than they used to.
	resumed, _ = meter.Int64Counter("tickets.orders.resumed",
		metric.WithDescription("Orders completed by the resumer rather than their own request"),
		metric.WithUnit("{order}"))
)

// before2step names a step by the work it does rather than the state it starts
// from. "created" is a state; "convert_hold" is what happens there, and it is the
// same word the saga_log rows use — so a metric and a database row can be read
// side by side without a translation table in the reader's head.
func before2step(st State) string {
	switch st {
	case StateCreated:
		return "convert_hold"
	case StateAwaitingPayment:
		return "charge"
	case StatePaid:
		return "commit"
	default:
		return string(st)
	}
}
