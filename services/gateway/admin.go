package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/slash3b/tickets/services/catalog"
)

// Operator endpoints: create a showing on demand.
//
// THIS IS THE ONLY WAY A SHOWING APPEARS NOW. The 03:00 seeder CronJob is
// suspended — see deploy/apps/tickets/seeder.yaml. Nothing creates events on its
// own any more, because an operator who wants to watch a sale wants it to start
// while they are looking at it, not at three in the morning.
//
// The gateway is the right place for it because it already talks to catalog, and
// because creating a showing is exactly the thing catalog exists to do. The
// alternative — teaching the frontend to create Kubernetes Jobs — would be a far
// worse idea than it first sounds.
//
// SEATS ARE NOT OPENED HERE, deliberately. The event is created with an on_sale_at
// in the near future and the workers on-sale loop opens it when that moment
// arrives, exactly as it does for a showing seeded by the CronJob. There is one
// path that starts a sale, and this is not a second one.

type Admin struct {
	catalog *catalog.Client
	lg      *zap.Logger
}

func NewAdmin(c *catalog.Client, lg *zap.Logger) *Admin {
	return &Admin{catalog: c, lg: lg}
}

type createShowingRequest struct {
	Title string `json:"title"`
	// Seconds from now until it goes on sale. Small on purpose: this exists so a
	// sale can be staged and watched, not scheduled for next week.
	OnSaleInSeconds int `json:"on_sale_in_seconds"`
	// arena, cinema or custom. Anything else is rejected rather than guessed at.
	Venue string `json:"venue"`

	// THE CUSTOM LAYOUT. Read only when Venue is "custom", and a zero field takes
	// the arena preset's value, so one dimension can be changed without restating
	// the others.
	VenueName   string `json:"venue_name"`
	Sections    int    `json:"sections"`
	Rows        int    `json:"rows"`
	SeatsPerRow int    `json:"seats_per_row"`
}

type createShowingResponse struct {
	EventID  string    `json:"event_id"`
	Title    string    `json:"title"`
	Venue    string    `json:"venue"`
	OnSaleAt time.Time `json:"on_sale_at"`
	Seats    int       `json:"seats"`
	// True when the seating chart already existed and was reused. It matters:
	// asking for a custom layout under a name that is already taken gets you the
	// EXISTING chart, and silently building a show on the wrong size of room is a
	// confusing hour.
	VenueReused bool `json:"venue_reused"`
}

// layout is a seating chart: a name, and the shape of the room behind it.
type layout struct {
	name     string
	kind     string // arena or cinema — decides pricing tiers and section labels
	sections int
	rows     int
	perRow   int
}

func (l layout) seats() int { return l.sections * l.rows * l.perRow }

var (
	arenaLayout  = layout{name: "Homelab Arena", kind: "arena", sections: 10, rows: 50, perRow: 40}
	cinemaLayout = layout{name: "Cineplex Screen 1", kind: "cinema", sections: 1, rows: 8, perRow: 12}
)

// CAPS EXIST BECAUSE THIS ENDPOINT HAS NO AUTH. A typo of one extra digit in
// `rows` is a request to build a million seat rows, and inventory would honestly
// try. The arena is 20,000; 60,000 is comfortably more than anything worth
// rehearsing here and still an amount the seat-open loop finishes in seconds.
const (
	maxSections = 40
	maxRows     = 500
	maxPerRow   = 100
	maxSeats    = 60000
)

// resolve turns a request into the chart it is asking for.
//
// A CUSTOM LAYOUT WITH NO NAME GETS ONE THAT ENCODES ITS SHAPE. Venues are found
// by name and built only when missing, so reusing a fixed name like "Custom"
// across two different shapes would hand back the first chart forever and quietly
// ignore the numbers the operator just typed.
func resolve(req createShowingRequest) (layout, error) {
	switch req.Venue {
	case "", "arena":
		return arenaLayout, nil
	case "cinema":
		return cinemaLayout, nil
	case "custom":
	default:
		return layout{}, fmt.Errorf("venue must be arena, cinema or custom")
	}

	l := arenaLayout
	if req.Sections > 0 {
		l.sections = req.Sections
	}
	if req.Rows > 0 {
		l.rows = req.Rows
	}
	if req.SeatsPerRow > 0 {
		l.perRow = req.SeatsPerRow
	}

	// ONE SECTION IS A SCREEN, SEVERAL ARE AN ARENA. That is the only thing kind
	// decides — whether the block is labelled "Stalls" and priced as one room, or
	// labelled "Block N" and priced in tiers from the floor back.
	if l.sections == 1 {
		l.kind = "cinema"
	}

	switch {
	case l.sections > maxSections:
		return layout{}, fmt.Errorf("sections must be 1..%d", maxSections)
	case l.rows > maxRows:
		return layout{}, fmt.Errorf("rows must be 1..%d", maxRows)
	case l.perRow > maxPerRow:
		return layout{}, fmt.Errorf("seats_per_row must be 1..%d", maxPerRow)
	case l.seats() > maxSeats:
		return layout{}, fmt.Errorf("that is %d seats; the limit is %d", l.seats(), maxSeats)
	}

	l.name = strings.TrimSpace(req.VenueName)
	if l.name == "" {
		l.name = fmt.Sprintf("Custom %dx%dx%d", l.sections, l.rows, l.perRow)
	}
	if len(l.name) > 80 {
		return layout{}, fmt.Errorf("venue_name is too long")
	}
	return l, nil
}

