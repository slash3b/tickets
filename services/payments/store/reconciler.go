package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/slash3b/tickets/services/payments/bankclient"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// Charger is the slice of the bank client the reconciler needs. Narrow on
// purpose: it makes the reconciler trivially testable and states exactly what it
// depends on.
type Charger interface {
	Authorize(ctx context.Context, key string, amountMinor int64) (*bankclient.Charge, error)
	Lookup(ctx context.Context, key string) (*bankclient.Charge, bool, error)
}

// Reconciler resolves payments the bank never answered about.
//
// This is what turns "recoverable in principle" into "recovered automatically".
// Without it, a timed-out charge sits in `unknown` forever: the money may have
// moved, the customer has no ticket, and nobody finds out until they complain.
//
// MUST RUN AS A SINGLETON, like the inventory sweepers. Nothing here is unsafe
// concurrently — the bank is idempotent and the updates are idempotent — but N
// replicas means N times the traffic to the one dependency you least want to
// hammer.
type Reconciler struct {
	store  *Store
	bank   Charger
	minAge time.Duration
	batch  int

	// OnResolved is called for each payment that reached a definite outcome.
	OnResolved func(orderID string, state State)
	// OnStuck fires for payments the bank still cannot account for after
	// several attempts. That is a human problem, not a retry problem.
	OnStuck func(p *Payment)
	OnError func(error)
}

func NewReconciler(s *Store, bank Charger, minAge time.Duration) *Reconciler {
	return &Reconciler{store: s, bank: bank, minAge: minAge, batch: 50}
}

// stuckAfter is when repeated silence stops being a retry and becomes an alert.
const stuckAfter = 5

// Once resolves one batch and reports how many reached a definite outcome.
func (r *Reconciler) Once(ctx context.Context) (int, error) {
	// The reconciler resolves payments the bank never answered for. Like the other
	// two loops it is invisible when healthy and equally invisible when dead.
	ctx, span := tracer.Start(ctx, "reconcile")
	defer span.End()

	pending, err := r.store.Unresolved(ctx, r.minAge, r.batch)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "list unresolved")
		return 0, fmt.Errorf("list unresolved: %w", err)
	}
	span.SetAttributes(attribute.Int("payments.unresolved", len(pending)))

	resolved := 0
	for _, p := range pending {
		done, err := r.resolveOne(ctx, p)
		if err != nil {
			r.report(fmt.Errorf("reconcile %s: %w", p.OrderID, err))
			continue
		}
		if done {
			resolved++
			reconciled.Add(ctx, 1)
		}
	}
	return resolved, nil
}

func (r *Reconciler) resolveOne(ctx context.Context, p *Payment) (bool, error) {
	// ASK, DO NOT RE-CHARGE. Lookup is a question; Authorize is an action. Even
	// though the bank is idempotent and a repeat would be safe, asking is the
	// honest operation here — the goal is to find out what happened, not to make
	// something happen.
	charge, found, err := r.bank.Lookup(ctx, p.IdempotencyKey)
	if err != nil {
		// The bank is unreachable. Leave the payment unresolved and try later;
		// treating this as failure would be a guess.
		if mErr := r.store.MarkUnknown(ctx, p.OrderID); mErr != nil {
			return false, errors.Join(err, mErr)
		}
		if p.ReconcileAttempts+1 >= stuckAfter && r.OnStuck != nil {
			r.OnStuck(p)
		}
		return false, nil
	}

	if !found {
		// The bank has no record, so no money moved. The charge can now be
		// attempted for real — the request never reached it.
		charge, err = r.bank.Authorize(ctx, p.IdempotencyKey, p.AmountMinor)
		switch {
		case errors.Is(err, bankclient.ErrDeclined):
			return true, r.finish(ctx, p, StateFailed, "", charge.DeclineCode)
		case err != nil:
			// Still no answer. Count it and come back.
			if mErr := r.store.MarkUnknown(ctx, p.OrderID); mErr != nil {
				return false, mErr
			}
			if p.ReconcileAttempts+1 >= stuckAfter && r.OnStuck != nil {
				r.OnStuck(p)
			}
			return false, nil
		}
	}

	if charge.Status == "declined" {
		return true, r.finish(ctx, p, StateFailed, "", charge.DeclineCode)
	}
	return true, r.finish(ctx, p, StateSucceeded, charge.ID, "")
}

func (r *Reconciler) finish(ctx context.Context, p *Payment, state State, chargeID, declineCode string) error {
	if err := r.store.Resolve(ctx, p.OrderID, state, chargeID, declineCode); err != nil {
		return err
	}
	if r.OnResolved != nil {
		r.OnResolved(p.OrderID.String(), state)
	}
	return nil
}

// Run reconciles on a ticker until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context, every time.Duration) {
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

func (r *Reconciler) report(err error) {
	if r.OnError != nil {
		r.OnError(err)
	}
}

var (
	tracer = otel.Tracer("payments")
	meter  = otel.Meter("payments")

	// Payments the bank never answered for, that the reconciler had to establish
	// the truth about afterwards. THE UNKNOWN STATE IS THE DANGEROUS ONE — it is
	// the only path where a customer can be charged for seats they did not get —
	// so how often it happens is worth a number rather than an anecdote.
	reconciled, _ = meter.Int64Counter("tickets.payments.reconciled",
		metric.WithDescription("Payments resolved by the reconciler after an unknown outcome"),
		metric.WithUnit("{payment}"))
)
