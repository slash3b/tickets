package events

import (
	"context"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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

// startPublish opens the producer span and returns the headers to send with the
// message. Always call the returned end func.
//
// THE SPAN MEASURES THE ENQUEUE, NOT THE SEND, and that is not a mistake to fix
// later. The writer is Async on purpose (see NewPublisher): a seat claim must
// never wait on a broker. WriteMessages therefore returns as soon as the message
// is buffered, so this span is honestly named for what it covers — how long the
// request path paid for publishing, which is the number that matters here.
func startPublish(ctx context.Context, topic, key string, size int) (context.Context, []kafka.Header, func()) {
	ctx, span := tracer.Start(ctx, topic+" publish",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.destination.name", topic),
			attribute.String("messaging.operation", "publish"),
			attribute.String("messaging.operation.name", "publish"),
			attribute.String("messaging.kafka.message.key", key),
			attribute.Int("messaging.message.body.size", size),
		))

	var headers []kafka.Header
	otel.GetTextMapPropagator().Inject(ctx, kafkaHeaders{&headers})
	return ctx, headers, func() { span.End() }
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
			attribute.String("messaging.kafka.message.key", string(m.Key)),
			attribute.Int("messaging.destination.partition.id", m.Partition),
			attribute.Int("messaging.kafka.partition", m.Partition),
			attribute.Int64("messaging.kafka.message.offset", m.Offset),
			attribute.String("messaging.consumer.group.name", group),
			attribute.String("messaging.kafka.consumer.group", group),
			attribute.Int("messaging.message.body.size", len(m.Value)),
		))
	return ctx, func() { span.End() }
}