func (a *Admin) createShowing(w http.ResponseWriter, r *http.Request) {
	var req createShowingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "malformed body")
		return
	}
	if req.OnSaleInSeconds <= 0 {
		req.OnSaleInSeconds = 120
	}
	if req.Title == "" {
		req.Title = fmt.Sprintf("On-Sale %s", time.Now().Format("15:04:05"))
	}

	l, err := resolve(req)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	venueID, reused, err := a.venue(ctx, l)
	if err != nil {
		a.lg.Error("could not prepare venue", zap.Error(err))
		fail(w, http.StatusInternalServerError, "could not prepare the venue")
		return
	}

	startsAt := time.Now().AddDate(0, 0, 30).Truncate(time.Hour)
	onSaleAt := time.Now().Add(time.Duration(req.OnSaleInSeconds) * time.Second).Truncate(time.Second)

	event, err := a.catalog.CreateEvent(ctx, venueID, req.Title, startsAt, onSaleAt)
	if err != nil {
		a.lg.Error("could not create showing", zap.Error(err))
		fail(w, http.StatusInternalServerError, "could not create the showing")
		return
	}
	eventID, err := uuid.Parse(event.GetId())
	if err != nil {
		fail(w, http.StatusInternalServerError, "catalog returned a malformed id")
		return
	}

	secs, err := a.catalog.Sections(ctx, eventID)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not read sections")
		return
	}
	// TIERED BY POSITION. Sections come back in display order, so the first third
	// is the floor and the last third the gods. A flat price made every block
	// equally attractive, which spreads a rush evenly — and a real on-sale is not
	// even, it is a fight over the front.
	seats := 0
	for i, s := range secs {
		id, err := uuid.Parse(s.GetId())
		if err != nil {
			continue
		}
		tier := catalog.TierFor(i, len(secs), l.kind == "cinema")
		if err := a.catalog.SetPrice(ctx, eventID, id, tier.PriceMinor); err != nil {
			a.lg.Warn("could not price a section", zap.Error(err))
		}
		seats += int(s.GetSeats())
	}

	a.lg.Warn("showing created from the operator page",
		zap.String("event_id", event.GetId()), zap.String("title", req.Title),
		zap.String("venue", l.name), zap.Bool("venue_reused", reused),
		zap.Time("on_sale_at", onSaleAt), zap.Int("seats", seats))

	writeJSON(w, http.StatusCreated, createShowingResponse{
		EventID: event.GetId(), Title: req.Title, Venue: l.name,
		OnSaleAt: onSaleAt, Seats: seats, VenueReused: reused,
	})
}

// venue finds the seating chart, building it the first time. The bool reports
// whether it already existed.
//
// ErrNoVenue must be distinguishable from "catalog is down", or a bad minute
// builds a second arena.
func (a *Admin) venue(ctx context.Context, l layout) (uuid.UUID, bool, error) {
	venueID, err := a.catalog.FindVenueByName(ctx, l.name)
	switch {
	case errors.Is(err, catalog.ErrNoVenue):
		if venueID, err = a.catalog.CreateVenue(ctx, l.name, l.kind); err != nil {
			return uuid.Nil, false, fmt.Errorf("create venue: %w", err)
		}
		for i := 1; i <= l.sections; i++ {
			label := fmt.Sprintf("Block %d", i)
			if l.kind == "cinema" {
				label = "Stalls"
			}
			if _, err := a.catalog.AddSection(ctx, venueID, label, l.rows, l.perRow); err != nil {
				return uuid.Nil, false, fmt.Errorf("add %s: %w", label, err)
			}
		}
		return venueID, false, nil
	case err != nil:
		return uuid.Nil, false, err
	}
	return venueID, true, nil
}
