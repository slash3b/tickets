// The public API. The only service a browser reaches.
//
// IT IS NOW JUST A GATEWAY. It holds no database handle, applies no schema and
// owns no data. It calls catalog for what EXISTS, inventory for what is
// AVAILABLE, and orders to buy — and its whole job is assembling those answers
// into the shapes a seat map needs.
//
// Before 2026-08-28 catalog, inventory, orders and payments were packages
// compiled into this binary. They are separate deployments now.
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
	"github.com/slash3b/tickets/pkg/obs"
	"github.com/slash3b/tickets/services/catalog"
	"github.com/slash3b/tickets/services/gateway"
	"github.com/slash3b/tickets/services/inventory"
	"github.com/slash3b/tickets/services/orders"

	"go.uber.org/zap"
)

const (
	service = "gateway"
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
		catalogURL   = env.Get("CATALOG_URL", "http://catalog.tickets.svc.cluster.local")
		inventoryURL = env.Get("INVENTORY_URL", "http://inventory.tickets.svc.cluster.local")
		ordersURL    = env.Get("ORDERS_URL", "http://orders.tickets.svc.cluster.local")
		holdTTL      = envDuration("HOLD_TTL", 5*time.Minute)
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownObs, logProvider, err := obs.Setup(ctx, service, version, otlp)
	if err != nil {
		return fmt.Errorf("observability: %w", err)
	}
	lg, flush := logger.MustNew(service, debug, logProvider)
	defer func() { _ = flush() }()

	// Placing an order is the slow one: it fans out to inventory, payments and
	// the bank behind them. Reads get a short timeout because a slow seat map is
	// worse than no seat map — the browser is polling and will ask again.
	api := gateway.New(
		gateway.CatalogClient{C: catalog.NewClient(catalogURL, 3*time.Second)},
		gateway.InventoryClient{C: inventory.NewClient(inventoryURL, 5*time.Second)},
		orders.NewClient(ordersURL, 20*time.Second),
		holdTTL,
		lg,
	)

	mux := http.NewServeMux()
	mux.Handle("/", api.Handler())

	// NO READINESS CHECK ON DOWNSTREAM SERVICES, deliberately.
	//
	// It is tempting to fail /readyz when catalog is unreachable. Do not: that
	// takes the entire front door out of rotation because one dependency is
	// unwell, turning a partial outage into a total one. The gateway is ready
	// when it can serve; individual endpoints report their own failures.
	health.New(lg).Register(ctx, mux, 3*time.Second, 15*time.Second)

	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() {
		lg.Info("listening",
			zap.String("addr", srv.Addr),
			zap.String("catalog", catalogURL),
			zap.String("inventory", inventoryURL),
			zap.String("orders", ordersURL))
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

func envDuration(key string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(env.Get(key, ""))
	if err != nil {
		return fallback
	}
	return d
}
