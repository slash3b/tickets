// Package orders is the ORDERS service's HTTP surface: the saga.
//
// It is the only service that calls two others. Placing an order means converting
// a hold in inventory, charging in payments, and committing the seats back in
// inventory — three network calls that can each fail independently, which is
// precisely the thing this project exists to get right.
package orders

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/slash3b/tickets/pkg/obs"
	"github.com/slash3b/tickets/pkg/rpc"
	"github.com/slash3b/tickets/services/orders/store"

	"go.uber.org/zap"
)

type API struct {
	store   *store.Store
	saga    *store.Saga
	resumer *store.Resumer
	lg      *zap.Logger
}

func New(s *store.Store, saga *store.Saga, res *store.Resumer, lg *zap.Logger) *API {
	return &API{store: s, saga: saga, resumer: res, lg: lg}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	obs.Route(mux, a.lg, "POST /orders", a.place)
	obs.Route(mux, a.lg, "GET /orders/{id}", a.get)
	obs.Route(mux, a.lg, "POST /internal/resume", a.resume)
	return mux
}

type placeRequest struct {
	HoldID      uuid.UUID `json:"hold_id"`
	EventID     uuid.UUID `json:"event_id"`
	UserID      uuid.UUID `json:"user_id"`
	AmountMinor int64     `json:"amount_minor"`
}

type placeResponse struct {
	OrderID uuid.UUID `json:"order_id"`
	State   string    `json:"state"`
}

func (a *API) place(w http.ResponseWriter, r *http.Request) {
	var req placeRequest
	if !decode(w, r, &req) {
		return
	}
	if req.AmountMinor <= 0 {
		rpc.Fail(w, http.StatusBadRequest, "bad_request", "hold_id, event_id, user_id and amount_minor are required")
		return
	}

	o, err := a.store.Create(r.Context(), req.HoldID, req.EventID, req.UserID, req.AmountMinor)
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not create order")
		return
	}

	// THE SAGA MAY LEGITIMATELY NOT FINISH, and that is not an error to the
	// caller. An unknown payment leaves the order mid-flight for the resumer, and
	// the honest answer to the browser is the state it actually reached — not a
	// 500, which would suggest retrying something that is already in progress.
	if err := a.saga.Run(r.Context(), o.ID); err != nil {
		a.lg.Warn("saga did not complete in the request",
			zap.String("order_id", o.ID.String()), zap.Error(err))
	}

	state, err := a.state(r.Context(), o.ID)
	if err != nil {
		state = "unknown"
	}
	rpc.OK(w, http.StatusCreated, placeResponse{OrderID: o.ID, State: state})
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	state, err := a.state(r.Context(), id)
	if err != nil {
		rpc.Fail(w, http.StatusNotFound, "not_found", "no such order")
		return
	}
	rpc.OK(w, http.StatusOK, placeResponse{OrderID: id, State: state})
}

func (a *API) state(ctx context.Context, id uuid.UUID) (string, error) {
	o, err := a.store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if o == nil {
		return "", errNoOrder
	}
	return string(o.State), nil
}

var errNoOrder = errors.New("no such order")

func (a *API) resume(w http.ResponseWriter, r *http.Request) {
	if a.resumer == nil {
		rpc.Fail(w, http.StatusServiceUnavailable, "internal", errNoResumer.Error())
		return
	}
	n, err := a.resumer.Once(r.Context())
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "resume failed")
		return
	}
	rpc.OK(w, http.StatusOK, map[string]any{"resumed": n})
}

var errNoResumer = errors.New("resumer not configured")
