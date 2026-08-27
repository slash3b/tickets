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
)

// Everything gateway needs, declared here by the consumer. Today these are direct
// calls in one binary; behind gRPC later, none of this changes.
type Catalog interface {
	ListOnSale(ctx context.Context, limit int) ([]Event, error)
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
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Venue     string    `json:"venue"`
	StartsAt  time.Time `json:"starts_at"`
	OnSaleAt  time.Time `json:"on_sale_at"`
}

type Section struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Seats int       `json:"seats"`
}

type Seat struct {
	ID       uuid.UUID `json:"id"`
	Row      string    `json:"row"`
	Number   int       `json:"number"`
	X        float64   `json:"x"`
	Y        float64   `json:"y"`
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
}

func New(c Catalog, i Inventory, o Orders, holdTTL time.Duration) *API {
	return &API{catalog: c, inventory: i, orders: o, holdTTL: holdTTL}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/events", a.listEvents)
	mux.HandleFunc("GET /api/events/{id}", a.getEvent)
	mux.HandleFunc("GET /api/events/{id}/sections", a.listSections)
	mux.HandleFunc("GET /api/events/{id}/sections/{sid}", a.sectionSeats)
	mux.HandleFunc("POST /api/holds", a.createHold)
	mux.HandleFunc("DELETE /api/holds/{id}", a.releaseHold)
	mux.HandleFunc("POST /api/orders", a.createOrder)
	mux.HandleFunc("GET /api/orders/{id}", a.getOrder)
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

	ids := make([]uuid.UUID, len(seats))
	for i, s := range seats {
		ids[i] = s.ID
	}
	statuses, err := a.inventory.SeatStatuses(r.Context(), eventID, ids)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not load availability")
		return
	}
	for i := range seats {
		if st, ok := statuses[seats[i].ID]; ok {
			seats[i].Status = st
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"seats": seats})
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
