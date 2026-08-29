// Package gateway is the browser-facing BFF and the only service reachable from
// outside the cluster.
//
// It owns no data. It calls catalog for what exists, inventory for what is
// available, and orders to buy — and it is the one place where the seat map is
// assembled from two sources, because a seat's identity (catalog) and a seat's
// availability (inventory) are deliberately owned by different services.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/slash3b/tickets/pkg/cache"
	"github.com/slash3b/tickets/pkg/obs"
)

// Everything gateway needs, declared here by the consumer. Today these are direct
// calls in one binary; behind gRPC later, none of this changes.
type Catalog interface {
	ListOnSale(ctx context.Context, limit int) ([]Event, error)
	ListUpcoming(ctx context.Context, limit int) ([]Event, error)
	GetEvent(ctx context.Context, id uuid.UUID) (*Event, error)
	Sections(ctx context.Context, eventID uuid.UUID) ([]Section, error)
	SectionSeats(ctx context.Context, sectionID uuid.UUID) ([]Seat, error)
}

type Inventory interface {
	Hold(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID, ttl time.Duration) (uuid.UUID, error)
	Release(ctx context.Context, holdID uuid.UUID, reason string) error
	SeatStatuses(ctx context.Context, eventID uuid.UUID, seatIDs []uuid.UUID) (map[uuid.UUID]string, error)
}

type Orders interface {
	Place(ctx context.Context, holdID, eventID, userID uuid.UUID, amountMinor int64) (uuid.UUID, error)
	Status(ctx context.Context, orderID uuid.UUID) (string, error)
}

type Event struct {
	ID       uuid.UUID `json:"id"`
	Title    string    `json:"title"`
	Venue    string    `json:"venue"`
	StartsAt time.Time `json:"starts_at"`
	OnSaleAt time.Time `json:"on_sale_at"`
}

type Section struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Seats int       `json:"seats"`
	// The frontend needs this to show a total before someone commits to buying.
	// It used to hardcode a number that mirrored the seeder, which would have
	// charged a real amount for a guessed one.
	PriceMinor int64 `json:"price_minor"`
}

type Seat struct {
	ID     uuid.UUID `json:"id"`
	Row    string    `json:"row"`
	Number int       `json:"number"`
	X      float64   `json:"x"`
	Y      float64   `json:"y"`
	// Status is filled from inventory, not catalog. It is ALLOWED TO BE STALE by
	// the time the browser renders it — a user clicking a seat the map showed as
	// free and getting a 409 is correct behaviour, not a bug.
	Status string `json:"status,omitempty"`
}

// ErrSeatsGone is returned when a hold loses the race.
var ErrSeatsGone = errors.New("seats unavailable")

type API struct {
	catalog   Catalog
	inventory Inventory
	orders    Orders
	holdTTL   time.Duration
	lg        *zap.Logger
	hub       *hub
	cache     *cache.Cache
}

func New(c Catalog, i Inventory, o Orders, holdTTL time.Duration, lg *zap.Logger) *API {
	return &API{catalog: c, inventory: i, orders: o, holdTTL: holdTTL, lg: lg}
}

// WithCache turns on the Redis seat-map projection. A nil cache is fine and
// means every read goes to inventory, which is what happened before this existed.
func (a *API) WithCache(c *cache.Cache) *API {
	a.cache = c
	return a
}

// WithStreaming turns on the live seat map. Without it the API is exactly what it
// was and browsers keep polling, which is the correct behaviour when there is no
// broker to listen to.
func (a *API) WithStreaming(lg *zap.Logger) *API {
	a.hub = newHub(lg)
	return a
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	// obs.Route, not mux.HandleFunc: each route gets a server span named after the
	// PATTERN, so "GET /api/events/{id}" stays one span name instead of one per
	// event id. See pkg/obs/http.go for why that is done per route.
	obs.Route(mux, a.lg, "GET /api/events", a.listEvents)
	// BEFORE /api/events/{id}, because net/http's mux matches the more specific
	// pattern first — but only if it can tell them apart, and "upcoming" would
	// otherwise be a perfectly good {id} that fails to parse as a uuid.
	obs.Route(mux, a.lg, "GET /api/events/upcoming", a.listUpcoming)
	obs.Route(mux, a.lg, "GET /api/events/{id}", a.getEvent)
	obs.Route(mux, a.lg, "GET /api/events/{id}/sections", a.listSections)
	// NOT wrapped in obs.Route: a stream is open for minutes, and a span that
	// lives as long as the connection tells you nothing except that somebody has
	// a browser tab open. The interesting spans are the changes flowing through
	// it, and those belong to inventory.
	mux.HandleFunc("GET /api/events/{id}/stream", a.stream)
	obs.Route(mux, a.lg, "GET /api/events/{id}/sections/{sid}", a.sectionSeats)
	obs.Route(mux, a.lg, "POST /api/holds", a.createHold)
	obs.Route(mux, a.lg, "DELETE /api/holds/{id}", a.releaseHold)
	obs.Route(mux, a.lg, "POST /api/orders", a.createOrder)
	obs.Route(mux, a.lg, "GET /api/orders/{id}", a.getOrder)
	return mux
}

