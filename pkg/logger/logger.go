// Package logger builds the one logger every service uses.
package logger

import (
	"context"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// MustNew returns a structured logger tagged with the service name, and its flush
// function. Call the flush on shutdown or buffered lines are lost.
func MustNew(service string, debug bool) (*zap.Logger, func() error) {
	build := zap.NewProduction
	if debug {
		build = zap.NewDevelopment
	}

	l, err := build()
	if err != nil {
		panic(err)
	}

	return l.With(zap.String("service.name", service)), l.Sync
}

// Ctx returns a logger carrying the trace and span ids from ctx.
//
// DESIGN.md requires a trace id on every line, and this is how that happens. A log
// line without one cannot be correlated to the request that produced it, which on a
// single-store observability backend makes it close to worthless — you can find it,
// but you cannot find what it belongs to.
func Ctx(ctx context.Context, lg *zap.Logger) *zap.Logger {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return lg
	}

	return lg.With(
		zap.String("trace_id", sc.TraceID().String()),
		zap.String("span_id", sc.SpanID().String()),
	)
}
