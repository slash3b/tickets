// Package simulator drives virtual buyers against the public API.
//
// It is a DEPLOYED SERVICE, not a test script, and it is a CLIENT, not a peer:
// it speaks the same HTTP the browser speaks and gets no privileged path into the
// system. If the simulator ever needs an internal API to work, the API is wrong.
//
// Its purpose is to keep the system permanently doing something real, so that
// leaks, drift and contention show up on their own instead of being hunted.
package simulator

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/slash3b/tickets/pkg/obs"
)

func newUUID() string { return uuid.NewString() }

// Profile is how one virtual buyer behaves. The mix matters more than the rate:
// a thousand buyers who all buy instantly exercise far less than a hundred who
// browse, hesitate, abandon and retry.
type Profile string

const (
	// ProfileBrowser opens a seat map and never buys. Pure read load, and the
	// majority of real traffic.
	ProfileBrowser Profile = "browser"
	// ProfileDecisive takes the first acceptable seat.
	ProfileDecisive Profile = "decisive"
	// ProfilePicky holds, releases, holds elsewhere. Generates hold churn, which
	// is the load nobody plans for.
	ProfilePicky Profile = "picky"
	// ProfileGroup wants several adjacent seats — the interesting contention, and
	// the case that makes seat ids collide.
	ProfileGroup Profile = "group"
	// ProfileAbandoner reaches checkout and vanishes, leaving a hold to expire.
	ProfileAbandoner Profile = "abandoner"
)

type Config struct {
	// ArrivalsPerMinute is the load dial. THE DEFAULT IS DELIBERATELY TINY.
	ArrivalsPerMinute float64             `json:"arrivals_per_minute"`
	Mix               map[Profile]float64 `json:"mix"`
	GroupSize         int                 `json:"group_size"`

	// TargetEventID pins every session to one event. Empty means wander, which is
	// what steady traffic should do.
	TargetEventID string `json:"target_event_id,omitempty"`

	// Rush makes every buyer go for the BEST seats instead of a random block.
	//
	// WITHOUT THIS A BURST IS NOT AN ON-SALE. The first run of 500 buyers at a
	// 20,000-seat arena produced zero lost races, because each one picked a random
	// section and a random block inside it — 500 darts at a very large board.
	// Real buyers all want the front of Block 1, which is what turns an on-sale
	// into a fight over a few hundred seats and is the only thing that exercises
	// the seat-claim primitive under pressure.
	Rush bool `json:"rush,omitempty"`
}

// DefaultConfig paces one showing's 96 seats across a whole DAY, while keeping
// the request rate high enough that the system is never silent.
//
// THOSE ARE TWO SEPARATE DIALS AND CONFLATING THEM WAS THE ORIGINAL MISTAKE. The
// first mix sold 0.5 seats per arrival, so at two arrivals a minute the day's
// showing was gone in about an hour and a half — after which the simulator could
// only browse, and holds, orders, payments and the bank emitted nothing for the
// other twenty-two hours. Half the system was invisible most of the day.
//
// The fix is NOT fewer arrivals. Pacing 96 seats over 24h purely by rate needs
// 0.13 arrivals a minute — one session every seven and a half minutes — which
// hits the target by making the whole thing quiet, which is the opposite of what
// the simulator is for. Instead the ARRIVAL RATE stays where it was and the MIX
// carries the pacing: far more lookers, far fewer buyers.
//
// THE ARITHMETIC, so this stays retunable. Only decisive and group consume a seat
// permanently — picky releases its hold and abandoner lets it expire, so both
// generate load without ever reducing stock:
//
//	seats per arrival = decisive*1 + group*3
//	                  = 0.015 + 0.006*3 = 0.033
//	seats per day     = 0.033 * 2 arrivals/min * 1440 min = 95
//
// which is one showing's 96 seats, give or take. Arrivals are Poisson, so some
// days sell out a little early and some end with seats unsold; a metronome would
// be less realistic and no more useful.
//
// A 93% look-to-buy ratio is not pessimism, it is roughly what real ticketing
// conversion looks like. It also keeps every profile properly exercised: at 2880
// sessions a day that is still ~86 picky sessions churning holds and ~58
// abandoners feeding the expiry sweeper, which nothing else exercises under real
// conditions.
//
// Load only goes up when someone turns it up on purpose.
func DefaultConfig() Config {
	return Config{
		ArrivalsPerMinute: 2,
		Mix: map[Profile]float64{
			ProfileBrowser:   0.929, // most people never buy, and it is not close
			ProfileDecisive:  0.015, // 1 seat
			ProfilePicky:     0.030, // holds then releases — churn, no stock consumed
			ProfileGroup:     0.006, // 3 seats, the interesting contention
			ProfileAbandoner: 0.020, // hold left to the sweeper, no stock consumed
		},
		GroupSize: 3,
	}
}

