package catalog

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/slash3b/tickets/pkg/rpc"
)

// Client talks to the catalog service.
//
// It returns this package's wire types, not the caller's. Callers adapt — which
// is what keeps a change to the gateway's response shape from reaching in here,
// and vice versa.
type Client struct{ c *rpc.Client }

func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{c: rpc.New(baseURL, timeout)}
}

func (c *Client) ListOnSale(ctx context.Context, limit int) ([]Event, error) {
	var out struct {
		Events []Event `json:"events"`
	}
	err := c.c.Do(ctx, http.MethodGet, fmt.Sprintf("/events?limit=%d", limit), nil, &out)
	return out.Events, err
}

func (c *Client) GetEvent(ctx context.Context, id uuid.UUID) (*Event, error) {
	var e Event
	if err := c.c.Do(ctx, http.MethodGet, "/events/"+id.String(), nil, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

func (c *Client) Sections(ctx context.Context, eventID uuid.UUID) ([]Section, error) {
	var out struct {
		Sections []Section `json:"sections"`
	}
	err := c.c.Do(ctx, http.MethodGet, "/events/"+eventID.String()+"/sections", nil, &out)
	return out.Sections, err
}

func (c *Client) SectionSeats(ctx context.Context, sectionID uuid.UUID) ([]Seat, error) {
	var out struct {
		Seats []Seat `json:"seats"`
	}
	err := c.c.Do(ctx, http.MethodGet, "/sections/"+sectionID.String()+"/seats", nil, &out)
	return out.Seats, err
}

func (c *Client) SeatIDsForEvent(ctx context.Context, eventID uuid.UUID) ([]uuid.UUID, error) {
	var out struct {
		SeatIDs []uuid.UUID `json:"seat_ids"`
	}
	err := c.c.Do(ctx, http.MethodGet, "/events/"+eventID.String()+"/seat-ids", nil, &out)
	return out.SeatIDs, err
}

func (c *Client) CreateEvent(ctx context.Context, venueID uuid.UUID, title string, startsAt, onSaleAt time.Time) (*Event, error) {
	var e Event
	err := c.c.Do(ctx, http.MethodPost, "/events",
		createEventRequest{VenueID: venueID, Title: title, StartsAt: startsAt, OnSaleAt: onSaleAt}, &e)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (c *Client) SetPrice(ctx context.Context, eventID, sectionID uuid.UUID, priceMinor int64) error {
	return c.c.Do(ctx, http.MethodPost, "/events/"+eventID.String()+"/prices",
		setPriceRequest{SectionID: sectionID, PriceMinor: priceMinor}, nil)
}

func (c *Client) CountEventsStartingOn(ctx context.Context, day time.Time) (int, error) {
	var out struct {
		Count int `json:"count"`
	}
	err := c.c.Do(ctx, http.MethodGet,
		"/events/count?day="+url.QueryEscape(day.Format(time.RFC3339)), nil, &out)
	return out.Count, err
}

// ErrNoVenue is returned when the catalog has no venue by that name. The seeder
// has to tell that apart from "the catalog is down": one means nobody
// bootstrapped the cinema, the other means try again in a minute.
var ErrNoVenue = fmt.Errorf("no such venue")

func (c *Client) FindVenueByName(ctx context.Context, name string) (uuid.UUID, error) {
	var out struct {
		VenueID uuid.UUID `json:"venue_id"`
	}
	err := c.c.Do(ctx, http.MethodGet, "/venues/by-name?name="+url.QueryEscape(name), nil, &out)
	if rpc.CodeOf(err) == "not_found" {
		return uuid.Nil, ErrNoVenue
	}
	return out.VenueID, err
}

func (c *Client) CreateVenue(ctx context.Context, name, kind string) (uuid.UUID, error) {
	var out struct {
		VenueID uuid.UUID `json:"venue_id"`
	}
	err := c.c.Do(ctx, http.MethodPost, "/venues", createVenueRequest{Name: name, Kind: kind}, &out)
	return out.VenueID, err
}

func (c *Client) AddSection(ctx context.Context, venueID uuid.UUID, name string, rows, seatsPerRow int) (uuid.UUID, error) {
	var out struct {
		SectionID uuid.UUID `json:"section_id"`
	}
	err := c.c.Do(ctx, http.MethodPost, "/venues/"+venueID.String()+"/sections",
		addSectionRequest{Name: name, Rows: rows, SeatsPerRow: seatsPerRow}, &out)
	return out.SectionID, err
}

func (c *Client) FirstSectionID(ctx context.Context, venueID uuid.UUID) (uuid.UUID, error) {
	var out struct {
		SectionID uuid.UUID `json:"section_id"`
	}
	err := c.c.Do(ctx, http.MethodGet, "/venues/"+venueID.String()+"/first-section", nil, &out)
	return out.SectionID, err
}
