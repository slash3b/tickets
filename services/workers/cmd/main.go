// The singletons.
//
// Three background loops that must run EXACTLY ONCE in the cluster:
//
//	inventory expiry sweeper   returns seats whose short TTL ran out
//	inventory hard-deadline    returns seats stuck converting, flags for refund
//	payments reconciler        establishes what the bank actually did
//	orders resumer             drives crashed sagas forward
//
// They live in their own binary, deployed with replicas: 1, precisely so that
// scaling the API cannot accidentally scale them. Nothing here is unsafe
// concurrently — every operation is idempotent — but N replicas do N times the
// work on the same rows and manufacture the lock contention the rest of the
// design works to avoid. The payments reconciler would also hammer the one
// dependency you least want to hammer.
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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/slash3b/tickets/pkg/env"
	"github.com/slash3b/tickets/pkg/health"
	"github.com/slash3b/tickets/pkg/logger"
	"github.com/slash3b/tickets/pkg/migrate"
	"github.com/slash3b/tickets/pkg/obs"

	catalogstore "github.com/slash3b/tickets/services/catalog/store"
	"github.com/slash3b/tickets/services/gateway"
	inventorystore "github.com/slash3b/tickets/services/inventory/store"
	ordersstore "github.com/slash3b/tickets/services/orders/store"
	"github.com/slash3b/tickets/services/payments/bankclient"
	paystore "github.com/slash3b/tickets/services/payments/store"

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
		port    = env.Get("PORT", "8080")
		debug   = env.Get("DEBUG", "false") == "true"
		otlp    = env.Get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		dsn     = env.Get("DATABASE_URL", "")
		bankURL = env.Get("BANK_URL", "http://bank.bank.svc.cluster.local")
		every   = envDuration("SWEEP_INTERVAL", 30*time.Second)
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

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()

	// Applied by whichever process starts first; the advisory lock inside makes
	// concurrent starts safe. See pkg/migrate for what this is and is not.
	if err := migrate.Apply(ctx, pool,
		catalogstore.SchemaSQL, inventorystore.SchemaSQL,
		ordersstore.SchemaSQL, paystore.SchemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	inv := inventorystore.New(pool)
	ord := ordersstore.New(pool)
	pay := paystore.New(pool)
	bankCli := bankclient.New(bankURL, 5*time.Second)

	sweeper := inventorystore.NewSweeper(inv, every)
	sweeper.OnError = func(err error) { lg.Error("sweep failed", zap.Error(err)) }
	sweeper.OnHardDeadline = func(ids []uuid.UUID) {
		// Money may have moved against seats that are now gone. Rare enough that
		// it should be alertable, loud enough that it never passes unnoticed.
		lg.Error("holds crossed the hard deadline and need reconciliation",
			zap.Int("count", len(ids)), zap.Any("hold_ids", ids))
	}

	reconciler := paystore.NewReconciler(pay, bankCli, time.Minute)
	reconciler.OnError = func(err error) { lg.Error("reconcile failed", zap.Error(err)) }
	reconciler.OnResolved = func(orderID string, state paystore.State) {
		lg.Info("payment resolved by reconciliation",
			zap.String("order_id", orderID), zap.String("state", string(state)))
	}
	reconciler.OnStuck = func(p *paystore.Payment) {
		lg.Error("payment stuck; the bank cannot account for it",
			zap.String("order_id", p.OrderID.String()),
			zap.Int("attempts", p.ReconcileAttempts))
	}

	saga := ordersstore.NewSaga(ord, gateway.InventoryAdapter{S: inv},
		paymentsBridge{store: pay, bank: bankCli})
	resumer := ordersstore.NewResumer(saga, ord, time.Minute)
	resumer.OnError = func(err error) { lg.Error("resume failed", zap.Error(err)) }
	resumer.OnResumed = func(id uuid.UUID, from ordersstore.State) {
		lg.Info("resumed a stalled order",
			zap.String("order_id", id.String()), zap.String("from", string(from)))
	}

	go sweeper.Run(ctx)
	go reconciler.Run(ctx, every)
	go resumer.Run(ctx, every)

	lg.Info("singletons running", zap.Duration("interval", every))

	// Probes only — this binary serves no traffic, but Kubernetes still needs to
	// know whether it is alive and whether it can reach the database.
	mux := http.NewServeMux()
	health.New(lg).Register(ctx, mux, 3*time.Second, 15*time.Second,
		func(ctx context.Context) error { return pool.Ping(ctx) })
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			lg.Error("probe server", zap.Error(err))
		}
	}()

	<-ctx.Done()
	lg.Info("shutting down")

	drain, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return errors.Join(srv.Shutdown(drain), shutdownObs(drain))
}

type paymentsBridge struct {
	store *paystore.Store
	bank  *bankclient.Client
}

func (p paymentsBridge) Charge(ctx context.Context, orderID uuid.UUID, amountMinor int64) (ordersstore.PaymentOutcome, string, error) {
	pay, err := p.store.Create(ctx, orderID, amountMinor)
	if err != nil {
		return "", "", err
	}
	charge, err := p.bank.AuthorizeAndReconcile(ctx, pay.IdempotencyKey, amountMinor)
	switch {
	case err == nil:
		return ordersstore.PaymentSucceeded, "",
			p.store.Resolve(ctx, orderID, paystore.StateSucceeded, charge.ID, "")
	case charge != nil && charge.Status == "declined":
		return ordersstore.PaymentFailed, charge.DeclineCode,
			p.store.Resolve(ctx, orderID, paystore.StateFailed, "", charge.DeclineCode)
	default:
		return ordersstore.PaymentUnknown, "", p.store.MarkUnknown(ctx, orderID)
	}
}

func envDuration(key string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(env.Get(key, ""))
	if err != nil {
		return fallback
	}
	return d
}
