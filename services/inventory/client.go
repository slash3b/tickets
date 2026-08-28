package inventory

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/slash3b/tickets/pkg/rpc"
	"github.com/slash3b/tickets/services/inventory/store"
)

// Client talks to the inventory service.
//
// It re-raises the store's OWN sentinel errors — store.ErrSeatsUnavailable and
// store.ErrHoldReleased — so callers keep using errors.Is exactly as they did
// when this was a function call. That is what made the split wiring rather than
// rework: the gateway's 409 handling and the saga's "seats are gone" branch did
// not change a line.
type Client struct{ c *rpc.Client }

func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{c: rpc.New(baseURL, timeout)}
}

func (c *Client) Hold(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID, ttl time.Duration) (uuid.UUID, error) {
	var out holdResponse
	err := c.c.Do(ctx, http.MethodPost, "/holds",
		holdRequest{EventID: eventID, SeatIDs: seatIDs, TTL: ttl}, &out)
	if rpc.CodeOf(err) == CodeSeatsUnavailable {
		return uuid.Nil, store.ErrSeatsUnavailable
	}
	return out.HoldID, err
}

func (c *Client) Release(ctx context.Context, holdID uuid.UUID, reason string) error {
	return c.c.Do(ctx, http.MethodDelete, "/holds/"+holdID.String(), releaseRequest{Reason: reason}, nil)
}

func (c *Client) Convert(ctx context.Context, holdID uuid.UUID) error {
	return c.c.Do(ctx, http.MethodPost, "/holds/"+holdID.String()+"/convert", nil, nil)
}

func (c *Client) Commit(ctx context.Context, holdID uuid.UUID) error {
	err := c.c.Do(ctx, http.MethodPost, "/holds/"+holdID.String()+"/commit", nil, nil)
	if rpc.CodeOf(err) == CodeHoldReleased {
		return store.ErrHoldReleased
	}
	return err
}

func (c *Client) OpenEvent(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID) (int, error) {
	var out struct {
		Opened int `json:"opened"`
	}
	err := c.c.Do(ctx, http.MethodPost, "/events/"+eventID.String()+"/open",
		openRequest{SeatIDs: seatIDs}, &out)
	return out.Opened, err
}

func (c *Client) SeatStatuses(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	var out struct {
		Statuses map[string]string `json:"statuses"`
	}
	if err := c.c.Do(ctx, http.MethodPost, "/events/"+eventID.String()+"/seat-status",
		seatStatusRequest{SeatIDs: seatIDs}, &out); err != nil {
		return nil, err
	}
	statuses := make(map[uuid.UUID]string, len(out.Statuses))
	for k, v := range out.Statuses {
		id, err := uuid.Parse(k)
		if err != nil {
			continue // a key we cannot parse is not worth failing the whole map for
		}
		statuses[id] = v
	}
	return statuses, nil
}

// Sweep runs one sweeper pass. Called by workers on a ticker, never by a request.
func (c *Client) Sweep(ctx context.Context) (expired, hardDeadline int, err error) {
	var out struct {
		Expired      int `json:"expired"`
		HardDeadline int `json:"hard_deadline"`
	}
	err = c.c.Do(ctx, http.MethodPost, "/internal/sweep", nil, &out)
	return out.Expired, out.HardDeadline, err
}
