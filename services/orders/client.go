package orders

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/slash3b/tickets/pkg/rpc"
)

// Client talks to the orders service.
type Client struct{ c *rpc.Client }

func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{c: rpc.New(baseURL, timeout)}
}

func (c *Client) Place(ctx context.Context, holdID, eventID, userID uuid.UUID, amountMinor int64) (uuid.UUID, error) {
	var out placeResponse
	err := c.c.Do(ctx, http.MethodPost, "/orders",
		placeRequest{HoldID: holdID, EventID: eventID, UserID: userID, AmountMinor: amountMinor}, &out)
	return out.OrderID, err
}

func (c *Client) Status(ctx context.Context, orderID uuid.UUID) (string, error) {
	var out placeResponse
	err := c.c.Do(ctx, http.MethodGet, "/orders/"+orderID.String(), nil, &out)
	return out.State, err
}

// Resume runs one resumer pass. Driven by workers.
func (c *Client) Resume(ctx context.Context) (int, error) {
	var out struct {
		Resumed int `json:"resumed"`
	}
	err := c.c.Do(ctx, http.MethodPost, "/internal/resume", nil, &out)
	return out.Resumed, err
}
