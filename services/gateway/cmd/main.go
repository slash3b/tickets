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
	"github.com/slash3b/tickets/pkg/events"
	"github.com/slash3b/tickets/pkg/grpcx"
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
		port          = env.Get("PORT", "8080")
		debug         = env.Get("DEBUG", "false") == "true"
		otlp          = env.Get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		catalogAddr   = env.Get("CATALOG_ADDR", "catalog.tickets.svc.cluster.local:9090")
		inventoryAddr = env.Get("INVENTORY_ADDR", "inventory.tickets.svc.cluster.local:9090")
		ordersAddr    = env.Get("ORDERS_ADDR", "orders.tickets.svc.cluster.local:9090")
		holdTTL       = envDuration("HOLD_TTL", 5*time.Minute)
		kafkaAddr     = env.Get("KAFKA_BROKERS", "")
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownObs, logProvider, err := obs.Setup(ctx, service, version, otlp)
	if err != nil {
		return fmt.Errorf("observability: %w", err)
	}
	lg, flush := logger.MustNew(service, debug, logProvider)
	defer func() { _ = flush() }()

	// grpcx.Dial does not block, so the gateway boots whether or not its peers
	// are up yet. A front door that refuses to start because a dependency is
	// briefly down makes a cluster restart ordering-dependent.
	catConn, err := grpcx.Dial(catalogAddr)
	if err != nil {
		return err
	}
	defer func() { _ = catConn.Close() }()

	invConn, err := grpcx.Dial(inventoryAddr)
	if err != nil {
		return err
	}
	defer func() { _ = invConn.Close() }()

	ordConn, err := grpcx.Dial(ordersAddr)
	if err != nil {
		return err
	}
	defer func() { _ = ordConn.Close() }()

	api := gateway.New(
		gateway.CatalogClient{C: catalog.NewClient(catConn)},
		gateway.InventoryClient{C: inventory.NewClient(invConn)},
		orders.NewClient(ordConn),
		holdTTL,
		lg,
	)

	// LIVE SEAT MAP. Each replica subscribes with its OWN group id, which is the
	// opposite of the usual advice and is required here: a consumer group splits
	// partitions between its members, so with six gateway replicas in one group
	// each message would reach exactly one of them and browsers connected to the
	// other five would never hear it. This is a broadcast, not a work queue.
	//
	// The hostname makes the id unique per pod, which is all that is needed.
	if brokers := events.Brokers(kafkaAddr); brokers != nil {
		api = api.WithStreaming(lg)
		host, _ := os.Hostname()
		groupID := "gateway-stream-" + host
		events.Subscribe(ctx, brokers,
			[]string{events.TopicSeatHeld, events.TopicSeatReleased, events.TopicSeatSold},
			groupID, lg, func(_ string, c events.SeatChange) { api.Broadcast(c) })
		lg.Info("live seat map on", zap.String("kafka", kafkaAddr), zap.String("group", groupID))
	}

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
			zap.String("catalog", catalogAddr),
			zap.String("inventory", inventoryAddr),
			zap.String("orders", ordersAddr))
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
