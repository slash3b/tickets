package simulator

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/slash3b/tickets/pkg/env"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// BurstResult is what one on-sale rehearsal did.
type BurstResult struct {
	EventID  string        `json:"event_id"`
	Buyers   int           `json:"buyers"`
	Over     string        `json:"over"`
	Took     string        `json:"took"`
	Sessions int64         `json:"sessions"`
	Held     int64         `json:"held"`
	Bought   int64         `json:"bought"`
	LostRace int64         `json:"lost_race_409"`
	Errors   int64         `json:"errors"`
	Duration time.Duration `json:"-"`
}

// Burst is the Lady Gaga test: N buyers at one event inside a short window.
//
// IT IS NOT "TURN THE ARRIVAL RATE UP". Steady load at a high rate spreads
// arrivals evenly and across every event on sale, which is the one thing that
// does NOT reproduce an on-sale. What makes an on-sale hard is that thousands of
// people want the SAME seats in the SAME few seconds, so this pins every buyer to
// one event and starts them all inside `over`.
//
// The mix is deliberately not the steady-state one either. Nobody who queued for
// a Lady Gaga on-sale is a browser: these are decisive and group buyers, which is
// what puts overlapping seat sets in flight and makes lock ordering matter.
func (s *Simulator) Burst(ctx context.Context, eventID string, buyers int, over time.Duration, groupShare float64) BurstResult {
	// One span for the whole rehearsal, so the entire event is a single thing to
	// look at afterwards rather than N unrelated traces.
	parent := ctx
	ctx, span := tracer.Start(ctx, "on-sale burst", trace.WithAttributes(
		attribute.String("event_id", eventID),
		attribute.Int("buyers", buyers),
		attribute.String("over", over.String()),
	))
	defer span.End()

	// Sessions run on a context that does NOT carry the burst span, so each buyer
	// gets its own trace, plus the burst's span context so each can link back.
	// See the note in RunOne for why this is not parent-child.
	sessionCtx := context.WithValue(parent, burstKey{}, span.SpanContext())

	before := snapshot(&s.Stats)
	start := time.Now()

	// Pin every session to the target for the duration, then put it back.
	prev := s.config()
	cfg := prev
	cfg.TargetEventID = eventID
	cfg.Rush = true
	s.SetConfig(cfg)
	defer s.SetConfig(prev)

	// BOUND THE IN-FLIGHT BUYERS. The first 3,000-buyer run OOMKilled this pod:
	// every concurrent buyer holds an HTTP request and a decoded seat map, and a
	// 2,000-seat section is not small. Three thousand at once is also a lie about
	// the world — a real on-sale is 3,000 separate browsers on 3,000 machines, not
	// one process pretending. Arrivals still spread across the window; what is
	// capped is how many are mid-purchase simultaneously, which is what costs
	// memory and is still far more contention than the system has ever seen.
	concurrency := s.burstConcurrency()
	sem := make(chan struct{}, concurrency)
	span.SetAttributes(attribute.Int("concurrency", concurrency))

	var wg sync.WaitGroup
	for i := range buyers {
		// Spread the starts across the window instead of releasing them all on one
		// instruction. A real on-sale is a stampede, not a barrier — and a barrier
		// would measure Go's scheduler more than it measures the system.
		delay := time.Duration(float64(over) * float64(i) / float64(buyers))

		profile := ProfileDecisive
		if s.float() < groupShare {
			profile = ProfileGroup
		}

		wg.Add(1)
		go func(delay time.Duration, profile Profile) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}

			select {
			case <-ctx.Done():
				return
			case sem <- struct{}{}:
			}
			defer func() { <-sem }()

			s.RunOne(sessionCtx, profile)
		}(delay, profile)
	}
	wg.Wait()

	took := time.Since(start)
	after := snapshot(&s.Stats)
	res := BurstResult{
		EventID:  eventID,
		Buyers:   buyers,
		Over:     over.String(),
		Took:     took.String(),
		Duration: took,
		Sessions: after.sessions - before.sessions,
		Held:     after.held - before.held,
		Bought:   after.bought - before.bought,
		LostRace: after.lost - before.lost,
		Errors:   after.errors - before.errors,
	}
	span.SetAttributes(
		attribute.Int64("bought", res.Bought),
		attribute.Int64("lost_race", res.LostRace),
		attribute.Int64("errors", res.Errors),
	)
	return res
}

// burstConcurrency is how many buyers may be mid-purchase at once.
//
// Not a config field: it is a property of how much memory this pod has, not of
// the experiment being run, and conflating the two is how a rehearsal ends up
// tuned to avoid a crash rather than to answer a question.
func (s *Simulator) burstConcurrency() int {
	if n, err := strconv.Atoi(env.Get("BURST_CONCURRENCY", "")); err == nil && n > 0 {
		return n
	}
	return 250
}

type counts struct{ sessions, held, bought, lost, errors int64 }

// snapshot reads the counters so a burst can report its OWN totals. The
// simulator keeps running steady traffic throughout, so absolute numbers would
// include arrivals that had nothing to do with the rehearsal.
func snapshot(s *Stats) counts {
	return counts{
		sessions: s.Sessions.Load(),
		held:     s.Held.Load(),
		bought:   s.Bought.Load(),
		lost:     s.LostRace.Load(),
		errors:   s.Errors.Load(),
	}
}
