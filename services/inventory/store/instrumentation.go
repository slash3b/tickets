package store

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// Instruments for the contended core.
//
// THESE ARE CREATED AT PACKAGE INIT, DELIBERATELY. The global MeterProvider is a
// no-op until obs.Setup installs the real one, and instruments created against
// the no-op keep working afterwards — the SDK resolves them lazily. Creating them
// per call would allocate on the hottest path in the system for no benefit.
var (
	meter = otel.Meter("inventory")

	// One counter, one attribute, four values: won, lost, error, exhausted.
	// Separate counters per outcome would make "what fraction of holds lost?" a
	// query across metric names instead of a group-by, which is the wrong shape.
	holds, _ = meter.Int64Counter("tickets.holds",
		metric.WithDescription("Hold attempts by outcome"),
		metric.WithUnit("{hold}"))

	// Deadlocks and serialization failures that triggered a retry. THIS IS THE
	// EARLY WARNING: it rises while everything still looks healthy from outside,
	// because the retry loop hides it from callers entirely.
	contention, _ = meter.Int64Counter("tickets.hold.contention",
		metric.WithDescription("Retryable database conflicts during a hold"),
		metric.WithUnit("{conflict}"))

	tracer = otel.Tracer("inventory")
)
