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
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/slash3b/tickets/pkg/env"
	"github.com/slash3b/tickets/pkg/health"
	"github.com/slash3b/tickets/pkg/logger"
	"github.com/slash3b/tickets/pkg/obs"
	"github.com/slash3b/tickets/services/inventory"
	"github.com/slash3b/tickets/services/orders"
	"github.com/slash3b/tickets/services/payments"

	"go.uber.org/zap"
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
		port         = env.Get("PORT", "8080")
		debug        = env.Get("DEBUG", "false") == "true"
		otlp         = env.Get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		inventoryURL = env.Get("INVENTORY_URL", "http://inventory.tickets.svc.cluster.local")
		ordersURL    = env.Get("ORDERS_URL", "http://orders.tickets.svc.cluster.local")
		paymentsURL  = env.Get("PAYMENTS_URL", "http://payments.tickets.svc.cluster.local")
		every        = envDuration("SWEEP_INTERVAL", 30*time.Second)
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownObs, logProvider, err := obs.Setup(ctx, service, version, otlp)
	if err != nil {
		return fmt.Errorf("observability: %w", err)
	}
	lg, flush := logger.MustNew(service, debug, logProvider)
	defer func() { _ = flush() }()

	// Generous timeouts: a pass that takes a while is fine, it runs on a ticker
	// and nobody is waiting. Reconcile is longest because it may talk to a bank
	// that is deliberately slow.
	inv := inventory.NewClient(inventoryURL, 30*time.Second)
	ord := orders.NewClient(ordersURL, 60*time.Second)
	pay := payments.NewClient(paymentsURL, 60*time.Second)

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
			zap.String("inventory", inventoryURL), zap.String("orders", ordersURL),
			zap.String("payments", paymentsURL))
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
		result, err := once(ctx)
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
