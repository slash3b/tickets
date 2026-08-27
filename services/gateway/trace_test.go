package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/slash3b/tickets/services/bank"
)

// TestPurchaseIsTraced is the test that would have caught the reason SigNoz sat
// empty: pkg/obs installed a TracerProvider and exporters correctly, and NOTHING
// IN THE SYSTEM EVER STARTED A SPAN. Everything was wired and nothing was
// instrumented, which looks identical to a broken collector from the UI.
//
// It asserts on span NAMES rather than counts, because the names are the contract
// a person reads in the trace view. If someone later swaps obs.Route back for
// mux.HandleFunc, or replaces obs.Pool with pgxpool.New, this fails and says so.
func TestPurchaseIsTraced(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	// THE PROPAGATOR IS NOT OPTIONAL, and forgetting it here is instructive: with
	// only a TracerProvider installed, every service still traces itself perfectly
	// and the bank's span lands in its OWN trace, because nothing writes the
	// traceparent header. obs.Setup installs this in the binaries; the test has to
	// mirror it or it is not testing what production does.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// A bank that always says yes: this test is about the shape of the trace, not
	// about payment outcomes, which are covered elsewhere.
	srv, cat, inv, _ := buildSystem(t, bank.Config{})
	eventID := seedShowing(t, cat, inv)

	var sections struct{ Sections []Section }
	get(t, srv, "/api/events/"+eventID.String()+"/sections", &sections)
	var seatmap struct{ Seats []Seat }
	get(t, srv, "/api/events/"+eventID.String()+"/sections/"+sections.Sections[0].ID.String(), &seatmap)

	var held struct {
		HoldID uuid.UUID `json:"hold_id"`
	}
	if code := post(t, srv, "/api/holds", holdRequest{
		EventID: eventID,
		SeatIDs: []uuid.UUID{seatmap.Seats[0].ID, seatmap.Seats[1].ID},
	}, &held); code != http.StatusCreated {
		t.Fatalf("POST /api/holds -> %d", code)
	}

	var placed struct {
		State string `json:"state"`
	}
	if code := post(t, srv, "/api/orders", orderRequest{
		HoldID: held.HoldID, EventID: eventID, UserID: uuid.New(), AmountMinor: 2400,
	}, &placed); code != http.StatusCreated {
		t.Fatalf("POST /api/orders -> %d", code)
	}
	if placed.State != "confirmed" {
		t.Fatalf("order state = %q, want confirmed", placed.State)
	}

	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var names []string
	for _, s := range rec.Ended() {
		names = append(names, s.Name())
	}

	// The span every reader looks for first, in the order a person expects to see
	// them nested.
	for _, want := range []string{
		"POST /api/holds",  // otelhttp server span, named after the ROUTE PATTERN
		"inventory.Hold",   // the contended core
		"POST /api/orders", //
		"saga.Run",         // the order state machine
		"saga.created",     // one span per saga step
	} {
		if !slicesContains(names, want) {
			t.Errorf("no span named %q\ngot: %v", want, names)
		}
	}

	// THE SEAT CLAIM ITSELF must be a span. This is the one query the entire
	// design exists to make safe, and a trace that does not show it is not worth
	// opening.
	var claim bool
	for _, n := range names {
		// otelpgx prefixes the statement with "query " or "prepare ", so this
		// matches on the statement rather than anchoring at the start.
		if strings.Contains(n, "UPDATE inventory.event_seats SET status = 'held'") {
			claim = true
		}
	}
	if !claim {
		t.Errorf("no span for the seat-claim UPDATE; obs.Pool lost its tracer\ngot: %v", names)
	}

	// CONTEXT MUST PROPAGATE WITHIN A REQUEST, and across the HTTP hop to the bank.
	//
	// Not "everything is one trace": the test's own client is a plain http.Client,
	// so each call it makes is correctly its own trace — exactly like a browser,
	// which is also not instrumented. What must hold is that everything the order
	// touched, INCLUDING the bank on the far side of a network call, shares the
	// trace of the request that caused it. That is the assertion that catches a
	// dropped ctx, which is the classic way traces turn into confetti.
	var orderTrace string
	for _, sp := range rec.Ended() {
		if sp.Name() == "POST /api/orders" {
			orderTrace = sp.SpanContext().TraceID().String()
		}
	}
	if orderTrace == "" {
		t.Fatal("no POST /api/orders span")
	}
	for _, want := range []string{"saga.Run", "saga.created", "saga.awaiting_payment", "POST /authorize"} {
		var found, joined bool
		for _, sp := range rec.Ended() {
			if sp.Name() != want {
				continue
			}
			found = true
			if sp.SpanContext().TraceID().String() == orderTrace {
				joined = true
			}
		}
		switch {
		case !found:
			t.Errorf("no span named %q", want)
		case !joined:
			t.Errorf("span %q is in a different trace from the order that caused it", want)
		}
	}

	traces := map[string]bool{}
	for _, sp := range rec.Ended() {
		traces[sp.SpanContext().TraceID().String()] = true
	}
	t.Logf("recorded %d spans across %d traces (one per uninstrumented client call)", len(names), len(traces))
}

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
