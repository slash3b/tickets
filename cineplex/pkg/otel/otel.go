package otel

import (
	"cineplex/pkg/env"
	"context"
	"errors"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

const (
	serviceName    = "cineplex"
	serviceVersion = "0.1.0"
	// Collector endpoint - no http:// prefix!
	collectorEndpoint = "otel-collector-opentelemetry-collector.monitoring.svc.cluster.local:4318"
)

var collectorEndpoint2 = env.Get("OTEL_EXPORTER_OTLP_ENDPOINT", "")

// SetupOTelSDK bootstraps the OpenTelemetry pipeline.
// If it does not return an error, make sure to call shutdown for proper cleanup.
// from: https://opentelemetry.io/docs/languages/go/getting-started/
func SetupOTelSDK(ctx context.Context) (shutdown func(context.Context) error, err error) {

	var shutdownFuncs []func(context.Context) error

	// shutdown calls cleanup functions registered via shutdownFuncs.
	// The errors from the calls are joined.
	// Each registered cleanup will be invoked once.
	shutdown = func(ctx context.Context) error {
		var err error
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}

		shutdownFuncs = nil

		return err
	}

	// handleErr calls shutdown for cleanup and makes sure that all errors are returned.
	handleErr := func(inErr error) {
		err = errors.Join(inErr, shutdown(ctx))
	}

	// Set up propagator.
	prop := newPropagator()
	otel.SetTextMapPropagator(prop)

	// Set up resource (common for all providers)
	res, err := newResource(ctx)
	if err != nil {
		handleErr(err)
		return shutdown, err
	}

	// 1. Set up trace provider.
	tracerProvider, err := newTracerProvider(ctx, res)
	if err != nil {
		handleErr(err)
		return shutdown, err
	}

	shutdownFuncs = append(shutdownFuncs, tracerProvider.Shutdown)
	otel.SetTracerProvider(tracerProvider)

	// 2. Set up meter provider.
	meterProvider, err := newMeterProvider(ctx, res)
	if err != nil {
		handleErr(err)
		return shutdown, err
	}

	shutdownFuncs = append(shutdownFuncs, meterProvider.Shutdown)
	otel.SetMeterProvider(meterProvider)

	// 3. Set up logger provider.
	//loggerProvider, err := newLoggerProvider(ctx, res)
	//if err != nil {
	//	handleErr(err)
	//	return shutdown, err
	//}
	//
	//shutdownFuncs = append(shutdownFuncs, loggerProvider.Shutdown)
	//global.SetLoggerProvider(loggerProvider)
	//
	return shutdown, err
}

func newPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

func newResource(ctx context.Context) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(serviceVersion),
		),
	)
}
