// The inventory service: what is AVAILABLE.
//
// The contended core. Every seat claim happens here, in one conditional UPDATE,
// and this is the only process with credentials for the inventory schema.
//
// The sweep is NOT a loop in here — workers calls Sweep on a ticker. Keeping the
// singleton outside is what lets this service run more than one replica when the
// seat-claim path needs it.
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
	"github.com/slash3b/tickets/pkg/migrate"
	"github.com/slash3b/tickets/pkg/obs"

	pb "github.com/slash3b/tickets/gen/tickets/v1"
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
		httpPort  = env.Get("PORT", "8080")
		grpcPort  = env.Get("GRPC_PORT", "9090")
		debug     = env.Get("DEBUG", "false") == "true"
		otlp      = env.Get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		dsn       = env.Get("DATABASE_URL", "")
		kafkaAddr = env.Get("KAFKA_BROKERS", "")
		bankURL   = env.Get("BANK_URL", "http://bank.bank.svc.cluster.local")
	)
	_ = bankURL
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
	// worked and quietly meant every service could see every table.
	if err := migrate.Apply(ctx, pool, store.SchemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	// KAFKA IS OPTIONAL. With no broker configured the publisher is nil and every
	// Publish is a no-op — the system keeps working, browsers just go back to
	// polling. A message broker must not be something this service cannot boot
	// without.
	var pub *events.Publisher
	if brokers := events.Brokers(kafkaAddr); brokers != nil {
		pub = events.NewPublisher(brokers, lg)
		defer func() { _ = pub.Close() }()
		lg.Info("publishing seat changes", zap.String("kafka", kafkaAddr))
	}

	grpcSrv := grpcx.NewServer(lg)
	pb.RegisterInventoryServiceServer(grpcSrv, inventory.NewServer(store.New(pool), pub, lg))

	// TWO LISTENERS, DELIBERATELY. gRPC serves the peers; a tiny HTTP server
	// serves /livez and /readyz, because kubelet probes speak HTTP and wiring
	// grpc-health-probe into every image buys nothing here.
	mux := http.NewServeMux()
	health.New(lg).Register(ctx, mux, 3*time.Second, 15*time.Second,
		func(ctx context.Context) error { return pool.Ping(ctx) })
	httpSrv := &http.Server{Addr: ":" + httpPort, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	lis, err := grpcx.Listen(grpcPort)
	if err != nil {
		return err
	}

	errc := make(chan error, 2)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("health server: %w", err)
		}
	}()
	go func() {
		lg.Info("serving grpc", zap.String("addr", lis.Addr().String()),
			zap.String("health", httpSrv.Addr))
		if err := grpcSrv.Serve(lis); err != nil {
			errc <- fmt.Errorf("grpc: %w", err)
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		lg.Info("shutting down")
	}

	// GracefulStop lets in-flight calls finish. A saga step cut off mid-charge is
	// exactly the ambiguity this system spends its time avoiding.
	grpcSrv.GracefulStop()
	drain, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return errors.Join(httpSrv.Shutdown(drain), shutdownObs(drain))
}
