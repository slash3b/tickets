// Package payments is the PAYMENTS service's HTTP surface: whether money moved.
//
// The charge sequence used to live in the gateway's main.go, wiring the payment
// store and the bank together on the saga's behalf. It belongs here — it is the
// whole of what this service is, and having it in the caller meant every future
// caller would have had to repeat it identically or be subtly wrong.
//
// THE THREE OUTCOMES ARE NOT TWO. succeeded and failed are the easy ones. unknown
// is a first-class state: the bank took the request and never answered, so money
// may or may not have moved. It is emphatically NOT failed, and an unknown
// payment must not release the seats — that is exactly how a paying customer
// loses them to someone else.
package payments

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/slash3b/tickets/pkg/obs"
	"github.com/slash3b/tickets/pkg/rpc"
	"github.com/slash3b/tickets/services/payments/bankclient"
	"github.com/slash3b/tickets/services/payments/store"

	"go.uber.org/zap"
)

type API struct {
	store      *store.Store
	bank       *bankclient.Client
	reconciler *store.Reconciler
	lg         *zap.Logger
}

func New(s *store.Store, bank *bankclient.Client, rec *store.Reconciler, lg *zap.Logger) *API {
	return &API{store: s, bank: bank, reconciler: rec, lg: lg}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	obs.Route(mux, a.lg, "POST /charges", a.charge)
	obs.Route(mux, a.lg, "GET /charges/{orderID}", a.get)
	obs.Route(mux, a.lg, "POST /internal/reconcile", a.reconcile)
	return mux
}

type chargeRequest struct {
	OrderID     uuid.UUID `json:"order_id"`
	AmountMinor int64     `json:"amount_minor"`
}

type chargeResponse struct {
	Outcome     string `json:"outcome"` // succeeded | failed | unknown
	DeclineCode string `json:"decline_code,omitempty"`
}

func (a *API) charge(w http.ResponseWriter, r *http.Request) {
	var req chargeRequest
	if !decode(w, r, &req) {
		return
	}
	if req.AmountMinor <= 0 {
		rpc.Fail(w, http.StatusBadRequest, "bad_request", "order_id and a positive amount_minor are required")
		return
	}

	outcome, declineCode, err := a.Charge(r.Context(), req.OrderID, req.AmountMinor)
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "could not record the charge")
		return
	}
	// NOTE the status is 200 for all three outcomes. A declined card is not an
	// HTTP error — the call worked, the answer was no. Encoding a business
	// outcome as a transport failure is how retry logic ends up retrying
	// decisions that were made correctly the first time.
	rpc.OK(w, http.StatusOK, chargeResponse{Outcome: outcome, DeclineCode: declineCode})
}

// Charge runs the whole sequence: record the intent, ask the bank, record what
// happened. Exported because it is the service, not a handler detail.
func (a *API) Charge(ctx context.Context, orderID uuid.UUID, amountMinor int64) (outcome, declineCode string, err error) {
	pay, err := a.store.Create(ctx, orderID, amountMinor)
	if err != nil {
		return "", "", err
	}

	charge, err := a.bank.AuthorizeAndReconcile(ctx, pay.IdempotencyKey, amountMinor)
	switch {
	case err == nil:
		return "succeeded", "", a.store.Resolve(ctx, orderID, store.StateSucceeded, charge.ID, "")
	case charge != nil && charge.Status == "declined":
		return "failed", charge.DeclineCode,
			a.store.Resolve(ctx, orderID, store.StateFailed, "", charge.DeclineCode)
	default:
		// UNKNOWN. The reconciler establishes the truth later.
		return "unknown", "", a.store.MarkUnknown(ctx, orderID)
	}
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "orderID")
	if !ok {
		return
	}
	p, err := a.store.Get(r.Context(), id)
	if err != nil || p == nil {
		rpc.Fail(w, http.StatusNotFound, "not_found", "no payment for that order")
		return
	}
	rpc.OK(w, http.StatusOK, map[string]any{
		"order_id": p.OrderID, "state": string(p.State), "amount_minor": p.AmountMinor,
	})
}

func (a *API) reconcile(w http.ResponseWriter, r *http.Request) {
	if a.reconciler == nil {
		rpc.Fail(w, http.StatusServiceUnavailable, "internal", errNoReconciler.Error())
		return
	}
	n, err := a.reconciler.Once(r.Context())
	if err != nil {
		rpc.Fail(w, http.StatusInternalServerError, "internal", "reconcile failed")
		return
	}
	rpc.OK(w, http.StatusOK, map[string]any{"resolved": n})
}

var errNoReconciler = errors.New("reconciler not configured")
