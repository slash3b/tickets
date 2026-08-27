package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)


func newTestStore(t *testing.T) *Store {
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
	if _, err := pool.Exec(ctx, SchemaSQL); err != nil {
		t.Fatalf("schema: %v", err)
	}
	// The resumer processes every in-flight order, so tests must not see each
	// other's leftovers. See the same note in payments.
	if _, err := pool.Exec(ctx, `TRUNCATE orders.orders CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return New(pool)
}

// fakeInventory records what the saga did to the hold.
type fakeInventory struct {
	converted, committed, released int
	commitErr                      error
	releaseReason                  string
}

func (f *fakeInventory) Convert(context.Context, uuid.UUID) error { f.converted++; return nil }
func (f *fakeInventory) Commit(context.Context, uuid.UUID) error {
	f.committed++
	return f.commitErr
}
func (f *fakeInventory) Release(_ context.Context, _ uuid.UUID, reason string) error {
	f.released++
	f.releaseReason = reason
	return nil
}

type fakePayments struct {
	outcome     PaymentOutcome
	declineCode string
	charges     int
}

func (f *fakePayments) Charge(context.Context, uuid.UUID, int64) (PaymentOutcome, string, error) {
	f.charges++
	return f.outcome, f.declineCode, nil
}

func newOrder(t *testing.T, s *Store) *Order {
	t.Helper()
	o, err := s.Create(context.Background(), uuid.New(), uuid.New(), uuid.New(), 4500)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestHappyPathReachesConfirmed(t *testing.T) {
	s := newTestStore(t)
	inv := &fakeInventory{}
	pay := &fakePayments{outcome: PaymentSucceeded}
	o := newOrder(t, s)
	ctx := context.Background()

	if err := NewSaga(s, inv, pay).Run(ctx, o.ID); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, _ := s.Get(ctx, o.ID)
	if got.State != StateConfirmed {
		t.Fatalf("state = %q, want confirmed", got.State)
	}
	if inv.converted != 1 || inv.committed != 1 || pay.charges != 1 {
		t.Fatalf("converted=%d charged=%d committed=%d, want 1 each",
			inv.converted, pay.charges, inv.committed)
	}

	// The log must show the order of operations, because that ordering is the
	// design: the hold is converted BEFORE any money moves.
	steps, _ := s.Steps(ctx, o.ID)
	t.Logf("saga log: %v", steps)
	if len(steps) < 6 {
		t.Fatalf("only %d log entries; every step should be logged before and after", len(steps))
	}
}

// TestDeclineReleasesSeats — a definite no means give the seats back immediately.
func TestDeclineReleasesSeats(t *testing.T) {
	s := newTestStore(t)
	inv := &fakeInventory{}
	pay := &fakePayments{outcome: PaymentFailed, declineCode: "insufficient_funds"}
	o := newOrder(t, s)
	ctx := context.Background()

	if err := NewSaga(s, inv, pay).Run(ctx, o.ID); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, _ := s.Get(ctx, o.ID)
	if got.State != StateFailed {
		t.Fatalf("state = %q, want failed", got.State)
	}
	if inv.released != 1 {
		t.Fatalf("released %d times, want 1 — a declined order must free its seats", inv.released)
	}
	if inv.committed != 0 {
		t.Fatal("seats were committed despite a declined payment")
	}
}

// TestUnknownPaymentDoesNotReleaseSeats guards the most expensive mistake in the
// saga. The money may have moved; releasing the seats here is how a paying
// customer loses them to someone else.
func TestUnknownPaymentDoesNotReleaseSeats(t *testing.T) {
	s := newTestStore(t)
	inv := &fakeInventory{}
	pay := &fakePayments{outcome: PaymentUnknown}
	o := newOrder(t, s)
	ctx := context.Background()

	if err := NewSaga(s, inv, pay).Run(ctx, o.ID); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, _ := s.Get(ctx, o.ID)
	if got.State != StateAwaitingPayment {
		t.Fatalf("state = %q, want it to stay awaiting_payment", got.State)
	}
	if inv.released != 0 {
		t.Fatal("seats were RELEASED on an unknown payment — the customer may have " +
			"paid, and those seats can now be sold to someone else")
	}
	if got.State == StateFailed {
		t.Fatal("an unknown payment must never fail the order")
	}
}

// TestResumerCompletesACrashedOrderForward is the milestone's headline test.
//
// A process died after the money moved but before the seats were sold. The order
// sits in `paid`. Recovery must go FORWARD — commit the seats — not backward.
// The seats are still held in `converting`, so nobody else can have taken them,
// and refunding would be the wrong answer to a purchase that can still complete.
func TestResumerCompletesACrashedOrderForward(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	o := newOrder(t, s)

	// Simulate the crash: money moved, seats not yet sold.
	if err := s.Advance(ctx, o.ID, StatePaid, "charge", ""); err != nil {
		t.Fatal(err)
	}

	inv := &fakeInventory{}
	pay := &fakePayments{outcome: PaymentSucceeded}
	saga := NewSaga(s, inv, pay)

	n, err := NewResumer(saga, s, 0).Once(ctx)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n != 1 {
		t.Fatalf("resumed %d orders, want 1", n)
	}

	got, _ := s.Get(ctx, o.ID)
	if got.State != StateConfirmed {
		t.Fatalf("state = %q, want confirmed — recovery goes forward", got.State)
	}
	if pay.charges != 0 {
		t.Fatalf("the resumer charged %d more times — the money had already moved", pay.charges)
	}
	if inv.released != 0 {
		t.Fatal("the resumer released the seats instead of completing the purchase")
	}
	if inv.committed != 1 {
		t.Fatalf("committed %d times, want 1", inv.committed)
	}
}

// TestReleasedHoldNeedsRefund — the one case where the system holds money it
// cannot honour. It must be visible as `reconciling`, not silently confirmed.
func TestReleasedHoldNeedsRefund(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	o := newOrder(t, s)

	if err := s.Advance(ctx, o.ID, StatePaid, "charge", ""); err != nil {
		t.Fatal(err)
	}

	inv := &fakeInventory{commitErr: ErrHoldGone}
	saga := NewSaga(s, inv, &fakePayments{outcome: PaymentSucceeded})

	if err := saga.Run(ctx, o.ID); err != nil && !errors.Is(err, ErrHoldGone) {
		t.Fatalf("run: %v", err)
	}

	got, _ := s.Get(ctx, o.ID)
	if got.State != StateReconciling {
		t.Fatalf("state = %q, want reconciling — money was taken for seats that are gone", got.State)
	}
	if got.FailureReason == "" {
		t.Fatal("reconciling order carries no reason; a human has to act on this")
	}
}
