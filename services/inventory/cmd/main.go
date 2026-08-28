// The inventory service: what is AVAILABLE.
//
// The contended core. Every seat claim in the system happens here, in one
// conditional UPDATE, and this is the only process with credentials for the
// inventory schema — so "inventory is the only writer of seat status" stopped
// being a rule people had to remember and became a fact of the topology.
//
// It also serves /internal/sweep, which workers drives on a ticker. The sweep is
// deliberately NOT a loop inside this process: keeping it out means this service
// can run more than one replica when the seat-claim path needs it, which is
// exactly what milestone 9 will ask for.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/slash3b/tickets/pkg/env"
	"github.com/slash3b/tickets/pkg/health"
	"github.com/slash3b/tickets/pkg/logger"
	"github.com/slash3b/tickets/pkg/migrate"
	"github.com/slash3b/tickets/pkg/obs"
	"github.com/slash3b/tickets/services/inventory"
	"github.com/slash3b/tickets/services/inventory/store"

	"go.uber.org/zap"
)

const (
	service = "inventory"
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
		port  = env.Get("PORT", "8080")
		debug = env.Get("DEBUG", "false") == "true"
		otlp  = env.Get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		dsn   = env.Get("DATABASE_URL", "")
	)
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownObs, logProvider, err := obs.Setup(ctx, service, version, otlp)
	if err != nil {
		return fmt.Errorf("observability: %w", err)
	}
	lg, flush := logger.MustNew(service, debug, logProvider)
	defer func() { _ = flush() }()

	pool, err := obs.Pool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()

	// Only its own schema — see the note in the catalog service.
	if err := migrate.Apply(ctx, pool, store.SchemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	api := inventory.New(store.New(pool), lg)

	mux := http.NewServeMux()
	mux.Handle("/", api.Handler())
	health.New(lg).Register(ctx, mux, 3*time.Second, 15*time.Second,
		func(ctx context.Context) error { return pool.Ping(ctx) })

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

	drain, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return errors.Join(srv.Shutdown(drain), shutdownObs(drain))
}
