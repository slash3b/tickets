// The fake bank as a running service.
//
// Deliberately the same shape as hello: pkg/obs for OTLP, pkg/logger for
// trace-correlated logs, pkg/health for the probes. Every service in this repo is
// hello with logic added, and this is the first one to prove that claim.
package main

import (
	"context"
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
	"github.com/slash3b/tickets/services/bank"

	"go.uber.org/zap"
)

const (
	service = "bank"
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
		port     = env.Get("PORT", "8080")
		debug    = env.Get("DEBUG", "false") == "true"
		endpoint = env.Get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownObs, logProvider, err := obs.Setup(ctx, service, version, endpoint)
	if err != nil {
		return fmt.Errorf("observability: %w", err)
	}

	lg, flush := logger.MustNew(service, debug, logProvider)
	defer func() { _ = flush() }()

	cfg := bank.DefaultConfig()
	cfg.DeclineRate = envFloat("DECLINE_RATE", cfg.DeclineRate)
	cfg.TimeoutRate = envFloat("TIMEOUT_RATE", cfg.TimeoutRate)

	lg.Info("bank starting — it is supposed to misbehave",
		zap.Float64("decline_rate", cfg.DeclineRate),
		zap.Float64("timeout_rate", cfg.TimeoutRate))

	mux := http.NewServeMux()
	mux.Handle("/", bank.New(cfg).WithLogger(lg).Handler())
	health.New(lg).Register(ctx, mux, 2*time.Second, 15*time.Second)

	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() {
		lg.Info("listening", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		lg.Info("shutting down")
	}

	drain, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return errors.Join(srv.Shutdown(drain), shutdownObs(drain))
}

func envFloat(key string, fallback float64) float64 {
	v, err := strconv.ParseFloat(env.Get(key, ""), 64)
	if err != nil {
		return fallback
	}
	return v
}
