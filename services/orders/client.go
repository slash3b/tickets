package orders

import (
	"context"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	pb "github.com/slash3b/tickets/gen/tickets/v1"
)

type Client struct{ c pb.OrdersServiceClient }

func NewClient(cc grpc.ClientConnInterface) *Client {
	return &Client{c: pb.NewOrdersServiceClient(cc)}
}

func (c *Client) Place(ctx context.Context, holdID, eventID, userID uuid.UUID, amountMinor int64) (uuid.UUID, error) {
	resp, err := c.c.Place(ctx, &pb.PlaceRequest{
		HoldId:      holdID.String(),
		EventId:     eventID.String(),
		UserId:      userID.String(),
		AmountMinor: amountMinor,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(resp.GetOrderId())
}

func (c *Client) Status(ctx context.Context, orderID uuid.UUID) (string, error) {
	resp, err := c.c.GetOrder(ctx, &pb.GetOrderRequest{OrderId: orderID.String()})
	return resp.GetState(), err
}

func (c *Client) Resume(ctx context.Context) (int, error) {
	resp, err := c.c.Resume(ctx, &pb.ResumeRequest{})
	return int(resp.GetResumed()), err
}
