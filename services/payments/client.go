package payments

import (
	"context"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	pb "github.com/slash3b/tickets/gen/tickets/v1"
)

type Client struct{ c pb.PaymentsServiceClient }

func NewClient(cc grpc.ClientConnInterface) *Client {
	return &Client{c: pb.NewPaymentsServiceClient(cc)}
}

// Charge returns the wire outcome. The caller maps it to its own type.
//
// A TRANSPORT FAILURE IS NOT A DECLINE, and the caller must treat it as unknown —
// see orders.PaymentsAdapter, which is where that decision lives.
func (c *Client) Charge(ctx context.Context, orderID uuid.UUID, amountMinor int64) (pb.Outcome, string, error) {
	resp, err := c.c.Charge(ctx, &pb.ChargeRequest{
		OrderId: orderID.String(), AmountMinor: amountMinor,
	})
	if err != nil {
		return pb.Outcome_OUTCOME_UNKNOWN, "", err
	}
	return resp.GetOutcome(), resp.GetDeclineCode(), nil
}

func (c *Client) Reconcile(ctx context.Context) (int, error) {
	resp, err := c.c.Reconcile(ctx, &pb.ReconcileRequest{})
	return int(resp.GetResolved()), err
}