func (a *API) listEvents(w http.ResponseWriter, r *http.Request) {
	events, err := a.catalog.ListOnSale(r.Context(), 50)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not list events")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// listUpcoming is what lets a concert exist before anybody can buy a seat.
//
// The response deliberately carries on_sale_at and nothing about seats: there
// are no seats yet. Inventory has no rows for this event until its on-sale
// moment, which is exactly why no endpoint here has to check a clock.
func (a *API) listUpcoming(w http.ResponseWriter, r *http.Request) {
	events, err := a.catalog.ListUpcoming(r.Context(), 50)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not list upcoming events")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (a *API) getEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	e, err := a.catalog.GetEvent(r.Context(), id)
	if err != nil || e == nil {
		fail(w, http.StatusNotFound, "no such event")
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (a *API) listSections(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	sections, err := a.catalog.Sections(r.Context(), id)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not list sections")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sections": sections})
}

// sectionSeats is the read model: catalog says which seats exist and where they
// are, inventory says which are free. ONE SECTION AT A TIME, deliberately —
// there is no whole-event endpoint, because at 20,000 seats that is a denial of
// service against your own database.
func (a *API) sectionSeats(w http.ResponseWriter, r *http.Request) {
	eventID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	sectionID, ok := pathUUID(w, r, "sid")
	if !ok {
		return
	}

	seats, err := a.catalog.SectionSeats(r.Context(), sectionID)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not load seats")
		return
	}

	statuses, err := a.seatStatuses(r.Context(), eventID, seats)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not load availability")
		return
	}
	for i := range seats {
		if st, ok := statuses[seats[i].ID.String()]; ok {
			seats[i].Status = st
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"seats": seats})
}

// seatStatuses answers from the Redis projection when it can, and from inventory
// when it cannot.
//
// THE CACHE IS NEVER THE TRUTH. Every miss falls through to inventory, and a
// PARTIAL hit counts as a miss — a seat map missing three seats is not a stale
// seat map, it is a wrong one. That is also what makes flushing Redis harmless:
// everything is absent, so everything misses, and the only symptom is the latency
// this exists to remove.
//
// It reads 2,000 rows from inventory.event_seats otherwise, which is the same
// table every seat claim is contending on — so this is not only a read
// optimisation, it takes browse traffic off the contended writer's rows.
func (a *API) seatStatuses(ctx context.Context, eventID uuid.UUID, seats []Seat) (map[string]string, error) {
	ids := make([]string, len(seats))
	uuids := make([]uuid.UUID, len(seats))
	for i, s := range seats {
		ids[i] = s.ID.String()
		uuids[i] = s.ID
	}

	if statuses, hit := a.cache.Statuses(ctx, eventID.String(), ids); hit {
		return statuses, nil
	}

	fresh, err := a.inventory.SeatStatuses(ctx, eventID, uuids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(fresh))
	for id, st := range fresh {
		out[id.String()] = st
	}

	// Populate for next time. A change that lands between the read above and this
	// write would be overwritten by a value that is a few milliseconds stale — and
	// that is acceptable here for the reason the whole seat map is allowed to be
	// stale: the SSE stream corrects it immediately, and the worst outcome is a
	// 409 on a hold, which is the ordinary path this system is built around.
	a.cache.PutStatuses(ctx, eventID.String(), out)
	return out, nil
}

type holdRequest struct {
	EventID uuid.UUID   `json:"event_id"`
	SeatIDs []uuid.UUID `json:"seat_ids"`
}

func (a *API) createHold(w http.ResponseWriter, r *http.Request) {
	var req holdRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.SeatIDs) == 0 {
		fail(w, http.StatusBadRequest, "event_id and at least one seat_id are required")
		return
	}

	holdID, err := a.inventory.Hold(r.Context(), req.EventID, req.SeatIDs, a.holdTTL)
	if errors.Is(err, ErrSeatsGone) {
		// 409, not 500. Losing a race is a normal outcome, and the SPA must show
		// the user a refreshed map rather than an error page.
		fail(w, http.StatusConflict, "one or more seats were just taken")
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not hold seats")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"hold_id":    holdID,
		"expires_at": time.Now().Add(a.holdTTL),
	})
}

func (a *API) releaseHold(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := a.inventory.Release(r.Context(), id, "user_cancelled"); err != nil {
		fail(w, http.StatusInternalServerError, "could not release hold")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type orderRequest struct {
	HoldID      uuid.UUID `json:"hold_id"`
	EventID     uuid.UUID `json:"event_id"`
	UserID      uuid.UUID `json:"user_id"`
	AmountMinor int64     `json:"amount_minor"`
}

func (a *API) createOrder(w http.ResponseWriter, r *http.Request) {
	var req orderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AmountMinor <= 0 {
		fail(w, http.StatusBadRequest, "hold_id, event_id, user_id and amount_minor are required")
		return
	}

	orderID, err := a.orders.Place(r.Context(), req.HoldID, req.EventID, req.UserID, req.AmountMinor)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not place order")
		return
	}

	status, err := a.orders.Status(r.Context(), orderID)
	if err != nil {
		status = "unknown"
	}
	writeJSON(w, http.StatusCreated, map[string]any{"order_id": orderID, "state": status})
}

func (a *API) getOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	status, err := a.orders.Status(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, "no such order")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order_id": id, "state": status})
}

func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		fail(w, http.StatusBadRequest, "malformed "+name)
		return uuid.Nil, false
	}
	return id, true
}

// fail writes an error the SPA can act on. Errors say what went wrong, never
// apologise, and never leak an internal message to a browser.
func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
