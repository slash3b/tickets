package events

import (
	"context"
	"testing"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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

	_, headers, ps := startPublish(root, "inventory.seat.sold", "evt-1", 42)
	// Ended the way the writer ends it, with the partition the broker chose.
	finishPublish([]kafka.Message{{Partition: 2, Offset: 99, WriterData: ps}}, nil)
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

// TestPartitionIsAStringAttribute is a direct regression test for a bug that
// looked like nothing at all.
//
// messaging.destination.partition.id was set with attribute.Int, so it landed in
// the numeric attribute map. SigNoz's Kafka view reads the string map, so its
// onboarding check reported the attribute missing while the code plainly set it.
// The semantic convention specifies a string; the int was simply wrong.
func TestPartitionIsAStringAttribute(t *testing.T) {
	kv := partitionID(7)

	if kv.Key != "messaging.destination.partition.id" {
		t.Fatalf("key = %q", kv.Key)
	}
	if kv.Value.Type() != attribute.STRING {
		t.Fatalf("partition id is a %v; the convention says STRING, and anything "+
			"else lands in a map SigNoz does not read", kv.Value.Type())
	}
	if kv.Value.AsString() != "7" {
		t.Fatalf("value = %q, want \"7\"", kv.Value.AsString())
	}
}

// TestProducerSpanCarriesThePartition: the partition is only knowable once the
// writer has built the batch, so the span has to survive until Completion. If it
// ended at WriteMessages the producer would have no partition at all — which is
// the one attribute the hot-partition question is about.
func TestProducerSpanCarriesThePartition(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	tracer = otel.Tracer("events")

	_, _, ps := startPublish(context.Background(), "orders.created", "order-1", 10)
	if len(exp.GetSpans()) != 0 {
		t.Fatal("the producer span ended at WriteMessages; the partition is not known yet")
	}

	finishPublish([]kafka.Message{{Partition: 2, Offset: 41, WriterData: ps}}, nil)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want the producer span ended by Completion", len(spans))
	}
	got := map[string]string{}
	for _, a := range spans[0].Attributes {
		got[string(a.Key)] = a.Value.Emit()
	}
	if got["messaging.destination.partition.id"] != "2" {
		t.Errorf("partition = %q, want \"2\"", got["messaging.destination.partition.id"])
	}
	if got["messaging.client_id"] == "" {
		t.Error("no messaging.client_id — SigNoz reports it missing without one")
	}
	if spans[0].SpanKind != trace.SpanKindProducer {
		t.Errorf("kind = %v, want producer", spans[0].SpanKind)
	}
}