// Stats is the simulator's own view of what happened.
//
// THE NUMBER THAT MATTERS is Bought, compared against the backend's count of
// confirmed orders. Those two are produced by independent systems and must agree.
// When they diverge, either an oversell or a lost order has happened, and this is
// where it becomes visible first.
type Stats struct {
	Sessions  atomic.Int64
	Held      atomic.Int64
	Bought    atomic.Int64
	LostRace  atomic.Int64 // 409 — a normal outcome, not an error
	Abandoned atomic.Int64
	Errors    atomic.Int64
}

func (s *Stats) String() string {
	return fmt.Sprintf("sessions=%d held=%d bought=%d lost409=%d abandoned=%d errors=%d",
		s.Sessions.Load(), s.Held.Load(), s.Bought.Load(),
		s.LostRace.Load(), s.Abandoned.Load(), s.Errors.Load())
}

// One tracer for the package. otel.Tracer is cheap but not free, and naming it
// after the service is what groups these spans in the backend.
var tracer = otel.Tracer("simulator")

type Simulator struct {
	baseURL string
	http    *http.Client
	cfg     Config
	mu      sync.RWMutex
	Stats   Stats
	rnd     *rand.Rand
	rndMu   sync.Mutex
}

func New(baseURL string, cfg Config) *Simulator {
	return &Simulator{
		baseURL: baseURL,
		// Traced transport: every call a buyer makes carries traceparent, so the
		// gateway's spans and the bank's spans hang under the session span below
		// instead of appearing as unrelated traces.
		http: obs.HTTPClient(10 * time.Second),
		cfg:  cfg,
		rnd:  rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
	}
}

func (s *Simulator) SetConfig(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

// burstKey carries the rehearsal's span context so a session can LINK to it
// without becoming part of its trace.
type burstKey struct{}

func (s *Simulator) target() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.TargetEventID
}

func (s *Simulator) config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Simulator) float() float64 {
	s.rndMu.Lock()
	defer s.rndMu.Unlock()
	return s.rnd.Float64()
}

func (s *Simulator) intn(n int) int {
	s.rndMu.Lock()
	defer s.rndMu.Unlock()
	if n <= 0 {
		return 0
	}
	return s.rnd.IntN(n)
}

// Run spawns buyers until ctx is cancelled.
//
// Arrivals are a POISSON PROCESS, not a fixed interval: real buyers do not
// arrive on a metronome, and evenly spaced requests hide exactly the bunching
// that causes contention.
func (s *Simulator) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		rate := s.config().ArrivalsPerMinute
		if rate <= 0 {
			rate = 0.01 // never busy-loop
		}
		// Exponential inter-arrival time is what makes it Poisson.
		mean := 60.0 / rate
		wait := time.Duration(-math.Log(1-s.float()) * mean * float64(time.Second))

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			s.RunOne(ctx, s.pickProfile())
		}()
	}
}

func (s *Simulator) pickProfile() Profile {
	roll := s.float()
	var acc float64
	for p, share := range s.config().Mix {
		acc += share
		if roll <= acc {
			return p
		}
	}
	return ProfileBrowser
}

