package obs

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/exaring/otelpgx"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/zap"

	"github.com/slash3b/tickets/pkg/logger"
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
func Route(mux *http.ServeMux, lg *zap.Logger, pattern string, h http.HandlerFunc) {
	mux.Handle(pattern, otelhttp.NewHandler(access(lg, pattern, h), pattern))
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
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// POOL METRICS. DESIGN.md asks, as an open question, whether Go's pool is
	// enough at this scale or whether pgbouncer is needed. That is not answerable
	// by opinion — it is answerable by watching acquire wait time and how often
	// the pool is empty under load, which is what these record.
	if err := otelpgx.RecordStats(pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pool metrics: %w", err)
	}
	return pool, nil
}

// Access logs one line per request, correlated to the trace.
//
// THIS IS THE OTHER HALF OF THE INSTRUMENTATION, and it was missing for the same
// reason the spans were: pkg/logger built a careful correlation mechanism and the
// only caller was the hello canary. The gateway served every request in this
// system and logged nothing but "listening".
//
// It runs INSIDE otelhttp's handler, not outside it, so the context already
// carries the span — which is what puts a real TraceId on the OTLP record rather
// than a string in the body.
func access(lg *zap.Logger, route string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		h(rec, r)

		// LEVEL BY WHO IS AT FAULT, not by whether the response was a success.
		// A 409 means someone else took the seat first — that is this system
		// working exactly as designed, and logging it as an error would bury the
		// real ones. Only 5xx is ours.
		log := logger.Ctx(r.Context(), lg).Info
		if rec.status >= 500 {
			log = logger.Ctx(r.Context(), lg).Error
		}
		log("request",
			zap.String("route", route),
			zap.String("method", r.Method),
			zap.Int("status", rec.status),
			zap.Duration("took", time.Since(start)),
		)
	})
}

// statusRecorder remembers the status code, which net/http otherwise discards the
// moment it is written.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status, s.written = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

// Write covers handlers that never call WriteHeader — net/http implies 200 then,
// and without this the recorder would report 200 for a handler that wrote a body
// after an explicit WriteHeader we missed.
func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}
