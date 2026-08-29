// The load. Virtual buyers driving the public API exactly as a browser would.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/slash3b/tickets/pkg/env"
	"github.com/slash3b/tickets/pkg/health"
	"github.com/slash3b/tickets/pkg/logger"
	"github.com/slash3b/tickets/pkg/obs"
	"github.com/slash3b/tickets/services/simulator"

	"go.uber.org/zap"
)

const (
	service = "simulator"
	version = "0.1.0"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		port    = env.Get("PORT", "8080")
		debug   = env.Get("DEBUG", "false") == "true"
		otlp    = env.Get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		gateway = env.Get("GATEWAY_URL", "http://gateway.tickets.svc.cluster.local")
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownObs, logProvider, err := obs.Setup(ctx, service, version, otlp)
	if err != nil {
		return fmt.Errorf("observability: %w", err)
	}
	lg, flush := logger.MustNew(service, debug, logProvider)
	defer func() { _ = flush() }()

	cfg := simulator.DefaultConfig()
	if v, err := strconv.ParseFloat(env.Get("ARRIVALS_PER_MINUTE", ""), 64); err == nil {
		cfg.ArrivalsPerMinute = v
	}
	sim := simulator.New(gateway, cfg)

	lg.Info("simulator starting — deliberately quiet",
		zap.Float64("arrivals_per_minute", cfg.ArrivalsPerMinute),
		zap.String("gateway", gateway))

	go sim.Run(ctx)

	// Report periodically. THE NUMBER THAT MATTERS is bought, compared against the
	// backend's confirmed orders — those come from independent systems and must
	// agree, and divergence is the first sign of an oversell or a lost order.
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				lg.Info("simulator stats",
					zap.Int64("sessions", sim.Stats.Sessions.Load()),
					zap.Int64("held", sim.Stats.Held.Load()),
					zap.Int64("bought", sim.Stats.Bought.Load()),
					zap.Int64("lost_race_409", sim.Stats.LostRace.Load()),
					zap.Int64("abandoned", sim.Stats.Abandoned.Load()),
					zap.Int64("errors", sim.Stats.Errors.Load()))
			}
		}
	}()

	mux := http.NewServeMux()
	health.New(lg).Register(ctx, mux, 2*time.Second, 15*time.Second)
	// The load dial, reachable at runtime so an on-sale can be triggered
	// deliberately rather than by redeploying.
	mux.HandleFunc("PUT /config", func(w http.ResponseWriter, r *http.Request) {
		var c simulator.Config
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			http.Error(w, "bad config", http.StatusBadRequest)
			return
		}
		sim.SetConfig(c)
		lg.Warn("load changed by request", zap.Float64("arrivals_per_minute", c.ArrivalsPerMinute))
		w.WriteHeader(http.StatusNoContent)
	})
	// THE ON-SALE. Triggered by hand, never by a schedule — this is the one thing
	// in the system that is supposed to hurt, and it should only ever happen
	// because somebody decided to watch it:
	//
	//   curl -X POST https://sim.tickets.lan/onsale \
	//        -d '{"event_id":"...","buyers":2000,"over_seconds":20}'
	//
	// It blocks until the last buyer is done and answers with what happened, so
	// the result is a measurement rather than a thing to go and look up.
	mux.HandleFunc("POST /onsale", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			EventID     string  `json:"event_id"`
			Buyers      int     `json:"buyers"`
			OverSeconds float64 `json:"over_seconds"`
			GroupShare  float64 `json:"group_share"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.EventID == "" {
			http.Error(w, "event_id is required", http.StatusBadRequest)
			return
		}
		if req.Buyers <= 0 {
			req.Buyers = 500
		}
		if req.OverSeconds <= 0 {
			req.OverSeconds = 10
		}
		if req.GroupShare <= 0 {
			req.GroupShare = 0.25
		}

		lg.Warn("ON-SALE STARTING — this is meant to hurt",
			zap.String("event_id", req.EventID),
			zap.Int("buyers", req.Buyers),
			zap.Float64("over_seconds", req.OverSeconds))

		// THE EXPERIMENT MUST OUTLIVE THE REQUEST. The first 2000-buyer run died
		// at exactly 15 seconds — Envoy's default route timeout — and because the
		// burst was running on r.Context(), the client disconnect CANCELLED IT
		// MID-FLIGHT: 365 seats sold instead of the full run, and five holds left
		// dangling for the sweeper. A proxy hanging up must not be able to abort
		// an on-sale rehearsal halfway; that produces a measurement of nothing and
		// leaves the system in a state nobody asked for.
		//
		// WithoutCancel keeps the trace context — so the burst stays one trace —
		// while dropping the cancellation. The deadline below is the experiment's
		// own, generous enough for a long window and finite so a wedged run cannot
		// hold goroutines forever.
		burstCtx, cancel := context.WithTimeout(
			context.WithoutCancel(r.Context()),
			time.Duration(req.OverSeconds*float64(time.Second))+10*time.Minute)
		defer cancel()

		res := sim.Burst(burstCtx, req.EventID, req.Buyers,
			time.Duration(req.OverSeconds*float64(time.Second)), req.GroupShare)

		lg.Warn("ON-SALE DONE",
			zap.Int64("bought", res.Bought), zap.Int64("lost_race", res.LostRace),
			zap.Int64("errors", res.Errors), zap.String("took", res.Took))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})

	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{
			"sessions": sim.Stats.Sessions.Load(),
			"held":     sim.Stats.Held.Load(),
			"bought":   sim.Stats.Bought.Load(),
			"lost_409": sim.Stats.LostRace.Load(),
			"errors":   sim.Stats.Errors.Load(),
		})
	})

	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			lg.Error("http", zap.Error(err))
		}
	}()

	<-ctx.Done()
	drain, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return errors.Join(srv.Shutdown(drain), shutdownObs(drain))
}
