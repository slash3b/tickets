// The orders service: the saga.
//
// The only service that calls two others. Placing an order converts a hold in
// inventory, charges in payments, and commits the seats back in inventory —
// three network calls that can each fail independently.
//
// It owns the orders schema and serves /internal/resume, which workers drives.
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
	"github.com/slash3b/tickets/services/orders"
	"github.com/slash3b/tickets/services/orders/store"
	"github.com/slash3b/tickets/services/payments"

	"go.uber.org/zap"
)

const (
	service = "orders"
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
		port         = env.Get("PORT", "8080")
		debug        = env.Get("DEBUG", "false") == "true"
		otlp         = env.Get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		dsn          = env.Get("DATABASE_URL", "")
		inventoryURL = env.Get("INVENTORY_URL", "http://inventory.tickets.svc.cluster.local")
		paymentsURL  = env.Get("PAYMENTS_URL", "http://payments.tickets.svc.cluster.local")
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

	if err := migrate.Apply(ctx, pool, store.SchemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	// The saga's two dependencies are now network clients. Its code did not
	// change: both were already consumer-declared interfaces, and these satisfy
	// them. That is the whole return on having written it that way.
	//
	// The payments timeout is longer than inventory's because the bank is on the
	// far side of it and is deliberately slow. Cutting it short would turn a slow
	// answer into an unknown one, which is strictly worse.
	inv := inventory.NewClient(inventoryURL, 5*time.Second)
	pay := orders.PaymentsAdapter{C: payments.NewClient(paymentsURL, 15*time.Second)}

	ord := store.New(pool)
	saga := store.NewSaga(ord, inv, pay)
	res := store.NewResumer(saga, ord, time.Minute)

	api := orders.New(ord, saga, res, lg)

	mux := http.NewServeMux()
	mux.Handle("/", api.Handler())
	health.New(lg).Register(ctx, mux, 3*time.Second, 15*time.Second,
		func(ctx context.Context) error { return pool.Ping(ctx) })

	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() {
		lg.Info("listening", zap.String("addr", srv.Addr),
			zap.String("inventory", inventoryURL), zap.String("payments", paymentsURL))
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
