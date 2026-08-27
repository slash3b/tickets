package bankclient

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/slash3b/tickets/services/bank"
)

func newBank(t *testing.T, cfg bank.Config) (*bank.Bank, *Client) {
	t.Helper()
	b := bank.New(cfg)
	srv := httptest.NewServer(b.Handler())
	t.Cleanup(srv.Close)
	return b, New(srv.URL, 300*time.Millisecond)
}

// TestTimeoutDoesNotDoubleCharge is the reason milestone 2 exists.
//
// The bank commits the charge and then never answers. The caller sees a timeout
// for a payment that SUCCEEDED. If a retry created a second charge, every network
// blip would bill a customer twice — and no amount of care on the client side
// could prevent it, because the client cannot tell this case from a real failure.
func TestTimeoutDoesNotDoubleCharge(t *testing.T) {
	b, c := newBank(t, bank.Config{TimeoutRate: 1.0}) // every request hangs
	ctx := context.Background()
	const key = "order-42"

	_, err := c.Authorize(ctx, key, 5000)
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("err = %v, want ErrUnknown — a timeout must never read as failure", err)
	}

	// The money moved even though the caller was told nothing.
	if n := b.ChargeCount(); n != 1 {
		t.Fatalf("bank recorded %d charges, want 1 — the charge did happen", n)
	}

	// Now retry, as any sane caller would.
	b.SetConfig(bank.Config{}) // bank behaves this time
	if _, err := c.Authorize(ctx, key, 5000); err != nil {
		t.Fatalf("retry: %v", err)
	}

	if n := b.ChargeCount(); n != 1 {
		t.Fatalf("bank recorded %d charges after a retry, want 1 — DOUBLE CHARGE", n)
	}
}

// TestReconcileFindsTheLostCharge proves the recovery path: after a timeout the
// caller can discover what actually happened instead of guessing.
func TestReconcileFindsTheLostCharge(t *testing.T) {
	b, c := newBank(t, bank.Config{TimeoutRate: 1.0})
	ctx := context.Background()

	ch, err := c.AuthorizeAndReconcile(ctx, "order-77", 2500)
	if err != nil {
		t.Fatalf("AuthorizeAndReconcile: %v", err)
	}
	if ch == nil || ch.Status != "authorized" {
		t.Fatalf("charge = %+v, want an authorized charge recovered by lookup", ch)
	}
	if n := b.ChargeCount(); n != 1 {
		t.Fatalf("bank recorded %d charges, want 1", n)
	}
}

// TestRetryDoesNotRerollTheVerdict is subtle and matters. If a repeated key made
// the bank decide again, a retry could turn an authorization into a decline — and
// the caller would have no way to know which answer was the real one.
func TestRetryDoesNotRerollTheVerdict(t *testing.T) {
	b, c := newBank(t, bank.Config{DeclineRate: 1.0}) // always decline
	ctx := context.Background()
	const key = "order-99"

	if _, err := c.Authorize(ctx, key, 100); !errors.Is(err, ErrDeclined) {
		t.Fatalf("first: err = %v, want ErrDeclined", err)
	}

	b.SetConfig(bank.Config{DeclineRate: 0.0}) // bank would now approve anything new
	ch, err := c.Authorize(ctx, key, 100)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("repeat with same key: err = %v, want ErrDeclined — the original "+
			"verdict must stand, not be re-decided", err)
	}
	if ch.Status != "declined" {
		t.Fatalf("status = %q, want declined", ch.Status)
	}
	if n := b.ChargeCount(); n != 1 {
		t.Fatalf("bank recorded %d charges, want 1", n)
	}
}

// TestOutageIsUnknownNotFailure — a 5xx says nothing about whether money moved.
// Only an explicit decline is a definite no.
func TestOutageIsUnknownNotFailure(t *testing.T) {
	_, c := newBank(t, bank.Config{Outage: true})

	_, err := c.Authorize(context.Background(), "order-1", 100)
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("err = %v, want ErrUnknown for a 5xx", err)
	}
	if errors.Is(err, ErrDeclined) {
		t.Fatal("an outage must never read as a decline")
	}
}

// TestReconcileReportsSafeToRetry — when the bank genuinely never saw the
// request, the caller is told so and can retry without fear.
func TestReconcileReportsSafeToRetry(t *testing.T) {
	_, c := newBank(t, bank.Config{Outage: true})

	_, err := c.AuthorizeAndReconcile(context.Background(), "order-never", 100)
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("err = %v, want ErrUnknown", err)
	}
	if got := err.Error(); !contains(got, "safe to retry") {
		t.Fatalf("err = %q, want it to say the retry is safe", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
