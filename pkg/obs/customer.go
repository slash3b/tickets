package obs

import (
	"context"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Who is doing this.
//
// A trace answers "what happened in this one request". It cannot answer "show me
// everything that customer did", which is the question you actually have when
// somebody says their seat vanished — and that includes the person sitting in
// front of the seat map wondering what the system just did to them.
//
// THE ID TRAVELS AS BAGGAGE, NOT AS A PARAMETER. Baggage is part of the
// propagation this system already runs — the composite propagator in obs.Setup
// has carried it since the beginning — so it crosses every gRPC hop on its own.
// Threading a customer id through Hold, Convert, Charge and Commit would have
// meant changing four proto messages to carry something none of them need to do
// their job.

// CustomerHeader is what a client sends. Anything may send it; nothing has to.
const CustomerHeader = "X-Customer-Id"

const customerBaggageKey = "customer.id"

// maxCustomerID bounds what a stranger can put in our telemetry. Baggage ends up
// on every span of every downstream service, so an unbounded header value is an
// unbounded write into the trace store.
const maxCustomerID = 64

// WithCustomer reads the header, puts the id into baggage so it reaches every
// service, and tags the current span.
//
// It returns the context unchanged when there is no id, which is the normal case
// for anything that has not been taught to send one.
func WithCustomer(ctx context.Context, r *http.Request) context.Context {
	id := sanitiseCustomerID(r.Header.Get(CustomerHeader))
	if id == "" {
		return ctx
	}

	trace.SpanFromContext(ctx).SetAttributes(attribute.String(customerBaggageKey, id))

	m, err := baggage.NewMember(customerBaggageKey, id)
	if err != nil {
		return ctx
	}
	b, err := baggage.FromContext(ctx).SetMember(m)
	if err != nil {
		return ctx
	}
	return baggage.ContextWithBaggage(ctx, b)
}

// CustomerFromContext returns the id carried in baggage, or "".
func CustomerFromContext(ctx context.Context) string {
	return baggage.FromContext(ctx).Member(customerBaggageKey).Value()
}

// TagCustomer puts the id on the current span. Downstream services call this so
// the id is filterable on their spans too, not only the gateway's.
func TagCustomer(ctx context.Context) string {
	id := CustomerFromContext(ctx)
	if id != "" {
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(customerBaggageKey, id))
	}
	return id
}

// CustomerField returns a zap field, or a no-op when there is no customer.
func CustomerField(ctx context.Context) zap.Field {
	if id := CustomerFromContext(ctx); id != "" {
		return zap.String("customer_id", id)
	}
	return zap.Skip()
}

// sanitiseCustomerID keeps this from becoming an injection point.
//
// Baggage is propagated in an HTTP header, so a value containing a comma or a
// semicolon would corrupt the header for everything downstream — and the value
// comes from whoever is calling. Anything unexpected is dropped rather than
// escaped, because there is no legitimate id that needs those characters.
func sanitiseCustomerID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxCustomerID {
		return ""
	}
	for _, r := range s {
		ok := r == '-' || r == '_' || r == '.' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return ""
		}
	}
	return s
}
