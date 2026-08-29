// The singletons.
//
// Three background loops that must run EXACTLY ONCE in the cluster: the
// inventory sweep, the payment reconciler and the order resumer.
//
// SINCE THE SPLIT IT OWNS NO DATA AND NO DATABASE. It is a ticker that calls
// /internal/* on the service that owns each loop. That is the point: the loops
// have to be singletons, but the SERVICES they act on do not — keeping the timer
// out here is what lets inventory run more than one replica when the seat-claim
// path needs it, which is exactly what milestone 9 will ask for.
//
// THIS MUST STAY AT ONE REPLICA. Nothing here is unsafe concurrently, but N
// replicas do N times the work on the same rows and multiply traffic to the bank.
// strategy: Recreate is deliberate — a rolling update would briefly run two.
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/slash3b/tickets/pkg/env"
	"github.com/slash3b/tickets/pkg/grpcx"
	"github.com/slash3b/tickets/pkg/health"
	"github.com/slash3b/tickets/pkg/logger"
	"github.com/slash3b/tickets/pkg/obs"
	"github.com/slash3b/tickets/services/catalog"
	"github.com/slash3b/tickets/services/inventory"
	"github.com/slash3b/tickets/services/orders"
	"github.com/slash3b/tickets/services/payments"

	"go.uber.org/zap"
	"google.golang.org/grpc"
)

const (
	service = "workers"
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
		paymentsAddr  = env.Get("PAYMENTS_ADDR", "payments.tickets.svc.cluster.local:9090")
		every         = envDuration("SWEEP_INTERVAL", 30*time.Second)
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownObs, logProvider, err := obs.Setup(ctx, service, version, otlp)
	if err != nil {
		return fmt.Errorf("observability: %w", err)
	}
	lg, flush := logger.MustNew(service, debug, logProvider)
	defer func() { _ = flush() }()

	conns := map[string]*grpc.ClientConn{}
	for name, addr := range map[string]string{
		"catalog": catalogAddr, "inventory": inventoryAddr,
		"orders": ordersAddr, "payments": paymentsAddr,
	} {
		conn, err := grpcx.Dial(addr)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		conns[name] = conn
	}

	cat := catalog.NewClient(conns["catalog"])
	inv := inventory.NewClient(conns["inventory"])
	ord := orders.NewClient(conns["orders"])
	pay := payments.NewClient(conns["payments"])

	// THE ON-SALE LOOP. Opening an event's seats IS its on-sale: before this runs
	// there are no rows in inventory.event_seats, so every hold fails on its own
	// and no code anywhere checks a clock on the hot path.
	//
	// It ticks faster than the others because the gap between on_sale_at and the
	// sale actually starting is the one delay a customer sees.
	go loop(ctx, lg, "onsale", 5*time.Second, func(ctx context.Context) (string, error) {
		return openDue(ctx, lg, cat, inv)
	})

	go loop(ctx, lg, "sweep", every, func(ctx context.Context) (string, error) {
		expired, hard, err := inv.Sweep(ctx)
		if hard > 0 {
			// A hold reaching its HARD deadline was stuck in `converting`, which
			// means a payment outcome was never established. Not routine cleanup.
			lg.Warn("holds hit the hard deadline", zap.Int("count", hard))
		}
		return fmt.Sprintf("expired=%d hard=%d", expired, hard), err
	})

	go loop(ctx, lg, "reconcile", every, func(ctx context.Context) (string, error) {
		n, err := pay.Reconcile(ctx)
		return fmt.Sprintf("resolved=%d", n), err
	})

	go loop(ctx, lg, "resume", every, func(ctx context.Context) (string, error) {
		n, err := ord.Resume(ctx)
		return fmt.Sprintf("resumed=%d", n), err
	})

	mux := http.NewServeMux()
	health.New(lg).Register(ctx, mux, 3*time.Second, 15*time.Second)
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() {
		lg.Info("running", zap.String("addr", srv.Addr), zap.Duration("every", every),
			zap.String("inventory", inventoryAddr), zap.String("orders", ordersAddr),
			zap.String("payments", paymentsAddr))
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

// openDue starts any sale whose moment has arrived.
//
// ORDER MATTERS AND IT IS DELIBERATE: ask inventory to open the seats FIRST, and
// only mark the event opened once inventory has confirmed. If the process dies
// in between, the event stays in the queue and the next tick opens it again —
// which is free, because OpenEvent is idempotent. The other order would risk
// recording a sale as started that never was, and nothing would ever retry it.
func openDue(ctx context.Context, lg *zap.Logger, cat *catalog.Client, inv *inventory.Client) (string, error) {
	due, err := cat.ListDueForOnSale(ctx, 10)
	if err != nil {
		return "", err
	}
	if len(due) == 0 {
		return "none due", nil
	}

	opened := 0
	for _, e := range due {
		id, err := uuid.Parse(e.GetId())
		if err != nil {
			return "", fmt.Errorf("catalog returned a malformed event id %q: %w", e.GetId(), err)
		}

		seatIDs, err := cat.SeatIDsForEvent(ctx, id)
		if err != nil {
			return "", fmt.Errorf("seat ids for %s: %w", id, err)
		}
		n, err := inv.OpenEvent(ctx, id, seatIDs)
		if err != nil {
			return "", fmt.Errorf("open %s: %w", id, err)
		}
		if err := cat.MarkSeatsOpened(ctx, id); err != nil {
			return "", fmt.Errorf("mark opened %s: %w", id, err)
		}

		// The one line that says a sale just started. Worth INFO even though this
		// loop is otherwise silent: it is the moment everything else reacts to.
		lg.Info("on sale",
			zap.String("event_id", id.String()),
			zap.String("title", e.GetTitle()),
			zap.Int("seats", n))
		opened++
	}
	return fmt.Sprintf("opened=%d", opened), nil
}

// loop runs one pass every interval until ctx is done.
//
// A FAILED PASS IS LOGGED AND THE LOOP CONTINUES. These are idempotent catch-up
// jobs: whatever this pass missed, the next one finds. Exiting on error would
// stop the only thing that repairs the system, at exactly the moment it is needed.
func loop(ctx context.Context, lg *zap.Logger, name string, every time.Duration,
	once func(context.Context) (string, error)) {

	t := time.NewTicker(every)
	defer t.Stop()

	for {
		// A gRPC connection carries no deadline of its own; each pass gets one.
		// Without this a wedged peer would stall the ticker forever.
		passCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		result, err := once(passCtx)
		cancel()
		switch {
		case err != nil && ctx.Err() == nil:
			lg.Error(name+" failed", zap.Error(err))
		case err == nil:
			lg.Debug(name+" done", zap.String("result", result))
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func envDuration(key string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(env.Get(key, ""))
	if err != nil {
		return fallback
	}
	return d
}
