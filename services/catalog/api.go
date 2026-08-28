// Package catalog is the CATALOG service's HTTP surface: what EXISTS.
//
// It is the easy one, deliberately — read-mostly, no contention, no state
// machine. The only writers are the seeder (a showing a day) and one-off venue
// setup. Everything else reads.
//
// It does NOT know what is available. That is inventory's, and the split is the
// point: a seat's identity and a seat's availability change at completely
// different rates and for completely different reasons.
package catalog

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/slash3b/tickets/pkg/obs"
	"github.com/slash3b/tickets/pkg/rpc"
	"github.com/slash3b/tickets/services/catalog/store"

	"go.uber.org/zap"
)

type API struct {
	store *store.Store
	lg    *zap.Logger
}

func New(s *store.Store, lg *zap.Logger) *API { return &API{store: s, lg: lg} }

// Wire shapes. Separate from the store's types on purpose: the store's field
// names are Go's business, and a rename there must not silently change a payload
// every other service parses.
type Event struct {
	ID       uuid.UUID `json:"id"`
	VenueID  uuid.UUID `json:"venue_id"`
	Title    string    `json:"title"`
	Venue    string    `json:"venue"`
	StartsAt time.Time `json:"starts_at"`
	OnSaleAt time.Time `json:"on_sale_at"`
}

type Section struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Seats int       `json:"seats"`
}

type Seat struct {
	ID     uuid.UUID `json:"id"`
	Row    string    `json:"row"`
	Number int       `json:"number"`
	X      float64   `json:"x"`
	Y      float64   `json:"y"`
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	obs.Route(mux, a.lg, "GET /events", a.listOnSale)
	obs.Route(mux, a.lg, "GET /events/{id}", a.getEvent)
	obs.Route(mux, a.lg, "GET /events/{id}/sections", a.sections)
	obs.Route(mux, a.lg, "GET /events/{id}/seat-ids", a.seatIDs)
	obs.Route(mux, a.lg, "GET /sections/{id}/seats", a.sectionSeats)

	// Writes. Only the seeder and one-off setup use these.
	obs.Route(mux, a.lg, "POST /events", a.createEvent)
	obs.Route(mux, a.lg, "POST /events/{id}/prices", a.setPrice)
	obs.Route(mux, a.lg, "GET /events/count", a.countOn)
	obs.Route(mux, a.lg, "GET /venues/by-name", a.venueByName)
	obs.Route(mux, a.lg, "GET /venues/{id}/first-section", a.firstSection)
	obs.Route(mux, a.lg, "POST /venues", a.createVenue)
	obs.Route(mux, a.lg, "POST /venues/{id}/sections", a.addSection)
	return mux
}

func (a *API) listOnSale(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	rows, err := a.store.ListOnSale(r.Context(), limit)
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not list events")
		return
	}
	out := make([]Event, len(rows))
	for i, e := range rows {
		out[i] = wireEvent(e)
	}
	rpc.OK(w, http.StatusOK, map[string]any{"events": out})
}

func (a *API) getEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	e, err := a.store.GetEvent(r.Context(), id)
	if err != nil {
		rpc.Fail(w, http.StatusNotFound, "not_found", "no such event")
		return
	}
	rpc.OK(w, http.StatusOK, wireEvent(e))
}

func (a *API) sections(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	rows, err := a.store.Sections(r.Context(), id)
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not list sections")
		return
	}
	out := make([]Section, len(rows))
	for i, s := range rows {
		out[i] = Section{ID: s.ID, Name: s.Name, Seats: s.Seats}
	}
	rpc.OK(w, http.StatusOK, map[string]any{"sections": out})
}

func (a *API) sectionSeats(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	rows, err := a.store.SectionSeats(r.Context(), id)
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not list seats")
		return
	}
	out := make([]Seat, len(rows))
	for i, s := range rows {
		out[i] = Seat{ID: s.ID, Row: s.RowLabel, Number: s.Number, X: s.X, Y: s.Y}
	}
	rpc.OK(w, http.StatusOK, map[string]any{"seats": out})
}

