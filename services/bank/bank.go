// Package bank is a deliberately adversarial fake payment processor.
//
// It exists to make the rest of the system suffer. If it were easy to talk to,
// payments and orders could be written naively and would still pass their tests —
// and would then fall over the first time a real processor was slow.
//
// Its most valuable behaviour is the one that looks like a bug: TIMEOUT WITH
// SIDE EFFECT. A configurable fraction of requests record the charge and then
// never answer, so the caller sees a timeout for a payment that actually
// succeeded. That single case is what forces genuine idempotency rather than
// accidental idempotency, and it is why payments must send a stable key and
// reconcile afterwards.
package bank

import (
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/slash3b/tickets/pkg/obs"

	"go.uber.org/zap"
)

type Status string

const (
	StatusAuthorized Status = "authorized"
	StatusDeclined   Status = "declined"
)

type Charge struct {
	ID             string    `json:"id"`
	IdempotencyKey string    `json:"idempotency_key"`
	AmountMinor    int64     `json:"amount_minor"`
	Status         Status    `json:"status"`
	DeclineCode    string    `json:"decline_code,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Config is the chaos dial. Defaults are tame enough to develop against; turn
// them savage for an afternoon deliberately.
type Config struct {
	MinLatency  time.Duration `json:"min_latency"`
	MaxLatency  time.Duration `json:"max_latency"`
	DeclineRate float64       `json:"decline_rate"` // 0..1
	// TimeoutRate is the fraction of requests that SUCCEED SERVER-SIDE and then
	// never reply. Not "fail" — succeed, silently. This is the whole point.
	TimeoutRate float64 `json:"timeout_rate"`
	// Outage refuses everything, for watching backpressure.
	Outage bool `json:"outage"`
}

func DefaultConfig() Config {
	return Config{
		MinLatency:  20 * time.Millisecond,
		MaxLatency:  300 * time.Millisecond,
		DeclineRate: 0.05,
		TimeoutRate: 0.01,
	}
}

type Bank struct {
	mu sync.Mutex
	// byKey is what makes this bank behave like a real one: a repeated
	// idempotency key returns the ORIGINAL charge instead of creating a second.
	// Without it, a client that retries a timeout double-charges, and no amount
	// of care on the client side can prevent that.
	byKey  map[string]*Charge
	cfg    Config
	nextID int

	// rand is seeded per instance so tests can be deterministic by setting rates
	// to 0 or 1 rather than by controlling the seed.
	rnd *rand.Rand

	// lg may be nil in tests, which construct a Bank directly. Handler is the only
	// consumer and substitutes a no-op logger.
	lg *zap.Logger
}

func New(cfg Config) *Bank {
	return &Bank{
		byKey: make(map[string]*Charge),
		cfg:   cfg,
		rnd:   rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
	}
}

// WithLogger attaches the service logger. Separate from New so the many tests
// that build a Bank for its behaviour do not each have to invent a logger.
func (b *Bank) WithLogger(lg *zap.Logger) *Bank {
	b.lg = lg
	return b
}

func (b *Bank) SetConfig(cfg Config) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg = cfg
}

// Config reports the settings currently in force.
//
// THE OPERATOR PAGE NEEDS THIS TO STOP LYING. Its sliders were write-only: they
// rendered whatever the browser had last set, which after a reload is the
// hardcoded default, so the page could show a tame 5% while the bank was
// declining everything. A control that reports a number it never read is worse
// than no control.
func (b *Bank) Config() Config {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg
}

// ChargeCount reports how many distinct charges exist. Tests assert on this: a
// retried timeout must not increase it.
func (b *Bank) ChargeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.byKey)
}

type authorizeRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	AmountMinor    int64  `json:"amount_minor"`
}

// Handler returns the bank's HTTP surface.
func (b *Bank) Handler() http.Handler {
	mux := http.NewServeMux()
	// Only /authorize is traced. It is the call the payment saga actually makes,
	// and the one whose latency is the whole point of this service existing. The
	// config and lookup endpoints are operator tools, not part of any request path.
	lg := b.lg
	if lg == nil {
		lg = zap.NewNop()
	}
	obs.Route(mux, lg, "POST /authorize", b.authorize)
	mux.HandleFunc("PUT /config", b.setConfig)
	mux.HandleFunc("GET /config", b.getConfig)
	mux.HandleFunc("GET /charges/{key}", b.lookup)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (b *Bank) authorize(w http.ResponseWriter, r *http.Request) {
	var req authorizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IdempotencyKey == "" {
		http.Error(w, "idempotency_key and amount_minor are required", http.StatusBadRequest)
		return
	}

	b.mu.Lock()
	cfg := b.cfg
	if existing, ok := b.byKey[req.IdempotencyKey]; ok {
		b.mu.Unlock()
		// A repeat. Return the original verdict — do NOT roll the dice again, or
		// a retry could turn an authorization into a decline and the client would
		// have no way to know which answer was real.
		writeJSON(w, http.StatusOK, existing)
		return
	}
	b.mu.Unlock()

	if cfg.Outage {
		http.Error(w, "bank is down", http.StatusServiceUnavailable)
		return
	}

	b.sleep(cfg)

	charge := &Charge{
		ID:             b.newID(),
		IdempotencyKey: req.IdempotencyKey,
		AmountMinor:    req.AmountMinor,
		Status:         StatusAuthorized,
		CreatedAt:      time.Now(),
	}
	if b.roll() < cfg.DeclineRate {
		charge.Status = StatusDeclined
		charge.DeclineCode = "insufficient_funds"
	}

	// RECORD FIRST, THEN DECIDE WHETHER TO ANSWER. This ordering is the entire
	// point of the fake bank: the money moves and the caller is left holding a
	// timeout, exactly as a real processor behaves when a network drops after it
	// has committed.
	b.mu.Lock()
	b.byKey[req.IdempotencyKey] = charge
	b.mu.Unlock()

	if b.roll() < cfg.TimeoutRate {
		// Never reply. The caller's context deadline is what ends this.
		<-r.Context().Done()
		return
	}

	writeJSON(w, http.StatusOK, charge)
}

// lookup is how a client reconciles: "you timed out on me — what actually
// happened?" A real processor offers the same thing, and without it a timeout is
// genuinely unrecoverable.
func (b *Bank) lookup(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	charge, ok := b.byKey[r.PathValue("key")]
	b.mu.Unlock()

	if !ok {
		http.Error(w, "no such charge", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, charge)
}

func (b *Bank) getConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(b.Config())
}

// setConfig MERGES, and that is the whole point of starting from Config().
//
// It used to decode into a zero Config and replace everything. The operator page
// sends only decline_rate and timeout_rate, so every nudge of a slider silently
// set min_latency and max_latency to ZERO — deleting the 20-300ms the fake bank
// exists to simulate. Nothing failed; payments just quietly became instant, which
// is the least realistic thing this service can do and the hardest to notice.
//
// Decoding into the live config leaves absent fields exactly as they were.
func (b *Bank) setConfig(w http.ResponseWriter, r *http.Request) {
	cfg := b.Config()
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "bad config", http.StatusBadRequest)
		return
	}
	b.SetConfig(cfg)
	w.WriteHeader(http.StatusNoContent)
}

func (b *Bank) sleep(cfg Config) {
	if cfg.MaxLatency <= cfg.MinLatency {
		time.Sleep(cfg.MinLatency)
		return
	}
	span := cfg.MaxLatency - cfg.MinLatency
	time.Sleep(cfg.MinLatency + time.Duration(b.rndInt64(int64(span))))
}

func (b *Bank) roll() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rnd.Float64()
}

func (b *Bank) rndInt64(n int64) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 {
		return 0
	}
	return b.rnd.Int64N(n)
}

func (b *Bank) newID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	return "ch_" + time.Now().Format("20060102") + "_" + itoa(b.nextID)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
