package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/slash3b/tickets/services/catalog"
	"github.com/slash3b/tickets/services/inventory"
	inventorystore "github.com/slash3b/tickets/services/inventory/store"
)

// The gateway holds NO stores and NO database handle. It calls catalog,
// inventory and orders over HTTP and assembles their answers.
//
// Only catalog needs an adapter. The inventory and orders clients already satisfy
// the Inventory and Orders interfaces on this side METHOD FOR METHOD — which is
// not luck: those interfaces were declared by this package, describing what it
// needed rather than what a store happened to offer, and the services were built
// to them. Catalog needs one only because its wire types carry fields the gateway
// does not serve.

type CatalogClient struct{ C *catalog.Client }

func (a CatalogClient) ListOnSale(ctx context.Context, limit int) ([]Event, error) {
	rows, err := a.C.ListOnSale(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Event, len(rows))
	for i, e := range rows {
		out[i] = Event{ID: e.ID, Title: e.Title, Venue: e.Venue, StartsAt: e.StartsAt, OnSaleAt: e.OnSaleAt}
	}
	return out, nil
}

func (a CatalogClient) GetEvent(ctx context.Context, id uuid.UUID) (*Event, error) {
	e, err := a.C.GetEvent(ctx, id)
	if err != nil {
		return nil, err
	}
	return &Event{ID: e.ID, Title: e.Title, Venue: e.Venue, StartsAt: e.StartsAt, OnSaleAt: e.OnSaleAt}, nil
}

func (a CatalogClient) Sections(ctx context.Context, eventID uuid.UUID) ([]Section, error) {
	rows, err := a.C.Sections(ctx, eventID)
	if err != nil {
		return nil, err
	}
	out := make([]Section, len(rows))
	for i, s := range rows {
		out[i] = Section{ID: s.ID, Name: s.Name, Seats: s.Seats}
	}
	return out, nil
}

func (a CatalogClient) SectionSeats(ctx context.Context, sectionID uuid.UUID) ([]Seat, error) {
	rows, err := a.C.SectionSeats(ctx, sectionID)
	if err != nil {
		return nil, err
	}
	out := make([]Seat, len(rows))
	for i, s := range rows {
		out[i] = Seat{ID: s.ID, Row: s.Row, Number: s.Number, X: s.X, Y: s.Y}
	}
	return out, nil
}

// InventoryClient translates inventory's sentinel into the gateway's own.
//
// THIS ADAPTER IS NOT CEREMONY, and removing it is a real bug that the e2e test
// caught immediately: without it a lost race came back as 500 instead of 409.
// The gateway declares ErrSeatsGone as ITS vocabulary for "someone beat you to
// it", and the fact that inventory happens to spell the same idea
// ErrSeatsUnavailable is inventory's business. The two were joined by the old
// store adapter; passing the HTTP client straight through quietly unjoined them,
// because errors.Is compares identity and these are two different values.
type InventoryClient struct{ C *inventory.Client }

func (a InventoryClient) Hold(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID, ttl time.Duration) (uuid.UUID, error) {
	id, err := a.C.Hold(ctx, eventID, seatIDs, ttl)
	if errors.Is(err, inventorystore.ErrSeatsUnavailable) {
		return uuid.Nil, ErrSeatsGone
	}
	return id, err
}

func (a InventoryClient) Release(ctx context.Context, holdID uuid.UUID, reason string) error {
	return a.C.Release(ctx, holdID, reason)
}

func (a InventoryClient) SeatStatuses(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	return a.C.SeatStatuses(ctx, eventID, seatIDs)
}
