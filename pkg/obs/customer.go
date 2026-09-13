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

// ProfileHeader says WHAT KIND of buyer this is — the simulator's browser, picky,
// group, decisive or abandoner. A real browser sends no such thing and the field
// is simply absent for it, which is the correct answer rather than a gap.
//
// IT USED TO BE READABLE ONLY BY PREFIX-MATCHING THE CUSTOMER ID, because the
// simulator encodes it as "sim-<profile>-<8 hex>". That worked right up until a
// profile name contains a dash, and it made "all picky buyers" a LIKE over a
// composite string instead of an equality on a field.
const ProfileHeader = "X-Buyer-Profile"

const (
	customerBaggageKey = "customer.id"
	profileBaggageKey  = "buyer.profile"
)

// maxTagValue bounds what a stranger can put in our telemetry. Baggage ends up on
// every span of every downstream service, so an unbounded header value is an
// unbounded write into the trace store.
const maxTagValue = 64

// clientTags are the header-borne labels that ride baggage across every hop. Both
// are optional, both are attacker-supplied at an unauthenticated door, and both
// go through the same sanitiser for that reason.
var clientTags = []struct {
	header     string
	baggageKey string
}{
	{CustomerHeader, customerBaggageKey},
	{ProfileHeader, profileBaggageKey},
}

// WithClientTags reads the headers, puts each value into baggage so it reaches
// every service, and tags the current span.
//
// It returns the context unchanged when a header is absent, which is the normal
// case for anything that has not been taught to send one.
func WithClientTags(ctx context.Context, r *http.Request) context.Context {
	for _, t := range clientTags {
		v := sanitiseTag(r.Header.Get(t.header))
		if v == "" {
			continue
		}

		trace.SpanFromContext(ctx).SetAttributes(attribute.String(t.baggageKey, v))

		m, err := baggage.NewMember(t.baggageKey, v)
		if err != nil {
			continue
		}
		b, err := baggage.FromContext(ctx).SetMember(m)
		if err != nil {
			continue
		}
		ctx = baggage.ContextWithBaggage(ctx, b)
	}
	return ctx
}

// CustomerFromContext returns the id carried in baggage, or "".
func CustomerFromContext(ctx context.Context) string {
	return baggage.FromContext(ctx).Member(customerBaggageKey).Value()
}

// ProfileFromContext returns the buyer profile carried in baggage, or "".
func ProfileFromContext(ctx context.Context) string {
	return baggage.FromContext(ctx).Member(profileBaggageKey).Value()
}

// TagClient puts every client tag on the current span. Downstream services call
// this so the tags are filterable on their spans too, not only the gateway's.
func TagClient(ctx context.Context) {
	span := trace.SpanFromContext(ctx)
	for _, t := range clientTags {
		if v := baggage.FromContext(ctx).Member(t.baggageKey).Value(); v != "" {
			span.SetAttributes(attribute.String(t.baggageKey, v))
		}
	}
}

// CustomerField returns a zap field, or a no-op when there is no customer.
func CustomerField(ctx context.Context) zap.Field {
	if id := CustomerFromContext(ctx); id != "" {
		return zap.String("customer_id", id)
	}
	return zap.Skip()
}

// ProfileField returns a zap field, or a no-op when there is no profile — which
// is every request from a real browser.
func ProfileField(ctx context.Context) zap.Field {
	if p := ProfileFromContext(ctx); p != "" {
		return zap.String("buyer_profile", p)
	}
	return zap.Skip()
}

// sanitiseTag keeps this from becoming an injection point.
//
// Baggage is propagated in an HTTP header, so a value containing a comma or a
// semicolon would corrupt the header for everything downstream — and the value
// comes from whoever is calling. Anything unexpected is dropped rather than
// escaped, because there is no legitimate id that needs those characters.
func sanitiseTag(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxTagValue {
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
