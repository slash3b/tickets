// hello-service does nothing useful on purpose.
//
// It serves /healthz, emits one metric, one log line and one trace span per
// request, and is deployed by the same pipeline every real service will use:
// commit -> GitHub Actions -> Docker Hub -> Argo CD -> running pod.
//
// Its job is to prove that pipeline and the observability wiring while there is
// no business logic to confuse the picture. Every later service in this repo is
// this service with logic added — so when a trace does not show up for inventory
// or orders, the question is what they do DIFFERENTLY from this, which is a much
// smaller question than "why is tracing broken".
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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
)

const (
	service = "hello"
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
		port     = env.Get("PORT", "8080")
		debug    = env.Get("DEBUG", "false") == "true"
		endpoint = env.Get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	)

	lg, flush := logger.MustNew(service, debug)
	defer func() { _ = flush() }()

	// SIGTERM is what Kubernetes sends first. Honouring it is what makes a rolling
	// update invisible instead of a burst of connection resets.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownObs, err := obs.Setup(ctx, service, version, endpoint)
	if err != nil {
		return fmt.Errorf("observability: %w", err)
	}
	lg.Info("observability ready", zap.String("otlp_endpoint", orNone(endpoint)))

	greetings, err := otel.Meter(service).Int64Counter("hello.greetings",
		metric.WithDescription("Greetings served"),
		metric.WithUnit("{greeting}"))
	if err != nil {
		return fmt.Errorf("meter: %w", err)
	}

	tracer := otel.Tracer(service)
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "greet")
		defer span.End()

		name := r.URL.Query().Get("name")
		if name == "" {
			name = "world"
		}

		span.SetAttributes(attribute.String("greeting.name", name))
		greetings.Add(ctx, 1, metric.WithAttributes(attribute.String("greeting.name", name)))

		// logger.Ctx attaches trace_id and span_id, which is what lets this line be
		// found from the trace and vice versa.
		logger.Ctx(ctx, lg).Info("greeted", zap.String("name", name))

		fmt.Fprintf(w, "hello, %s\n", name)
	})

	health.New(lg).Register(ctx, mux, 2*time.Second, 15*time.Second)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

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

	// Fresh context: ctx is already cancelled, and draining needs a live one.
	drain, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	return errors.Join(srv.Shutdown(drain), shutdownObs(drain))
}

func orNone(s string) string {
	if s == "" {
		return "(none — exporters disabled)"
	}
	return s
}
