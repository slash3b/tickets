package store

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// points returns "attr=value,attr=value" for every data point of one metric.
//
// ONE PROVIDER FOR THE WHOLE TEST, and both sagas run before anything is
// collected. otel.SetMeterProvider delegates the package-level instruments to a
// real provider exactly ONCE — a second call is silently ignored, so splitting
// this into two tests gives the second one an empty reader and a failure that
// looks like missing instrumentation.
func points(t *testing.T, ms []metricdata.Metrics, name string) []string {
	t.Helper()
	enc := attribute.DefaultEncoder()
	var out []string
	for _, m := range ms {
		if m.Name != name {
			continue
		}
		switch d := m.Data.(type) {
		case metricdata.Sum[int64]:
			for _, p := range d.DataPoints {
				out = append(out, p.Attributes.Encoded(enc))
			}
		case metricdata.Histogram[float64]:
			for _, p := range d.DataPoints {
				out = append(out, p.Attributes.Encoded(enc))
			}
		}
	}
	return out
}

func has(got []string, want string) bool {
	for _, g := range got {
		if g == want {
			return true
		}
	}
	return false
}

// TestSagaMetricsDescribeTheOutcome guards three mistakes, two of which were
// shipped and only showed up when a real decline went through the cluster.
//
//  1. failed_at must name the STEP that ended the order. `failed` is a resting
//     place, not a step, and the first version recorded exactly that: every
//     failure reported failed_at=failed, which narrows nothing.
//
//  2. A declined charge must not be recorded as outcome=ok. A decline returns a
//     nil error on purpose — the call worked, the answer was no — so reading the
//     outcome from err alone labels every rejection a success and makes the one
//     comparison worth having, rejected against accepted latency, unaskable.
//
//  3. A confirmed order carries no failed_at at all. An attribute present on
//     every point with an empty value costs a time series and answers nothing.
func TestSagaMetricsDescribeTheOutcome(t *testing.T) {
	s := newTestStore(t)

	r := metric.NewManualReader()
	otel.SetMeterProvider(metric.NewMeterProvider(metric.WithReader(r)))

	declined := newOrder(t, s)
	if err := NewSaga(s, &fakeInventory{},
		&fakePayments{outcome: PaymentFailed, declineCode: "insufficient_funds"}).
		Run(context.Background(), declined.ID); err != nil {
		t.Fatalf("declined run: %v", err)
	}

	confirmed := newOrder(t, s)
	if err := NewSaga(s, &fakeInventory{}, &fakePayments{outcome: PaymentSucceeded}).
		Run(context.Background(), confirmed.ID); err != nil {
		t.Fatalf("confirmed run: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var ms []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		ms = append(ms, sm.Metrics...)
	}

	orders := points(t, ms, "tickets.orders")
	if !has(orders, "failed_at=charge,state=failed") {
		t.Errorf("tickets.orders = %v, want failed_at=charge,state=failed", orders)
	}
	if !has(orders, "state=confirmed") {
		t.Errorf("tickets.orders = %v, want a bare state=confirmed with no failed_at", orders)
	}

	steps := points(t, ms, "tickets.saga.step.duration")
	if !has(steps, "outcome=failed,step=charge") {
		t.Errorf("step duration = %v, want the declined charge recorded as failed", steps)
	}
	for _, step := range []string{"convert_hold", "charge", "commit"} {
		if !has(steps, "outcome=ok,step="+step) {
			t.Errorf("step duration = %v, missing a successful %s", steps, step)
		}
	}

	life := points(t, ms, "tickets.saga.duration")
	for _, want := range []string{"state=failed", "state=confirmed"} {
		if !has(life, want) {
			t.Errorf("order lifetime = %v, missing %s", life, want)
		}
	}
}
