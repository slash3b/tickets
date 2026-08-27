package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	catalogstore "github.com/slash3b/tickets/services/catalog/store"
	inventorystore "github.com/slash3b/tickets/services/inventory/store"
	ordersstore "github.com/slash3b/tickets/services/orders/store"
)

// The adapters translate between each service's own types and the shapes the
// gateway serves. They are the only place that knows both, which is what lets
// either side change without the other noticing.

type CatalogAdapter struct{ S *catalogstore.Store }

func (a CatalogAdapter) ListOnSale(ctx context.Context, limit int) ([]Event, error) {
	rows, err := a.S.ListOnSale(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Event, len(rows))
	for i, e := range rows {
		out[i] = Event{ID: e.ID, Title: e.Title, Venue: e.VenueName, StartsAt: e.StartsAt, OnSaleAt: e.OnSaleAt}
	}
	return out, nil
}

func (a CatalogAdapter) GetEvent(ctx context.Context, id uuid.UUID) (*Event, error) {
	e, err := a.S.GetEvent(ctx, id)
	if err != nil {
		return nil, err
	}
	return &Event{ID: e.ID, Title: e.Title, Venue: e.VenueName, StartsAt: e.StartsAt, OnSaleAt: e.OnSaleAt}, nil
}

func (a CatalogAdapter) Sections(ctx context.Context, eventID uuid.UUID) ([]Section, error) {
	rows, err := a.S.Sections(ctx, eventID)
	if err != nil {
		return nil, err
	}
	out := make([]Section, len(rows))
	for i, s := range rows {
		out[i] = Section{ID: s.ID, Name: s.Name, Seats: s.Seats}
	}
	return out, nil
}

func (a CatalogAdapter) SectionSeats(ctx context.Context, sectionID uuid.UUID) ([]Seat, error) {
	rows, err := a.S.SectionSeats(ctx, sectionID)
	if err != nil {
		return nil, err
	}
	out := make([]Seat, len(rows))
	for i, s := range rows {
		out[i] = Seat{ID: s.ID, Row: s.RowLabel, Number: s.Number, X: s.X, Y: s.Y}
	}
	return out, nil
}

type InventoryAdapter struct{ S *inventorystore.Store }

func (a InventoryAdapter) Hold(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID, ttl time.Duration) (uuid.UUID, error) {
	id, err := a.S.Hold(ctx, eventID, seatIDs, ttl)
	if errors.Is(err, inventorystore.ErrSeatsUnavailable) {
		// Translate to the gateway's vocabulary so the HTTP layer never has to
		// import inventory's errors to know this is a 409.
		return uuid.Nil, ErrSeatsGone
	}
	return id, err
}

func (a InventoryAdapter) Release(ctx context.Context, holdID uuid.UUID, reason string) error {
	return a.S.Release(ctx, holdID, reason)
}

func (a InventoryAdapter) SeatStatuses(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	return a.S.SeatStatuses(ctx, eventID, seatIDs)
}

// Convert and Commit are not part of gateway.Inventory — a browser must never
// reach them. They are here because the saga needs them and this adapter already
// wraps the right store; the HTTP layer cannot call what its interface omits.

func (a InventoryAdapter) Convert(ctx context.Context, holdID uuid.UUID) error {
	return a.S.Convert(ctx, holdID)
}

func (a InventoryAdapter) Commit(ctx context.Context, holdID uuid.UUID) error {
	err := a.S.Commit(ctx, holdID)
	if errors.Is(err, inventorystore.ErrHoldReleased) {
		// Translate into the saga's vocabulary. This is the refund path, and the
		// saga must be able to tell it apart from a transient failure WITHOUT
		// importing inventory's errors — one is "try again", the other is "money
		// was taken for seats that no longer exist".
		return ordersstore.ErrHoldGone
	}
	return err
}

// OrdersAdapter creates the order and runs the saga far enough to answer.
type OrdersAdapter struct {
	S    *ordersstore.Store
	Saga *ordersstore.Saga
}

func (a OrdersAdapter) Place(ctx context.Context, holdID, eventID, userID uuid.UUID, amountMinor int64) (uuid.UUID, error) {
	o, err := a.S.Create(ctx, holdID, eventID, userID, amountMinor)
	if err != nil {
		return uuid.Nil, err
	}
	// Run inline so the caller gets a real answer. A saga that cannot finish now
	// leaves the order in a resumable state rather than failing the request — the
	// resumer will pick it up, and the SPA polls GET /api/orders/{id}.
	if err := a.Saga.Run(ctx, o.ID); err != nil {
		return o.ID, nil
	}
	return o.ID, nil
}

func (a OrdersAdapter) Status(ctx context.Context, orderID uuid.UUID) (string, error) {
	o, err := a.S.Get(ctx, orderID)
	if err != nil {
		return "", err
	}
	if o == nil {
		return "", errors.New("no such order")
	}
	return string(o.State), nil
}
