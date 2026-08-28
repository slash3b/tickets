// Package inventory is the INVENTORY service's HTTP surface: what is AVAILABLE.
//
// It is the only writer of seat status anywhere in the system, and after the
// split that is enforced by topology rather than by discipline — nothing else has
// credentials for the inventory schema.
//
// THE ERROR CODES BELOW ARE PART OF THE CONTRACT. In one process the gateway told
// "someone beat you to it" from "the server is broken" with errors.Is. Across a
// network that distinction has to be carried explicitly, and getting it wrong
// turns every lost race — the most common non-200 in this system — into a 500.
package inventory

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/slash3b/tickets/pkg/obs"
	"github.com/slash3b/tickets/pkg/rpc"
	"github.com/slash3b/tickets/services/inventory/store"

	"go.uber.org/zap"
)

// Wire codes for the two sentinels that callers switch on.
const (
	CodeSeatsUnavailable = "seats_unavailable"
	CodeHoldReleased     = "hold_released"
)

type API struct {
	store *store.Store
	lg    *zap.Logger
}

func New(s *store.Store, lg *zap.Logger) *API { return &API{store: s, lg: lg} }

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	obs.Route(mux, a.lg, "POST /holds", a.hold)
	obs.Route(mux, a.lg, "DELETE /holds/{id}", a.release)
	obs.Route(mux, a.lg, "POST /holds/{id}/convert", a.convert)
	obs.Route(mux, a.lg, "POST /holds/{id}/commit", a.commit)
	obs.Route(mux, a.lg, "POST /events/{id}/open", a.open)
	obs.Route(mux, a.lg, "POST /events/{id}/seat-status", a.seatStatus)

	// Driven by workers on a ticker. Under /internal because it is not part of
	// any request path and must not be reachable through the public gateway.
	obs.Route(mux, a.lg, "POST /internal/sweep", a.sweep)
	return mux
}

type holdRequest struct {
	EventID uuid.UUID     `json:"event_id"`
	SeatIDs []uuid.UUID   `json:"seat_ids"`
	TTL     time.Duration `json:"ttl_ns"`
}

type holdResponse struct {
	HoldID uuid.UUID `json:"hold_id"`
}

func (a *API) hold(w http.ResponseWriter, r *http.Request) {
	var req holdRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.SeatIDs) == 0 || req.TTL <= 0 {
		rpc.Fail(w, http.StatusBadRequest, "bad_request", "event_id, seat_ids and ttl_ns are required")
		return
	}
	id, err := a.store.Hold(r.Context(), req.EventID, req.SeatIDs, req.TTL)
	switch {
	case errors.Is(err, store.ErrSeatsUnavailable):
		// 409 with a code, not 500. This is the normal outcome on a contended
		// seat and the caller must be able to say so to a human.
		rpc.Fail(w, http.StatusConflict, CodeSeatsUnavailable, "one or more seats are not available")
	case err != nil:
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not hold seats")
	default:
		rpc.OK(w, http.StatusCreated, holdResponse{HoldID: id})
	}
}

type releaseRequest struct {
	Reason string `json:"reason"`
}

func (a *API) release(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req releaseRequest
	// A release with no body is fine; the reason is for the audit trail.
	_ = decodeOptional(r, &req)
	if req.Reason == "" {
		req.Reason = "released"
	}
	if err := a.store.Release(r.Context(), id, req.Reason); err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not release hold")
		return
	}
	rpc.OK(w, http.StatusNoContent, nil)
}

func (a *API) convert(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := a.store.Convert(r.Context(), id); err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not convert hold")
		return
	}
	rpc.OK(w, http.StatusNoContent, nil)
}

func (a *API) commit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	err := a.store.Commit(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrHoldReleased):
		// The saga has to tell this apart from a transient failure: the seats are
		// gone for good, so retrying forever would never succeed.
		rpc.Fail(w, http.StatusConflict, CodeHoldReleased, "hold was already released; seats are gone")
	case err != nil:
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not commit hold")
	default:
		rpc.OK(w, http.StatusNoContent, nil)
	}
}

type openRequest struct {
	SeatIDs []uuid.UUID `json:"seat_ids"`
}

func (a *API) open(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req openRequest
	if !decode(w, r, &req) {
		return
	}
	n, err := a.store.OpenEvent(r.Context(), id, req.SeatIDs)
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not open event")
		return
	}
	rpc.OK(w, http.StatusOK, map[string]any{"opened": n})
}

type seatStatusRequest struct {
	SeatIDs []uuid.UUID `json:"seat_ids"`
}

// seatStatus is a POST because the seat id list is the request. A section can
// hold hundreds of ids and they do not belong in a URL.
func (a *API) seatStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req seatStatusRequest
	if !decode(w, r, &req) {
		return
	}
	statuses, err := a.store.SeatStatuses(r.Context(), id, req.SeatIDs)
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not read seat statuses")
		return
	}
	out := make(map[string]string, len(statuses))
	for id, st := range statuses {
		out[id.String()] = st
	}
	rpc.OK(w, http.StatusOK, map[string]any{"statuses": out})
}

func (a *API) sweep(w http.ResponseWriter, r *http.Request) {
	expired, err := a.store.SweepExpired(r.Context())
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "sweep expired failed")
		return
	}
	hard, err := a.store.SweepHardDeadline(r.Context())
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "sweep hard deadline failed")
		return
	}
	if len(hard) > 0 {
		// A hold reaching its hard deadline was stuck in `converting`, which means
		// a payment outcome was never established. That is a bug signal, not
		// routine cleanup, and it gets said out loud.
		a.lg.Warn("holds hit the hard deadline", zap.Int("count", len(hard)))
	}
	rpc.OK(w, http.StatusOK, map[string]any{"expired": expired, "hard_deadline": len(hard)})
}
