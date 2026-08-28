// Package rpc is the thin JSON-over-HTTP layer the services use to call each
// other.
//
// NOT gRPC, which is what DESIGN.md describes and still intends. That deviation
// is deliberate and worth stating: gRPC brings protobuf, a codegen step and a
// protoc toolchain in CI, and buys correspondingly little at four services on one
// LAN. Everything else here already speaks HTTP — the bank, the simulator, the
// public API — so this keeps one transport in the system instead of two. The
// callers are all behind consumer-declared interfaces, so replacing this with
// generated stubs later touches these files and nothing else.
//
// THE ONE THING THAT ACTUALLY MATTERS HERE IS ERRORS. In-process, `errors.Is(err,
// ErrSeatsUnavailable)` was how the gateway told "someone beat you to it" from
// "the server is broken", and that distinction is the difference between a 409 and
// a 500. HTTP has no errors.Is. So every failure crosses the wire as a STABLE
// STRING CODE in the body, and each client maps it back to the sentinel its
// callers already switch on. Without that, splitting these services would quietly
// turn every lost race into an internal server error.
package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/slash3b/tickets/pkg/obs"
)

// Error is a failure that carries the callee's own classification of it.
type Error struct {
	Status int    `json:"-"`
	Code   string `json:"code"`
	Msg    string `json:"error"`
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s (%d %s)", e.Msg, e.Status, e.Code)
	}
	return fmt.Sprintf("%s (%d)", e.Msg, e.Status)
}

// Client calls one other service.
type Client struct {
	base string
	http *http.Client
}

func New(baseURL string, timeout time.Duration) *Client {
	// obs.HTTPClient, so traceparent goes out on every call. Without it the split
	// would turn one trace into five unrelated ones — the exact failure that makes
	// a distributed system harder to debug than the monolith it replaced.
	return &Client{base: baseURL, http: obs.HTTPClient(timeout)}
}

// Do sends req (nil for none) and decodes into out (nil to discard).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode}
		// A callee that failed before it could produce JSON — a proxy, a crash —
		// still has to yield an Error rather than a decode failure, or the caller
		// cannot tell a 503 from a bug in this function.
		_ = json.NewDecoder(resp.Body).Decode(e)
		if e.Msg == "" {
			e.Msg = http.StatusText(resp.StatusCode)
		}
		return e
	}

	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

// CodeOf returns the callee's classification, or "" if this was not an rpc error.
// Clients use it to rebuild their own sentinels.
func CodeOf(err error) string {
	var e *Error
	if ok := asError(err, &e); ok {
		return e.Code
	}
	return ""
}

// Fail writes an error a caller can classify. Handlers use this instead of
// http.Error so the code survives the hop.
func Fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Error{Code: code, Msg: msg})
}

// OK writes a success body. A nil v means 204.
func OK(w http.ResponseWriter, status int, v any) {
	if v == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
