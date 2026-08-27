// The public API.
//
// NOTE ON SHAPE: DESIGN.md describes catalog, inventory, orders and payments as
// separate services speaking gRPC. Today they are PACKAGES IN THIS BINARY. That
// is a deliberate staging decision, not a change of design — every boundary is
// already a consumer-declared interface, so splitting them out later is wiring
// rather than rework, and running one process until there is a reason to run five
// avoids paying for network hops that buy nothing yet.
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

	"github.com/google/uuid"
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
		port    = env.Get("PORT", "8080")
		debug   = env.Get("DEBUG", "false") == "true"
		otlp    = env.Get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		dsn     = env.Get("DATABASE_URL", "")
		bankURL = env.Get("BANK_URL", "http://bank.bank.svc.cluster.local")
		holdTTL = envDuration("HOLD_TTL", 5*time.Minute)
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

	cat := catalogstore.New(pool)
	inv := inventorystore.New(pool)
	ord := ordersstore.New(pool)
	pay := paystore.New(pool)

	invAdapter := gateway.InventoryAdapter{S: inv}
	saga := ordersstore.NewSaga(ord, invAdapter, payments{store: pay, bank: bankclient.New(bankURL, 5*time.Second)})

	api := gateway.New(
		gateway.CatalogAdapter{S: cat},
		invAdapter,
		gateway.OrdersAdapter{S: ord, Saga: saga},
		holdTTL,
	)

	mux := http.NewServeMux()
	mux.Handle("/", api.Handler())
	// Readiness depends on the database: a gateway that cannot reach Postgres
	// should leave the load balancer rotation rather than serve 500s.
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

// payments bridges the payment store and the bank into what the saga needs.
type payments struct {
	store *paystore.Store
	bank  *bankclient.Client
}

func (p payments) Charge(ctx context.Context, orderID uuid.UUID, amountMinor int64) (ordersstore.PaymentOutcome, string, error) {
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
		// UNKNOWN. Not failed. The reconciler in workers will establish the truth.
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
