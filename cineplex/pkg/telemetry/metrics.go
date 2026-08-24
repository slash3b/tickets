package telemetry

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const name = "cineplex"

var (
	Tracer = otel.Tracer(name)
	meter  = otel.Meter(name)

	CineplexCallMetricCounter = must(
		meter.Int64Counter("cineplex.call_to_fetch_movies",
			metric.WithDescription("The number of calls to cineplex.md"),
			metric.WithUnit("{call}")),
	)
)

func must(m metric.Int64Counter, err error) metric.Int64Counter {
	if err != nil {
		panic(err)
	}

	return m
}

// https://opentelemetry.io/docs/languages/go/getting-started

// func rolldice(w http.ResponseWriter, r *http.Request) {
//	ctx, span := tracer.Start(r.Context(), "roll")
//	defer span.End()
//
//	roll := 1 + rand.Intn(6)
//
//	rollValueAttr := attribute.Int("roll.value", roll)
//	span.SetAttributes(rollValueAttr)

//	rollValueAttr := attribute.Int("roll.value", roll)
//	rollCnt.Add(ctx, 1, metric.WithAttributes(rollValueAttr))
//
//	resp := strconv.Itoa(roll) + "\n"
//	if _, err := io.WriteString(w, resp); err != nil {
//		log.Printf("Write failed: %v\n", err)
//	}
//}
