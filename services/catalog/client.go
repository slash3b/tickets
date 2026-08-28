package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/slash3b/tickets/gen/tickets/v1"
)

type Client struct{ c pb.CatalogServiceClient }

func NewClient(cc grpc.ClientConnInterface) *Client {
	return &Client{c: pb.NewCatalogServiceClient(cc)}
}

// ErrNoVenue means the catalog has no venue by that name — as opposed to the
// catalog being unreachable. The seeder must not confuse the two: one says build
// the cinema, the other says try again later.
var ErrNoVenue = errors.New("no such venue")

func (c *Client) ListOnSale(ctx context.Context, limit int) ([]*pb.Event, error) {
	resp, err := c.c.ListOnSale(ctx, &pb.ListOnSaleRequest{Limit: int32(limit)})
	return resp.GetEvents(), err
}

func (c *Client) GetEvent(ctx context.Context, id uuid.UUID) (*pb.Event, error) {
	resp, err := c.c.GetEvent(ctx, &pb.GetEventRequest{EventId: id.String()})
	return resp.GetEvent(), err
}

func (c *Client) Sections(ctx context.Context, eventID uuid.UUID) ([]*pb.Section, error) {
	resp, err := c.c.ListSections(ctx, &pb.ListSectionsRequest{EventId: eventID.String()})
	return resp.GetSections(), err
}

func (c *Client) SectionSeats(ctx context.Context, sectionID uuid.UUID) ([]*pb.Seat, error) {
	resp, err := c.c.ListSectionSeats(ctx, &pb.ListSectionSeatsRequest{SectionId: sectionID.String()})
	return resp.GetSeats(), err
}

func (c *Client) SeatIDsForEvent(ctx context.Context, eventID uuid.UUID) ([]uuid.UUID, error) {
	resp, err := c.c.ListEventSeatIds(ctx, &pb.ListEventSeatIdsRequest{EventId: eventID.String()})
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(resp.GetSeatIds()))
	for _, s := range resp.GetSeatIds() {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

func (c *Client) CreateEvent(ctx context.Context, venueID uuid.UUID, title string, startsAt, onSaleAt time.Time) (*pb.Event, error) {
	resp, err := c.c.CreateEvent(ctx, &pb.CreateEventRequest{
		VenueId:  venueID.String(),
		Title:    title,
		StartsAt: timestamppb.New(startsAt),
		OnSaleAt: timestamppb.New(onSaleAt),
	})
	return resp.GetEvent(), err
}

func (c *Client) SetPrice(ctx context.Context, eventID, sectionID uuid.UUID, priceMinor int64) error {
	_, err := c.c.SetPrice(ctx, &pb.SetPriceRequest{
		EventId: eventID.String(), SectionId: sectionID.String(), PriceMinor: priceMinor,
	})
	return err
}

func (c *Client) CountEventsStartingOn(ctx context.Context, day time.Time) (int, error) {
	resp, err := c.c.CountEventsStartingOn(ctx, &pb.CountEventsStartingOnRequest{Day: timestamppb.New(day)})
	return int(resp.GetCount()), err
}

func (c *Client) FindVenueByName(ctx context.Context, name string) (uuid.UUID, error) {
	resp, err := c.c.FindVenueByName(ctx, &pb.FindVenueByNameRequest{Name: name})
	if status.Code(err) == codes.NotFound {
		return uuid.Nil, ErrNoVenue
	}
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(resp.GetVenueId())
}

func (c *Client) FirstSectionID(ctx context.Context, venueID uuid.UUID) (uuid.UUID, error) {
	resp, err := c.c.FirstSection(ctx, &pb.FirstSectionRequest{VenueId: venueID.String()})
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(resp.GetSectionId())
}

func (c *Client) CreateVenue(ctx context.Context, name, kind string) (uuid.UUID, error) {
	resp, err := c.c.CreateVenue(ctx, &pb.CreateVenueRequest{Name: name, Kind: kind})
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(resp.GetVenueId())
}

func (c *Client) AddSection(ctx context.Context, venueID uuid.UUID, name string, rows, seatsPerRow int) (uuid.UUID, error) {
	resp, err := c.c.AddSection(ctx, &pb.AddSectionRequest{
		VenueId: venueID.String(), Name: name,
		Rows: int32(rows), SeatsPerRow: int32(seatsPerRow),
	})
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(resp.GetSectionId())
}
