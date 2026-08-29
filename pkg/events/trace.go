package events

import (
	"context"
	"os"
	"strconv"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Tracing for the message path.
//
// PUBLISHES AND CONSUMES USED TO BE INVISIBLE. Every other hop in this system is
// a span — HTTP in, gRPC across, SQL at the bottom — and then a seat change fell
// into Kafka and reappeared somewhere else with no thread between the two. A
// trace stopped at the publish and the consumer's work was an orphan root.
//
// It also left SigNoz's Messaging Queues page with nothing to draw: that view is
// built from producer and consumer spans carrying the messaging semantic
// conventions, and we emitted neither.
//
// THE CONSUMER IS A CHILD OF THE PRODUCER, VIA THE MESSAGE ITSELF. W3C trace
// context rides in Kafka headers, which is the only channel that survives a
// broker in the middle. That is what makes "show me this customer's purchase"
// include the seat map update it caused.

var tracer = otel.Tracer("events")

// clientID identifies this process as a Kafka client. In Kubernetes the hostname
// is the pod name, which is the granularity wanted here: one client per pod, and
// the same string pkg/obs reports as service.instance.id.
var clientID = func() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}()

// kafkaHeaders adapts Kafka's headers to the propagator's carrier interface.
//
// Kafka header values are []byte and may repeat; the propagator wants string
// keys and single values. Set REPLACES rather than appends, because a retried
// publish must not accumulate two traceparents — a carrier with two would be
// ambiguous and the extractor takes the first.
type kafkaHeaders struct{ h *[]kafka.Header }

func (c kafkaHeaders) Get(key string) string {
	for _, kv := range *c.h {
		if kv.Key == key {
			return string(kv.Value)
		}
	}
	return ""
}

func (c kafkaHeaders) Set(key, value string) {
	for i, kv := range *c.h {
		if kv.Key == key {
			(*c.h)[i].Value = []byte(value)
			return
		}
	}
	*c.h = append(*c.h, kafka.Header{Key: key, Value: []byte(value)})
}

func (c kafkaHeaders) Keys() []string {
	out := make([]string, 0, len(*c.h))
	for _, kv := range *c.h {
		out = append(out, kv.Key)
	}
	return out
}

// publishSpan rides along with the message in kafka.Message.WriterData so the
// writer's Completion callback can finish it.
//
// THE SPAN CANNOT END AT WriteMessages. The writer is Async on purpose — a seat
// claim must never wait on a broker — so WriteMessages returns as soon as the
// message is buffered, and at that moment NOBODY KNOWS WHICH PARTITION it will
// land on. The balancer chooses when the batch is built, and kafka-go fills
// Topic, Partition and Offset into every message just before calling Completion.
//
// Ending it there costs nothing and buys two things: the span covers the real
// send rather than the enqueue, and it can carry the partition — the attribute
// the entire hot-partition question is about, and the one SigNoz's Kafka view
// wants on a producer.
type publishSpan struct{ span trace.Span }

// startPublish opens the producer span and returns the headers to send with the
// message plus the handle to hang on it. finishPublish ends it.
func startPublish(ctx context.Context, topic, key string, size int) (context.Context, []kafka.Header, *publishSpan) {
	ctx, span := tracer.Start(ctx, topic+" publish",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.destination.name", topic),
			attribute.String("messaging.operation", "publish"),
			attribute.String("messaging.operation.name", "publish"),
			attribute.String("messaging.client_id", clientID),
			attribute.String("messaging.kafka.message.key", key),
			attribute.Int("messaging.message.body.size", size),
		))

	var headers []kafka.Header
	otel.GetTextMapPropagator().Inject(ctx, kafkaHeaders{&headers})
	return ctx, headers, &publishSpan{span: span}
}

// finishPublish ends the producer spans for one delivered batch, from the
// writer's Completion callback — the first moment the partition is known.
//
// err belongs to the batch, so it applies to every message in it.
func finishPublish(msgs []kafka.Message, err error) {
	for _, m := range msgs {
		ps, ok := m.WriterData.(*publishSpan)
		if !ok || ps == nil || ps.span == nil {
			continue
		}
		if err != nil {
			ps.span.RecordError(err)
			ps.span.SetStatus(codes.Error, "publish failed")
		} else {
			ps.span.SetAttributes(
				partitionID(m.Partition),
				attribute.Int64("messaging.kafka.message.offset", m.Offset),
			)
		}
		ps.span.End()
	}
}

// partitionID is a STRING, because the semantic convention says so.
//
// It was an attribute.Int here first, which put it in the numeric attribute map.
// SigNoz's Kafka view reads the string map, so the attribute was present and
// invisible at the same time and the page reported it missing — which is a much
// better failure than it sounds, because the page said exactly which attribute
// and exactly what was wrong with it.
func partitionID(p int) attribute.KeyValue {
	return attribute.String("messaging.destination.partition.id", strconv.Itoa(p))
}

// startConsume reopens the producer's trace on the other side of the broker and
// opens the consumer span underneath it.
//
// A CHILD, NOT A LINK. The semantic conventions allow either, and a link is the
// right call for a batch consumer that drains many unrelated messages at once.
// These consumers handle one message at a time and the work is a direct
// consequence of the publish, so a parent-child edge is both true and the thing
// that makes a purchase and the seat-map update it caused one trace.
func startConsume(ctx context.Context, m kafka.Message, group string) (context.Context, func()) {
	ctx = otel.GetTextMapPropagator().Extract(ctx, kafkaHeaders{&m.Headers})

	ctx, span := tracer.Start(ctx, m.Topic+" process",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.destination.name", m.Topic),
			attribute.String("messaging.operation", "process"),
			attribute.String("messaging.operation.name", "process"),
			attribute.String("messaging.client_id", clientID),
			attribute.String("messaging.kafka.message.key", string(m.Key)),
			partitionID(m.Partition),
			attribute.Int64("messaging.kafka.message.offset", m.Offset),
			attribute.String("messaging.consumer.group.name", group),
			attribute.String("messaging.kafka.consumer.group", group),
			attribute.Int("messaging.message.body.size", len(m.Value)),
		))
	return ctx, func() { span.End() }
}
