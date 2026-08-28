// The payments service: whether money moved.
//
// It owns the payments schema and the only connection to the bank. Nothing else
// in the system talks to the bank, which is the point: there is exactly one place
// that knows an idempotency key exists, and exactly one place that can decide a
// charge is unknown rather than failed.
//
// It serves /internal/reconcile, which workers drives on a ticker. The reconciler
// is not a loop in here for the same reason the sweeper is not a loop in
// inventory — keeping the singleton outside lets this service run more than one
// replica when it needs to.
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
	"github.com/slash3b/tickets/services/payments"
	"github.com/slash3b/tickets/services/payments/bankclient"
	"github.com/slash3b/tickets/services/payments/store"

	"go.uber.org/zap"
)

const (
	service = "payments"
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
		dsn     = env.Get("DATABASE_URL", "")
		bankURL = env.Get("BANK_URL", "http://bank.bank.svc.cluster.local")
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

	// The bank timeout is deliberately generous: the bank is SUPPOSED to be slow
	// sometimes, and cutting it off early converts a slow answer into an unknown
	// one, which is strictly worse than waiting.
	bank := bankclient.New(bankURL, 5*time.Second)
	pay := store.New(pool)
	rec := store.NewReconciler(pay, bank, time.Minute)

	api := payments.New(pay, bank, rec, lg)

	mux := http.NewServeMux()
	mux.Handle("/", api.Handler())
	health.New(lg).Register(ctx, mux, 3*time.Second, 15*time.Second,
		func(ctx context.Context) error { return pool.Ping(ctx) })

	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() {
		lg.Info("listening", zap.String("addr", srv.Addr), zap.String("bank", bankURL))
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
