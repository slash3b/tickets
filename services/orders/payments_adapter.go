package orders

import (
	"context"

	"github.com/google/uuid"

	"github.com/slash3b/tickets/services/orders/store"
	"github.com/slash3b/tickets/services/payments"
)

// PaymentsAdapter maps the payments client's wire outcome to the saga's type.
//
// It lives in the package rather than in main so that the tests exercise the
// SAME mapping the binary uses. A second copy in a test would have happily
// disagreed with production about what a timeout means, which is the one thing
// here worth being sure about.
type PaymentsAdapter struct{ C *payments.Client }

func (p PaymentsAdapter) Charge(ctx context.Context, orderID uuid.UUID, amountMinor int64) (store.PaymentOutcome, string, error) {
	outcome, declineCode, err := p.C.Charge(ctx, orderID, amountMinor)
	if err != nil {
		// A TRANSPORT FAILURE IS "UNKNOWN", NOT AN ERROR, and this is the most
		// important line added by the split.
		//
		// If the call timed out, payments may have charged the card anyway — the
		// request arrived, the answer did not come back. Reporting that as a
		// failure would release the seats of someone who has just paid for them.
		// Reporting it as unknown leaves the hold in `converting`, where nobody
		// can take the seats, until the reconciler establishes what really
		// happened. Retrying is safe: the payment row is upserted per order and
		// the bank's idempotency key is derived from the order id, so one order
		// can only ever be charged once.
		return store.PaymentUnknown, "", nil
	}

	switch outcome {
	case "succeeded":
		return store.PaymentSucceeded, "", nil
	case "failed":
		return store.PaymentFailed, declineCode, nil
	default:
		return store.PaymentUnknown, "", nil
	}
}
