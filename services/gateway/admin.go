package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/slash3b/tickets/services/catalog"
)

// Operator endpoints: create a big sale on demand.
//
// THE DAILY MOVIE IS UNTOUCHED BY THIS. The seeder CronJob still creates exactly
// one cinema showing at 03:00 and knows nothing about any of this. What was
// missing is the other half: staging an arena on-sale needed a Kubernetes Job,
// which is fine for an operator with a terminal and useless from a browser.
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
	// arena (20,000 seats) or cinema (96). Anything else is rejected rather than
	// guessed at.
	Venue string `json:"venue"`
}

type createShowingResponse struct {
	EventID  string    `json:"event_id"`
	Title    string    `json:"title"`
	Venue    string    `json:"venue"`
	OnSaleAt time.Time `json:"on_sale_at"`
	Seats    int       `json:"seats"`
}

const (
	arenaName  = "Homelab Arena"
	cinemaName = "Cineplex Screen 1"
)

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

	venueName, rows, perRow, sections, price := arenaName, 50, 40, 10, int64(9500)
	switch req.Venue {
	case "", "arena":
	case "cinema":
		venueName, rows, perRow, sections, price = cinemaName, 8, 12, 1, 1200
	default:
		fail(w, http.StatusBadRequest, "venue must be arena or cinema")
		return
	}

	ctx := r.Context()
	venueID, err := a.venue(ctx, venueName, rows, perRow, sections)
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
	seats := 0
	for _, s := range secs {
		id, err := uuid.Parse(s.GetId())
		if err != nil {
			continue
		}
		if err := a.catalog.SetPrice(ctx, eventID, id, price); err != nil {
			a.lg.Warn("could not price a section", zap.Error(err))
		}
		seats += int(s.GetSeats())
	}

	a.lg.Warn("showing created from the operator page",
		zap.String("event_id", event.GetId()), zap.String("title", req.Title),
		zap.Time("on_sale_at", onSaleAt), zap.Int("seats", seats))

	writeJSON(w, http.StatusCreated, createShowingResponse{
		EventID: event.GetId(), Title: req.Title, Venue: venueName,
		OnSaleAt: onSaleAt, Seats: seats,
	})
}

// venue finds the venue, building it the first time. Same shape as the seeder's,
// and for the same reason: ErrNoVenue must be distinguishable from "catalog is
// down", or a bad minute builds a second arena.
func (a *Admin) venue(ctx context.Context, name string, rows, perRow, sections int) (uuid.UUID, error) {
	venueID, err := a.catalog.FindVenueByName(ctx, name)
	switch {
	case errors.Is(err, catalog.ErrNoVenue):
		kind := "arena"
		if name == cinemaName {
			kind = "cinema"
		}
		if venueID, err = a.catalog.CreateVenue(ctx, name, kind); err != nil {
			return uuid.Nil, fmt.Errorf("create venue: %w", err)
		}
		for i := 1; i <= sections; i++ {
			label := fmt.Sprintf("Block %d", i)
			if kind == "cinema" {
				label = "Stalls"
			}
			if _, err := a.catalog.AddSection(ctx, venueID, label, rows, perRow); err != nil {
				return uuid.Nil, fmt.Errorf("add %s: %w", label, err)
			}
		}
		return venueID, nil
	case err != nil:
		return uuid.Nil, err
	}
	return venueID, nil
}
