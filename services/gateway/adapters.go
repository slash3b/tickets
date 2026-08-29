package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	pb "github.com/slash3b/tickets/gen/tickets/v1"
	"github.com/slash3b/tickets/services/catalog"
	"github.com/slash3b/tickets/services/inventory"
	inventorystore "github.com/slash3b/tickets/services/inventory/store"
)

// The gateway holds NO stores and NO database handle. It calls catalog,
// inventory and orders over gRPC and assembles their answers into the shapes a
// seat map needs.
//
// The adapters translate between each service's wire types and the gateway's own
// JSON. They are the only place that knows both, which is what lets either side
// change without the other noticing.

type CatalogClient struct{ C *catalog.Client }

func (a CatalogClient) ListOnSale(ctx context.Context, limit int) ([]Event, error) {
	rows, err := a.C.ListOnSale(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, e := range rows {
		ev, err := event(e)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

func (a CatalogClient) ListUpcoming(ctx context.Context, limit int) ([]Event, error) {
	rows, err := a.C.ListUpcoming(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, e := range rows {
		ev, err := event(e)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

func (a CatalogClient) GetEvent(ctx context.Context, id uuid.UUID) (*Event, error) {
	e, err := a.C.GetEvent(ctx, id)
	if err != nil {
		return nil, err
	}
	ev, err := event(e)
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

func (a CatalogClient) Sections(ctx context.Context, eventID uuid.UUID) ([]Section, error) {
	rows, err := a.C.Sections(ctx, eventID)
	if err != nil {
		return nil, err
	}
	out := make([]Section, 0, len(rows))
	for _, s := range rows {
		id, err := uuid.Parse(s.GetId())
		if err != nil {
			return nil, err
		}
		out = append(out, Section{ID: id, Name: s.GetName(), Seats: int(s.GetSeats())})
	}
	return out, nil
}

func (a CatalogClient) SectionSeats(ctx context.Context, sectionID uuid.UUID) ([]Seat, error) {
	rows, err := a.C.SectionSeats(ctx, sectionID)
	if err != nil {
		return nil, err
	}
	out := make([]Seat, 0, len(rows))
	for _, s := range rows {
		id, err := uuid.Parse(s.GetId())
		if err != nil {
			return nil, err
		}
		out = append(out, Seat{
			ID: id, Row: s.GetRow(), Number: int(s.GetNumber()), X: s.GetX(), Y: s.GetY(),
		})
	}
	return out, nil
}

func event(e *pb.Event) (Event, error) {
	id, err := uuid.Parse(e.GetId())
	if err != nil {
		return Event{}, err
	}
	return Event{
		ID:       id,
		Title:    e.GetTitle(),
		Venue:    e.GetVenue(),
		StartsAt: e.GetStartsAt().AsTime(),
		OnSaleAt: e.GetOnSaleAt().AsTime(),
	}, nil
}

// InventoryClient translates inventory's sentinel into the gateway's own.
//
// THIS ADAPTER IS NOT CEREMONY, and removing it is a real bug the e2e test caught
// the moment the services were split: without it a lost race came back as 500
// instead of 409. The gateway declares ErrSeatsGone as ITS vocabulary for
// "someone beat you to it"; that inventory spells the same idea
// ErrSeatsUnavailable — and gRPC spells it codes.Aborted — is their business.
// errors.Is compares identity, so the three have to be joined somewhere explicit.
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
