package events

import (
	"context"
	"testing"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestTraceCrossesTheBroker is the whole point of the header carrier: a consumer
// on the other side of Kafka must land in the SAME TRACE as the publish that
// caused it. Without this the seat-map update is an orphan root and a purchase
// stops at the publish.
func TestTraceCrossesTheBroker(t *testing.T) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})
	tracer = otel.Tracer("events")

	// The producing side, inside some request's trace.
	root, rootSpan := otel.Tracer("test").Start(context.Background(), "request")
	want := rootSpan.SpanContext().TraceID()

	_, headers, endPublish := startPublish(root, "inventory.seat.sold", "evt-1", 42)
	endPublish()
	rootSpan.End()

	if len(headers) == 0 {
		t.Fatal("no headers injected — nothing to carry the trace across the broker")
	}

	// The consuming side, in a different process with a fresh context.
	msg := kafka.Message{Topic: "inventory.seat.sold", Key: []byte("evt-1"), Headers: headers}
	got, endConsume := startConsume(context.Background(), msg, "gateway-stream-abc")
	defer endConsume()

	sc := trace.SpanContextFromContext(got)
	if !sc.IsValid() {
		t.Fatal("consumer span has no span context")
	}
	if sc.TraceID() != want {
		t.Fatalf("consumer trace = %s, want %s — the trace did not survive the broker",
			sc.TraceID(), want)
	}
}

// TestHeaderCarrierReplaces: a retried publish must not leave two traceparents.
// The extractor takes the first, so an appended second one would silently pin
// every retry to the original attempt's trace.
func TestHeaderCarrierReplaces(t *testing.T) {
	var h []kafka.Header
	c := kafkaHeaders{&h}

	c.Set("traceparent", "first")
	c.Set("traceparent", "second")

	if len(h) != 1 {
		t.Fatalf("headers = %v, want one traceparent", h)
	}
	if got := c.Get("traceparent"); got != "second" {
		t.Fatalf("traceparent = %q, want the latest value", got)
	}
	if keys := c.Keys(); len(keys) != 1 || keys[0] != "traceparent" {
		t.Fatalf("keys = %v", keys)
	}
}

// TestMissingHeadersStartAFreshTrace: a message published before this existed,
// or by anything else, must still be consumable.
func TestMissingHeadersStartAFreshTrace(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	ctx, end := startConsume(context.Background(), kafka.Message{Topic: "t"}, "g")
	defer end()
	if ctx == nil {
		t.Fatal("no context returned for an untraced message")
	}
}
