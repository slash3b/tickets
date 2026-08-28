// The catalog service: what EXISTS.
//
// Venues, sections, seats, events and prices. Read-mostly and almost never
// written — the seeder adds one showing a day and that is the whole write load.
//
// It owns the catalog schema and is the only process that applies it. It does NOT
// write inventory.event_seats, not even when a showing is created: it reports
// which seats exist and inventory opens them. Inventory is the only writer of seat
// status anywhere, and letting catalog take that shortcut would end the guarantee.
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
	"github.com/slash3b/tickets/services/catalog"
	"github.com/slash3b/tickets/services/catalog/store"

	"go.uber.org/zap"
)

const (
	service = "catalog"
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

	// ONLY ITS OWN SCHEMA. Before the split one process applied all four, which
	// worked and quietly meant every service could see every table. Each service
	// now owns exactly one schema, so a query crossing a boundary fails at the
	// database rather than passing review.
	if err := migrate.Apply(ctx, pool, store.SchemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	api := catalog.New(store.New(pool), lg)

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
