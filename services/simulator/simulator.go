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
}

// DefaultConfig is one showing a day's worth of traffic, not a load test.
//
// A system that is quietly busy is observable and debuggable. A system under
// permanent stress is neither, and it outruns anyone's ability to read the code
// that produced it. Load only goes up when someone turns it up on purpose.
func DefaultConfig() Config {
	return Config{
		ArrivalsPerMinute: 2,
		Mix: map[Profile]float64{
			ProfileBrowser:   0.55, // most people never buy
			ProfileDecisive:  0.20,
			ProfilePicky:     0.10,
			ProfileGroup:     0.10,
			ProfileAbandoner: 0.05,
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
	ctx, span := tracer.Start(ctx, "session "+string(profile),
		trace.WithAttributes(attribute.String("buyer.profile", string(profile))))
	defer span.End()

	events, err := s.listEvents(ctx)
	if err != nil || len(events) == 0 {
		if err != nil {
			s.Stats.Errors.Add(1)
		}
		return
	}
	event := events[s.intn(len(events))]

	sections, err := s.sections(ctx, event.ID)
	if err != nil || len(sections) == 0 {
		if err != nil {
			s.Stats.Errors.Add(1)
		}
		return
	}
	section := sections[s.intn(len(sections))]

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
	return free[start : start+want]
}
