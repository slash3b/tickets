// Package obs sets up OpenTelemetry: traces and metrics over OTLP.
//
// Every service in this repo speaks PLAIN OTLP and knows nothing about what is on
// the other end. Today that is SigNoz. If SigNoz is ever replaced with Tempo and
// VictoriaMetrics, the change is one environment variable and a collector config —
// not a line of Go in any service. That vendor neutrality is the whole reason to
// use OTLP rather than a backend's own SDK, and it is only free while nobody
// imports a vendor package here.
//
// If OTEL_EXPORTER_OTLP_ENDPOINT is empty, setup is a no-op: the service runs with
// no exporters and never blocks on a collector that does not exist. That is what
// makes a service developable on a laptop, and what let the first service ship
// before the observability stack existed.
package obs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// Shutdown flushes and stops every provider. Call it, or the last batch of spans
// and metrics dies with the process.
type Shutdown func(context.Context) error

// Setup installs global tracer and meter providers.
//
// endpoint is host:port with NO scheme — "signoz-otel-collector.signoz:4318", not
// "http://...". The OTLP HTTP exporter rejects a scheme here, which is an easy
// half hour to lose.
// The returned LoggerProvider is nil when no endpoint is configured; pass it to
// logger.MustNew, which then logs to stdout only.
func Setup(ctx context.Context, service, version, endpoint string) (Shutdown, otellog.LoggerProvider, error) {
	// Propagators are set even with no exporter: they cost nothing and mean an
	// incoming traceparent header is still honoured.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// WithFromEnv is what makes OTEL_RESOURCE_ATTRIBUTES work, and its absence is
	// why every span arrived with no environment and no pod identity. The
	// deployment manifests set that variable from the downward API, so a span can
	// be traced back to the exact pod that emitted it without any service knowing
	// it runs in Kubernetes. WithHost adds host.name for the same reason.
	//
	// Order matters: explicit attributes go LAST so a stray environment variable
	// cannot rename a service out from under it.
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithHost(),
		resource.WithAttributes(
			semconv.ServiceName(service),
			semconv.ServiceVersion(version),
			// WHICH REPLICA, not just which service. Six inventory pods all report
			// service.name=inventory, and without this there is no way to ask
			// "is it always the same pod that is slow" — the question you have the
			// moment a service is scaled and one instance misbehaves.
			//
			// It is also required by SigNoz's Kafka view, which reported it
			// missing. The hostname is the pod name in Kubernetes and the same
			// string pkg/events uses as its Kafka client id, so a pod, its client
			// and its telemetry all answer to one name.
			semconv.ServiceInstanceID(instanceID()),
		))
	if err != nil {
		return nil, nil, fmt.Errorf("otel resource: %w", err)
	}

	// No endpoint: still install a real TracerProvider, just without an exporter.
	// Spans are recorded and get valid ids that nothing ever ships anywhere.
	//
	// This matters more than it looks. A no-op tracer produces an INVALID span
	// context, so logger.Ctx finds no trace id and every local log line loses the
	// correlation that DESIGN.md insists on — meaning the thing you most need to
	// see while developing is precisely the thing missing on a laptop. This way
	// trace_id appears in local logs exactly as it does in the cluster.
	if endpoint == "" {
		tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res))
		otel.SetTracerProvider(tp)

		return tp.Shutdown, nil, nil
	}

	// WithInsecure: plain HTTP to a collector inside the cluster. Fine here; it
	// never leaves the pod network.
	traceExp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("otlp trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	metricExp, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpoint(endpoint),
		otlpmetrichttp.WithInsecure(),
	)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("otlp metric exporter: %w", err), tp.Shutdown(ctx))
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithInterval(30*time.Second))),
	)
	otel.SetMeterProvider(mp)

	logExp, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpoint(endpoint),
		otlploghttp.WithInsecure(),
	)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("otlp log exporter: %w", err),
			tp.Shutdown(ctx), mp.Shutdown(ctx))
	}

	// RUNTIME METRICS, for every service, free. heap in use, goroutine count, GC
	// pause time, allocation rate. The k8s-infra DaemonSet already reports CPU and
	// memory PER POD, but that is the container's view: it cannot tell a memory
	// climb caused by a leak from one caused by a bigger workload, and it is keyed
	// by pod name so it does not survive a rollout. These are keyed by
	// service.name and describe the process's own behaviour, which is the half
	// that answers "why".
	if err := otelruntime.Start(otelruntime.WithMeterProvider(mp)); err != nil {
		return nil, nil, errors.Join(fmt.Errorf("runtime metrics: %w", err),
			tp.Shutdown(ctx), mp.Shutdown(ctx))
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
	)

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
	}, lp, nil
}

// instanceID names this process. The pod name in Kubernetes, the hostname
// anywhere else, and a generated id if even that fails — an id that changes on
// restart is still far better than every replica sharing one.
func instanceID() string {
	if pod := os.Getenv("POD_NAME"); pod != "" {
		return pod
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}
