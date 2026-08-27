package store

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/slash3b/tickets/services/bank"
	"github.com/slash3b/tickets/services/payments/bankclient"
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
		t.Fatalf("apply schema: %v", err)
	}

	// ISOLATE. The reconciler deliberately processes EVERY unresolved payment in
	// the table — that is what it is for. So a payment another test left in
	// `pending` gets reconciled here too, against this test's bank, and the charge
	// count assertions see work that was not theirs. Truncating is the honest fix;
	// narrowing the reconciler's query would break the thing being tested.
	if _, err := pool.Exec(ctx, `TRUNCATE payments.payments`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	t.Cleanup(pool.Close)
	return New(pool)
}

func newBank(t *testing.T, cfg bank.Config) (*bank.Bank, *bankclient.Client) {
	t.Helper()
	b := bank.New(cfg)
	srv := httptest.NewServer(b.Handler())
	t.Cleanup(srv.Close)
	return b, bankclient.New(srv.URL, 300*time.Millisecond)
}

// TestIdempotencyKeyIsStable — the key must survive a process restart, because a
// crash between creating a payment and hearing back is exactly when a fresh key
// would cause a second charge.
func TestIdempotencyKeyIsStable(t *testing.T) {
	id := uuid.New()
	if a, b := IdempotencyKey(id), IdempotencyKey(id); a != b {
		t.Fatalf("key changed between calls: %s vs %s", a, b)
	}
	if IdempotencyKey(uuid.New()) == IdempotencyKey(uuid.New()) {
		t.Fatal("different orders produced the same key")
	}
}

// TestCreateIsIdempotent — a duplicated request must not create a second payment.
func TestCreateIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	order := uuid.New()

	first, err := s.Create(ctx, order, 5000)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Create(ctx, order, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("two payments created for one order: %s and %s", first.ID, second.ID)
	}
}

// TestReconcilerRecoversATimedOutCharge is the point of this milestone.
//
// The bank takes the money and never answers. The payment lands in `unknown` —
// NOT failed. The reconciler then asks the bank what actually happened and
// resolves it to succeeded, without charging again.
func TestReconcilerRecoversATimedOutCharge(t *testing.T) {
	s := newTestStore(t)
	b, client := newBank(t, bank.Config{TimeoutRate: 1.0}) // never answers
	ctx := context.Background()
	order := uuid.New()

	p, err := s.Create(ctx, order, 7500)
	if err != nil {
		t.Fatal(err)
	}

	// The charge attempt: money moves, caller hears nothing.
	if _, err := client.Authorize(ctx, p.IdempotencyKey, p.AmountMinor); err == nil {
		t.Fatal("expected the bank to time out")
	}
	if err := s.MarkUnknown(ctx, order); err != nil {
		t.Fatal(err)
	}

	if got, _ := s.Get(ctx, order); got.State != StateUnknown {
		t.Fatalf("state = %q, want unknown — a timeout is not a failure", got.State)
	}
	if n := b.ChargeCount(); n != 1 {
		t.Fatalf("bank has %d charges, want 1 — the money did move", n)
	}

	// The reconciler establishes the truth.
	b.SetConfig(bank.Config{}) // bank answers again
	r := NewReconciler(s, client, 0)
	n, err := r.Once(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n < 1 {
		t.Fatal("reconciler resolved nothing")
	}

	got, err := s.Get(ctx, order)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateSucceeded {
		t.Fatalf("state = %q, want succeeded — the charge did happen", got.State)
	}
	if got.BankChargeID == "" {
		t.Fatal("succeeded payment has no charge id")
	}
	if n := b.ChargeCount(); n != 1 {
		t.Fatalf("bank has %d charges after reconciliation, want 1 — DOUBLE CHARGE", n)
	}
}

// TestReconcilerChargesWhenTheBankNeverSawIt — the opposite case. If the request
// never arrived, no money moved, and the charge should actually be made.
func TestReconcilerChargesWhenTheBankNeverSawIt(t *testing.T) {
	s := newTestStore(t)
	b, client := newBank(t, bank.Config{})
	ctx := context.Background()
	order := uuid.New()

	if _, err := s.Create(ctx, order, 1200); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnknown(ctx, order); err != nil { // as if a request was lost in flight
		t.Fatal(err)
	}

	r := NewReconciler(s, client, 0)
	if _, err := r.Once(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got, _ := s.Get(ctx, order)
	if got.State != StateSucceeded {
		t.Fatalf("state = %q, want succeeded", got.State)
	}
	if n := b.ChargeCount(); n != 1 {
		t.Fatalf("bank has %d charges, want exactly 1", n)
	}
}

// TestUnknownIsNotFailed guards the distinction the whole design rests on. If an
// unreachable bank marked payments failed, a customer who HAD been charged would
// be told their payment failed.
func TestUnknownIsNotFailed(t *testing.T) {
	s := newTestStore(t)
	_, client := newBank(t, bank.Config{Outage: true})
	ctx := context.Background()
	order := uuid.New()

	if _, err := s.Create(ctx, order, 999); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnknown(ctx, order); err != nil {
		t.Fatal(err)
	}

	r := NewReconciler(s, client, 0)
	if _, err := r.Once(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got, _ := s.Get(ctx, order)
	if got.State == StateFailed {
		t.Fatal("an unreachable bank must never produce `failed` — that tells a " +
			"customer who may have been charged that their payment did not happen")
	}
	if got.State != StateUnknown {
		t.Fatalf("state = %q, want it to stay unknown", got.State)
	}
	if got.ReconcileAttempts < 1 {
		t.Fatal("reconcile attempt was not counted")
	}
}
