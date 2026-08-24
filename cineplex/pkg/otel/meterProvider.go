package otel

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

func newMeterProvider(ctx context.Context, res *resource.Resource) (*metric.MeterProvider, error) {
	// OTLP exporter - will automatically use OTEL_EXPORTER_OTLP_ENDPOINT
	metricExporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpoint(collectorEndpoint2),
		otlpmetrichttp.WithInsecure(), // Use HTTP instead of HTTPS
	)
	if err != nil {
		return nil, err
	}

	// Optional: Console exporter for debugging
	consoleExporter, err := stdoutmetric.New()
	if err != nil {
		return nil, err
	}

	meterProvider := metric.NewMeterProvider(
		metric.WithResource(res),
		// OTLP exporter - sends to collector using environment variables
		// no it does not
		metric.WithReader(metric.NewPeriodicReader(
			metricExporter,
			metric.WithInterval(time.Second*10),
		)),
		// Console exporter - for debugging (optional)
		metric.WithReader(metric.NewPeriodicReader(
			consoleExporter,
			metric.WithInterval(time.Second*5),
		)),
	)

	return meterProvider, nil
}
