// Package bankclient talks to the bank. It is the only thing in the system
// permitted to, and it is written on the assumption that the bank lies by
// omission: it may take the money and then say nothing at all.
package bankclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/slash3b/tickets/pkg/obs"
)

var (
	// ErrUnknown means the charge may or may not have happened. It is NOT a
	// failure — treating it as one is how you refuse a customer who has already
	// been charged. The only correct response is to reconcile.
	ErrUnknown  = errors.New("charge outcome unknown")
	ErrDeclined = errors.New("charge declined")
)

type Charge struct {
	ID             string `json:"id"`
	IdempotencyKey string `json:"idempotency_key"`
	AmountMinor    int64  `json:"amount_minor"`
	Status         string `json:"status"`
	DeclineCode    string `json:"decline_code,omitempty"`
}

type Client struct {
	base string
	http *http.Client
}

func New(baseURL string, timeout time.Duration) *Client {
	// obs.HTTPClient injects traceparent, so the bank's spans hang under the
	// order that called it instead of forming a second, orphaned trace.
	return &Client{base: baseURL, http: obs.HTTPClient(timeout)}
}

// Authorize charges once per key. Repeating a key never charges twice — that
// guarantee lives in the bank, which is exactly where it has to live: no amount
// of care on this side can make a non-idempotent processor safe to retry.
func (c *Client) Authorize(ctx context.Context, key string, amountMinor int64) (*Charge, error) {
	body, _ := json.Marshal(map[string]any{
		"idempotency_key": key,
		"amount_minor":    amountMinor,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/authorize", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// A timeout here says nothing about whether money moved. Wrapping it as
		// ErrUnknown rather than as a plain error is what stops a caller
		// concluding "failed" and refunding or re-charging.
		return nil, fmt.Errorf("%w: %v", ErrUnknown, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 5xx is also unknown: the bank may have committed before failing to
		// answer. Only an explicit decline is a definite "no".
		return nil, fmt.Errorf("%w: bank returned %d", ErrUnknown, resp.StatusCode)
	}

	var ch Charge
	if err := json.NewDecoder(resp.Body).Decode(&ch); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnknown, err)
	}
	if ch.Status == "declined" {
		return &ch, fmt.Errorf("%w: %s", ErrDeclined, ch.DeclineCode)
	}
	return &ch, nil
}

// Lookup asks what actually happened. This is the recovery path for ErrUnknown,
// and a payment system without it cannot survive its own timeouts.
func (c *Client) Lookup(ctx context.Context, key string) (*Charge, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/charges/"+key, nil)
	if err != nil {
		return nil, false, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil // definitively never happened
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("lookup returned %d", resp.StatusCode)
	}

	var ch Charge
	if err := json.NewDecoder(resp.Body).Decode(&ch); err != nil {
		return nil, false, err
	}
	return &ch, true, nil
}

// AuthorizeAndReconcile is the operation callers should actually use. It charges
// once, and if the answer is lost it finds out what happened rather than
// guessing. The key must be stable for the same logical payment — derive it from
// the order id, never from a random value or a clock.
func (c *Client) AuthorizeAndReconcile(ctx context.Context, key string, amountMinor int64) (*Charge, error) {
	ch, err := c.Authorize(ctx, key, amountMinor)
	if err == nil || errors.Is(err, ErrDeclined) {
		return ch, err
	}
	if !errors.Is(err, ErrUnknown) {
		return nil, err
	}

	// The outcome is unknown. Ask.
	lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	found, ok, lerr := c.Lookup(lookupCtx, key)
	if lerr != nil {
		return nil, fmt.Errorf("%w: and reconciliation failed: %v", ErrUnknown, lerr)
	}
	if !ok {
		return nil, fmt.Errorf("%w: bank has no record, safe to retry", ErrUnknown)
	}
	if found.Status == "declined" {
		return found, fmt.Errorf("%w: %s", ErrDeclined, found.DeclineCode)
	}
	return found, nil
}