// RunOne plays a single buyer's session through the public API.
func (s *Simulator) RunOne(ctx context.Context, profile Profile) {
	s.Stats.Sessions.Add(1)

	// ONE SPAN PER SESSION, and it is the root of everything that follows.
	//
	// This is what makes the traces worth looking at: a trace is a whole customer
	// journey - browse, hold, pay - rather than eight disconnected HTTP calls. The
	// profile is an attribute, so "show me the group buyers who lost a race" is a
	// filter rather than an archaeology exercise.
	// A BURST MUST NOT PUT EVERY BUYER IN ONE TRACE. Passing the burst's context
	// straight down made each session a child of it: 12 buyers came out as a
	// single trace of 1,177 spans, and at 3,000 buyers that is a third of a
	// million spans in one trace — unopenable in any UI, and with no way to look
	// at what happened to ONE customer, which is the entire reason these spans
	// exist.
	//
	// Each session is its own trace, LINKED to the burst instead of parented by
	// it. A link says "these are related" without making them one tree, which is
	// exactly the relationship: the rehearsal caused the sessions, it does not
	// contain them.
	opts := []trace.SpanStartOption{
		trace.WithAttributes(attribute.String("buyer.profile", string(profile))),
	}
	if bs, ok := ctx.Value(burstKey{}).(trace.SpanContext); ok && bs.IsValid() {
		opts = append(opts, trace.WithNewRoot(), trace.WithLinks(trace.Link{SpanContext: bs}))
	}
	ctx, span := tracer.Start(ctx, "session "+string(profile), opts...)
	defer span.End()

	events, err := s.listEvents(ctx)
	if err != nil || len(events) == 0 {
		if err != nil {
			s.Stats.Errors.Add(1)
		}
		return
	}
	event := events[s.intn(len(events))]

	// An on-sale storms ONE event. Steady traffic wanders across whatever is
	// selling; a burst does not, and the difference is the entire experiment —
	// contention only happens when everybody wants the same thing at once.
	if target := s.target(); target != "" {
		var ok bool
		for _, e := range events {
			if e.ID == target {
				event, ok = e, true
				break
			}
		}
		if !ok {
			// The target is not on sale yet, or already gone. Not an error: during
			// an on-sale rehearsal most arrivals land before the doors open, and
			// counting those as failures would drown the real ones.
			return
		}
	}

	sections, err := s.sections(ctx, event.ID)
	if err != nil || len(sections) == 0 {
		if err != nil {
			s.Stats.Errors.Add(1)
		}
		return
	}
	section := sections[s.intn(len(sections))]
	if s.config().Rush {
		// Everybody piles into the same block, the way everybody wants the floor.
		section = sections[0]
	}

	seats, err := s.seats(ctx, event.ID, section.ID)
	if err != nil {
		s.Stats.Errors.Add(1)
		return
	}

	// A browser looked at the map and left. That is the whole session, and it is
	// the majority of real traffic.
	if profile == ProfileBrowser {
		return
	}

	want := 1
	if profile == ProfileGroup {
		want = s.config().GroupSize
	}
	picked := s.pickAvailable(seats, want)
	if len(picked) < want {
		return // the section filled up; nothing to do
	}

	holdID, err := s.hold(ctx, event.ID, picked)
	switch {
	case err == errLostRace:
		// Someone else got there first. Normal, and NOT an error — counting it as
		// one would make the error rate meaningless during an on-sale.
		s.Stats.LostRace.Add(1)
		return
	case err != nil:
		s.Stats.Errors.Add(1)
		return
	}
	s.Stats.Held.Add(1)

	// A picky buyer changes their mind, releasing the hold and leaving. This is
	// where hold churn comes from.
	if profile == ProfilePicky {
		if err := s.release(ctx, holdID); err != nil {
			s.Stats.Errors.Add(1)
		}
		return
	}

	// An abandoner simply stops. The hold is left to the expiry sweeper, which is
	// the only thing that ever exercises it under real conditions.
	if profile == ProfileAbandoner {
		s.Stats.Abandoned.Add(1)
		return
	}

	state, err := s.order(ctx, holdID, event.ID, int64(1200*len(picked)))
	if err != nil {
		s.Stats.Errors.Add(1)
		return
	}
	if state == "confirmed" {
		s.Stats.Bought.Add(1)
	}
}

func (s *Simulator) pickAvailable(seats []seat, want int) []string {
	var free []string
	for _, st := range seats {
		if st.Status == "available" {
			free = append(free, st.ID)
		}
	}
	if len(free) < want {
		return nil
	}
	// Adjacent, because "three seats together" is what people actually ask for
	// and it is what makes seat sets overlap.
	start := s.intn(len(free) - want + 1)
	if s.config().Rush {
		// THE BEST SEATS, not a random block. Every buyer converges on the same
		// front rows, so the seat sets in flight overlap heavily — which is the
		// whole point of an on-sale and the only way lock ordering gets tested.
		start = 0
	}
	return free[start : start+want]
}
