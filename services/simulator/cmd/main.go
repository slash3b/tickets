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
