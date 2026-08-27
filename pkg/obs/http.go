package obs

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/exaring/otelpgx"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// sqlSpanName collapses a statement to a single readable line.
//
// Cardinality is safe precisely because this system never builds SQL by string
// concatenation — the values are always $1, $2. If that ever changes, this
// becomes a cardinality bomb and the trimmer is the safer choice again.
func sqlSpanName(stmt string) string {
	const max = 72
	name := strings.Join(strings.Fields(stmt), " ")
	if len(name) > max {
		name = name[:max] + "..."
	}
	return name
}

// Route wraps one handler so it produces a server span.
//
// THE SPAN IS NAMED AFTER THE ROUTE PATTERN, NOT THE URL. "GET /api/events/{id}"
// is one span name; "GET /api/events/9f3a…" would be one span name PER EVENT, and
// a trace backend that has to group by name degrades into uselessness the moment
// ids leak into it. This is the single most common way to ruin a tracing setup.
//
// Wrapping per route rather than wrapping the whole mux is what makes that
// possible: net/http only knows which pattern matched AFTER routing, so a single
// outer wrapper would have to name the span before it could know. It also means
// the health endpoints are simply never wrapped — /livez and /readyz fire every
// few seconds forever and would otherwise be most of the traffic in here.
func Route(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	mux.Handle(pattern, otelhttp.NewHandler(h, pattern))
}

// HTTPClient is an http.Client that PROPAGATES trace context.
//
// Without this the gateway's span and the bank's span are two unrelated traces.
// The transport injects traceparent into every outgoing request, which is the
// entire mechanism by which a distributed trace becomes distributed.
func HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: otelhttp.NewTransport(http.DefaultTransport,
			// The default client span name is a bare "HTTP POST" for every
			// outbound call in the process. Method plus host says WHO was called
			// without putting ids from the path into the span name.
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return r.Method + " " + r.URL.Host
			}),
		),
	}
}

// Pool builds a pgx pool whose every query is a span.
//
// This is the one that makes this system interesting to look at: the conditional
// UPDATE that claims a seat shows up as its own timed span inside the request that
// ran it, so contention is visible as duration rather than inferred from a
// counter. Query parameters are deliberately NOT recorded — they are seat and hold
// ids, they would be high-cardinality noise on every span, and the row identity is
// already on the domain span above it.
func Pool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(
		// BOTH OPTIONS ARE REQUIRED, and that is not obvious. otelpgx only calls
		// the span-name function when the trim flag is set — WithSpanNameFunc on
		// its own is silently ignored and you get raw multi-line SQL as span
		// names, which is how this was first written.
		//
		// The two built-in behaviours are both unusable: the default names a span
		// after the entire statement, newlines and all, and the trimmer's default
		// names every read in the system "query SELECT". A one-line prefix is
		// recognisable AND low cardinality, the latter only because every
		// statement here is static and parameterised.
		otelpgx.WithTrimSQLInSpanName(),
		otelpgx.WithSpanNameFunc(sqlSpanName),

		// The SQL is already on the span as an attribute, and prepare spans repeat
		// the query span's name. Keeping the connection details off cuts noise
		// that says nothing in a single-database system.
		otelpgx.WithDisableConnectionDetailsInAttributes(),
	)
	return pgxpool.NewWithConfig(ctx, cfg)
}
