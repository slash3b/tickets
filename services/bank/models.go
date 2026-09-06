package bank

import "time"

// Status is the verdict on a charge. There is no "pending": this bank either
// answers with a decision or does not answer at all, and the second case is a
// timeout rather than a state.
type Status string

const (
	StatusAuthorized Status = "authorized"
	StatusDeclined   Status = "declined"
)

// Charge is the record of one authorization attempt, keyed by IdempotencyKey.
// Once written it never changes — a repeated key returns this same value rather
// than producing a second verdict.
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

// authorizeRequest is the wire shape of POST /authorize. AmountMinor is in minor
// units (cents) because money is never a float.
type authorizeRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	AmountMinor    int64  `json:"amount_minor"`
}
