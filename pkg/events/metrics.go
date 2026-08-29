package events

import (
	"context"
	"sync"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
)

// Consumer client metrics.
//
// WHY THIS IS HAND-ROLLED. SigNoz's Kafka view wants kafka.consumer.fetch_latency_avg,
// which everywhere else in the world comes from a JVM consumer's JMX MBean
// (kafka.consumer:type=consumer-fetch-manager-metrics -> fetch-latency-avg).
// Every consumer here is Go, using segmentio/kafka-go, which has no JMX and never
// will. Without this the panel is permanently empty for a reason that has nothing
// to do with the system being unhealthy.
//
// It is the same measurement, taken by the client instead of read off an MBean.
//
// THE FIELD IS WaitTime, NOT ReadTime, AND THE DIFFERENCE IS THE WHOLE METRIC.
// kafka-go splits one fetch into two timings: WaitTime is the round trip, from
// issuing the fetch to the broker answering, and ReadTime is the LOCAL work of
// decoding messages out of the batch that came back. ReadTime was the first
// choice here and reported 0.01ms — ten microseconds for a network call, which is
// the number telling you it is measuring the wrong thing. WaitTime is the
// analogue of the JMX metric.
//
// AN IDLE CONSUMER WILL SHOW ~MaxWait (250ms), because a fetch with no data to
// return blocks until the broker gives up. The JMX metric behaves the same way —
// fetch-latency-avg includes fetch.max.wait.ms — so this is faithful rather than
// broken. Read it as "how long fetches took", not "how loaded the broker is".
//
// Milliseconds, because that is the unit the JMX metric this stands in for uses,
// and a panel built for one and fed the other is worse than no panel.
//
// Stats() SNAPSHOTS AND RESETS, so the average covers the interval since the last
// collection rather than all time — which is what an "avg" gauge should show, and
// also why NOTHING ELSE MAY CALL Stats() on these readers. A second caller would
// silently halve everyone's numbers.

var (
	meter = otel.Meter("events")

	trackedMu sync.Mutex
	tracked   []*trackedReader

	registerOnce sync.Once
)

type trackedReader struct {
	r     *kafka.Reader
	topic string
	group string
}

// track registers a reader for metric collection and returns a function that
// unregisters it, to be called when the reader is closed.
func track(r *kafka.Reader, topic, group string, lg *zap.Logger) func() {
	tr := &trackedReader{r: r, topic: topic, group: group}

	trackedMu.Lock()
	tracked = append(tracked, tr)
	trackedMu.Unlock()

	registerOnce.Do(func() { registerFetchLatency(lg) })

	return func() {
		trackedMu.Lock()
		defer trackedMu.Unlock()
		for i, t := range tracked {
			if t == tr {
				tracked = append(tracked[:i], tracked[i+1:]...)
				return
			}
		}
	}
}

func registerFetchLatency(lg *zap.Logger) {
	avg, err := meter.Float64ObservableGauge("kafka.consumer.fetch_latency_avg",
		metric.WithDescription("Average time a fetch to the broker took, per topic"),
		metric.WithUnit("ms"))
	if err != nil {
		lg.Warn("could not create the fetch latency gauge", zap.Error(err))
		return
	}

	lag, err := meter.Int64ObservableGauge("kafka.consumer.records_lag",
		metric.WithDescription("Messages behind the log end, as the reader sees it"),
		metric.WithUnit("{message}"))
	if err != nil {
		lg.Warn("could not create the reader lag gauge", zap.Error(err))
		return
	}

	if _, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		trackedMu.Lock()
		snapshot := make([]*trackedReader, len(tracked))
		copy(snapshot, tracked)
		trackedMu.Unlock()

		for _, t := range snapshot {
			s := t.r.Stats()
			attrs := metric.WithAttributes(
				attribute.String("messaging.system", "kafka"),
				attribute.String("messaging.destination.name", t.topic),
				attribute.String("messaging.kafka.consumer.group", t.group),
				attribute.String("messaging.client_id", clientID),
			)
			// Only report when a fetch actually happened. A topic that saw none in
			// the interval would otherwise report 0ms and read as an impossibly
			// fast broker rather than as no data.
			if s.WaitTime.Count > 0 {
				o.ObserveFloat64(avg, float64(s.WaitTime.Avg.Microseconds())/1000, attrs)
			}
			o.ObserveInt64(lag, s.Lag, attrs)
		}
		return nil
	}, avg, lag); err != nil {
		lg.Warn("could not register the consumer metric callback", zap.Error(err))
	}
}
