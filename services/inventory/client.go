package inventory

import (
	"context"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	pb "github.com/slash3b/tickets/gen/tickets/v1"
	"github.com/slash3b/tickets/services/inventory/store"
)

// Client wraps the generated stub in the domain signatures callers already use.
//
// It re-raises the store's OWN sentinels, so the saga's "seats are gone" branch
// and the gateway's 409 handling keep working with errors.Is exactly as they did
// when this was a function call. THE STATUS CODE IS WHAT SURVIVES THE HOP; this
// is where it turns back into an error a Go caller can switch on.
type Client struct{ c pb.InventoryServiceClient }

func NewClient(cc grpc.ClientConnInterface) *Client {
	return &Client{c: pb.NewInventoryServiceClient(cc)}
}

func (c *Client) Hold(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID, ttl time.Duration) (uuid.UUID, error) {
	resp, err := c.c.Hold(ctx, &pb.HoldRequest{
		EventId: eventID.String(),
		SeatIds: uuidStrings(seatIDs),
		Ttl:     durationpb.New(ttl),
	})
	if status.Code(err) == codes.Aborted {
		return uuid.Nil, store.ErrSeatsUnavailable
	}
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(resp.GetHoldId())
}

func (c *Client) Release(ctx context.Context, holdID uuid.UUID, reason string) error {
	_, err := c.c.Release(ctx, &pb.ReleaseRequest{HoldId: holdID.String(), Reason: reason})
	return err
}

func (c *Client) Convert(ctx context.Context, holdID uuid.UUID) error {
	_, err := c.c.Convert(ctx, &pb.ConvertRequest{HoldId: holdID.String()})
	return err
}

func (c *Client) Commit(ctx context.Context, holdID uuid.UUID) error {
	_, err := c.c.Commit(ctx, &pb.CommitRequest{HoldId: holdID.String()})
	if status.Code(err) == codes.FailedPrecondition {
		return store.ErrHoldReleased
	}
	return err
}

func (c *Client) OpenEvent(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID) (int, error) {
	resp, err := c.c.OpenEvent(ctx, &pb.OpenEventRequest{
		EventId: eventID.String(), SeatIds: uuidStrings(seatIDs),
	})
	return int(resp.GetOpened()), err
}

func (c *Client) SeatStatuses(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	resp, err := c.c.SeatStatuses(ctx, &pb.SeatStatusesRequest{
		EventId: eventID.String(), SeatIds: uuidStrings(seatIDs),
	})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(resp.GetStatuses()))
	for k, v := range resp.GetStatuses() {
		id, err := uuid.Parse(k)
		if err != nil {
			continue // one unparseable key is not worth failing the whole map
		}
		out[id] = v
	}
	return out, nil
}

func (c *Client) Sweep(ctx context.Context) (expired, hardDeadline int, err error) {
	resp, err := c.c.Sweep(ctx, &pb.SweepRequest{})
	return int(resp.GetExpired()), int(resp.GetHardDeadline()), err
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