func (a *API) seatIDs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	ids, err := a.store.SeatIDsForEvent(r.Context(), id)
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not list seat ids")
		return
	}
	rpc.OK(w, http.StatusOK, map[string]any{"seat_ids": ids})
}

type createEventRequest struct {
	VenueID  uuid.UUID `json:"venue_id"`
	Title    string    `json:"title"`
	StartsAt time.Time `json:"starts_at"`
	OnSaleAt time.Time `json:"on_sale_at"`
}

func (a *API) createEvent(w http.ResponseWriter, r *http.Request) {
	var req createEventRequest
	if !decode(w, r, &req) {
		return
	}
	e, err := a.store.CreateEvent(r.Context(), req.VenueID, req.Title, req.StartsAt, req.OnSaleAt)
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not create event")
		return
	}
	rpc.OK(w, http.StatusCreated, wireEvent(e))
}

type setPriceRequest struct {
	SectionID  uuid.UUID `json:"section_id"`
	PriceMinor int64     `json:"price_minor"`
}

func (a *API) setPrice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req setPriceRequest
	if !decode(w, r, &req) {
		return
	}
	if err := a.store.SetPrice(r.Context(), id, req.SectionID, req.PriceMinor); err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not set price")
		return
	}
	rpc.OK(w, http.StatusNoContent, nil)
}

func (a *API) countOn(w http.ResponseWriter, r *http.Request) {
	day, err := time.Parse(time.RFC3339, r.URL.Query().Get("day"))
	if err != nil {
		rpc.Fail(w, http.StatusBadRequest, "bad_request", "day must be RFC3339")
		return
	}
	n, err := a.store.CountEventsStartingOn(r.Context(), day)
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not count events")
		return
	}
	rpc.OK(w, http.StatusOK, map[string]any{"count": n})
}

func (a *API) venueByName(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		rpc.Fail(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	id, err := a.store.FindVenueByName(r.Context(), name)
	if err != nil {
		// The seeder depends on telling "no such venue" from "the database is
		// down": one means the catalog was never bootstrapped, the other means
		// try again later.
		rpc.Fail(w, http.StatusNotFound, "not_found", "no such venue")
		return
	}
	rpc.OK(w, http.StatusOK, map[string]any{"venue_id": id})
}

func (a *API) firstSection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	sec, err := a.store.FirstSectionID(r.Context(), id)
	if err != nil {
		rpc.Fail(w, http.StatusNotFound, "not_found", "venue has no sections")
		return
	}
	rpc.OK(w, http.StatusOK, map[string]any{"section_id": sec})
}

type createVenueRequest struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func (a *API) createVenue(w http.ResponseWriter, r *http.Request) {
	var req createVenueRequest
	if !decode(w, r, &req) {
		return
	}
	v, err := a.store.CreateVenue(r.Context(), req.Name, req.Kind)
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not create venue")
		return
	}
	rpc.OK(w, http.StatusCreated, map[string]any{"venue_id": v.ID})
}

type addSectionRequest struct {
	Name        string `json:"name"`
	Rows        int    `json:"rows"`
	SeatsPerRow int    `json:"seats_per_row"`
}

func (a *API) addSection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req addSectionRequest
	if !decode(w, r, &req) {
		return
	}
	sec, err := a.store.AddSection(r.Context(), id, req.Name, req.Rows, req.SeatsPerRow)
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not add section")
		return
	}
	rpc.OK(w, http.StatusCreated, map[string]any{"section_id": sec})
}

func wireEvent(e *store.Event) Event {
	return Event{
		ID: e.ID, VenueID: e.VenueID, Title: e.Title, Venue: e.VenueName,
		StartsAt: e.StartsAt, OnSaleAt: e.OnSaleAt,
	}
}

var errBadUUID = errors.New("malformed id")

func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		rpc.Fail(w, http.StatusBadRequest, "bad_request", errBadUUID.Error())
		return uuid.Nil, false
	}
	return id, true
}
