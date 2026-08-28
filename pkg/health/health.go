// Package health serves the two probes Kubernetes needs.
//
// /livez asks: is the app alive? Failing it makes kubelet RESTART the container,
// so it must only fail when a restart would actually help — a deadlock, not a
// missing database.
//
// /readyz asks: is the app able to serve? Failing it removes the pod from Service
// endpoints but leaves it running. This is the one that may fail temporarily:
// during startup, or while a dependency is down.
//
// Getting these two backwards is a classic: a /livez that checks the database
// turns a brief database blip into an endless restart loop across every pod.
package health

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Check reports whether one dependency is usable. Returning an error makes the
// service not-ready; it never makes it not-alive.
type Check func(ctx context.Context) error

// Handler owns readiness state for one service.
type Handler struct {
	ready atomic.Bool // atomic: written by the poll loop, read by every request
	lg    *zap.Logger
}

func New(lg *zap.Logger) *Handler { return &Handler{lg: lg} }

// Register wires both probes onto mux and starts the readiness poll loop. With no
// checks the service reports ready as soon as the loop runs once.
func (h *Handler) Register(ctx context.Context, mux *http.ServeMux, timeout, interval time.Duration, checks ...Check) {
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if h.ready.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	go h.poll(ctx, timeout, interval, checks...)
}

func (h *Handler) poll(ctx context.Context, timeout, interval time.Duration, checks ...Check) {
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		h.ready.Store(h.runChecks(ctx, timeout, checks...))

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (h *Handler) runChecks(ctx context.Context, timeout time.Duration, checks ...Check) bool {
	for _, check := range checks {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		err := check(cctx)
		cancel()

		if err != nil {
			h.lg.Warn("readiness check failed", zap.Error(err))
			return false
		}
	}

	return true
}
