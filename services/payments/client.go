package payments

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/slash3b/tickets/pkg/rpc"
)

// Client talks to the payments service.
type Client struct{ c *rpc.Client }

func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{c: rpc.New(baseURL, timeout)}
}

// Charge returns the outcome as a string, which the caller maps to its own type.
//
// A TIMEOUT HERE IS NOT A FAILURE, and this is the subtlest thing in the split.
// If this call times out, the payments service may still have charged the card —
// the request got there, the answer did not come back. The caller must treat a
// transport error the same way it treats an explicit "unknown": leave the seats
// held and let the reconciler settle it. Returning an error that reads like
// "failed" here would be the bug that loses a paying customer their seats.
func (c *Client) Charge(ctx context.Context, orderID uuid.UUID, amountMinor int64) (outcome, declineCode string, err error) {
	var out chargeResponse
	err = c.c.Do(ctx, http.MethodPost, "/charges",
		chargeRequest{OrderID: orderID, AmountMinor: amountMinor}, &out)
	if err != nil {
		return "unknown", "", err
	}
	return out.Outcome, out.DeclineCode, nil
}

// Reconcile runs one reconciler pass. Driven by workers.
func (c *Client) Reconcile(ctx context.Context) (int, error) {
	var out struct {
		Resolved int `json:"resolved"`
	}
	err := c.c.Do(ctx, http.MethodPost, "/internal/reconcile", nil, &out)
	return out.Resolved, err
}
