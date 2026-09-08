package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/slash3b/tickets/services/bank"
)

// postKeyed is post() with an Idempotency-Key. Separate rather than another
// parameter on post(), because every existing caller would otherwise grow an
// empty string that says nothing.
func postKeyed(t *testing.T, srv *httptest.Server, path, key string, body, into any) int {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("build POST %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(IdempotencyHeader, key)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if into != nil {
		_ = json.NewDecoder(resp.Body).Decode(into)
	}
	return resp.StatusCode
}

// pickSeats browses to a section and returns the first n seat ids, the way a
// browser would.
func pickSeats(t *testing.T, srv *httptest.Server, eventID uuid.UUID, n int) []uuid.UUID {
	t.Helper()
	var sections struct{ Sections []Section }
	get(t, srv, "/api/events/"+eventID.String()+"/sections", &sections)
	if len(sections.Sections) == 0 {
		t.Fatal("no sections to pick from")
	}
	var seatmap struct{ Seats []Seat }
	get(t, srv, "/api/events/"+eventID.String()+"/sections/"+sections.Sections[0].ID.String(), &seatmap)
	if len(seatmap.Seats) < n {
		t.Fatalf("only %d seats available, want %d", len(seatmap.Seats), n)
	}
	out := make([]uuid.UUID, n)
	for i := range out {
		out[i] = seatmap.Seats[i].ID
	}
	return out
}

// TestRetriedHoldReturnsTheSameHold is the bug this endpoint had until the key
// existed.
//
// The retry is not hypothetical: the first response is lost to a timeout or a
// dropped connection and the client asks again. Without a key it is told 409 —
// that it lost a race it actually won — and the hold id it needed in order to
// release or buy those seats is gone, so they sit locked until the sweeper
// expires them.
func TestRetriedHoldReturnsTheSameHold(t *testing.T) {
	srv, cat, inv, _ := buildSystem(t, bank.Config{})
	eventID := seedShowing(t, cat, inv)
	picked := pickSeats(t, srv, eventID, 2)

	var first, second struct {
		HoldID    uuid.UUID `json:"hold_id"`
		ExpiresAt string    `json:"expires_at"`
	}

	if code := postKeyed(t, srv, "/api/holds", "checkout-1", holdRequest{
		EventID: eventID, SeatIDs: picked,
	}, &first); code != http.StatusCreated {
		t.Fatalf("first hold -> %d, want 201", code)
	}

	code := postKeyed(t, srv, "/api/holds", "checkout-1", holdRequest{
		EventID: eventID, SeatIDs: picked,
	}, &second)
	if code != http.StatusCreated {
		t.Fatalf("retried hold -> %d, want 201 — a retry with the same key must not "+
			"be told it lost the race it won", code)
	}
	if second.HoldID != first.HoldID {
		t.Fatalf("retried hold_id = %s, want the original %s", second.HoldID, first.HoldID)
	}
	// The original expiry, not a fresh one: a retry does not buy more time.
	if second.ExpiresAt != first.ExpiresAt {
		t.Fatalf("retried expires_at = %s, want the original %s", second.ExpiresAt, first.ExpiresAt)
	}
}

// TestHoldWithoutKeyStillConflicts: the key is opt-in, and nothing about the old
// behaviour changes for a client that does not send one.
func TestHoldWithoutKeyStillConflicts(t *testing.T) {
	srv, cat, inv, _ := buildSystem(t, bank.Config{})
	eventID := seedShowing(t, cat, inv)
	picked := pickSeats(t, srv, eventID, 1)

	if code := post(t, srv, "/api/holds", holdRequest{EventID: eventID, SeatIDs: picked}, nil); code != http.StatusCreated {
		t.Fatalf("first hold -> %d, want 201", code)
	}
	if code := post(t, srv, "/api/holds", holdRequest{EventID: eventID, SeatIDs: picked}, nil); code != http.StatusConflict {
		t.Fatalf("keyless retry -> %d, want 409", code)
	}
}

// TestDifferentKeysStillContend: the key must not become a way to take seats
// somebody else is already holding.
func TestDifferentKeysStillContend(t *testing.T) {
	srv, cat, inv, _ := buildSystem(t, bank.Config{})
	eventID := seedShowing(t, cat, inv)
	picked := pickSeats(t, srv, eventID, 1)

	if code := postKeyed(t, srv, "/api/holds", "buyer-a", holdRequest{
		EventID: eventID, SeatIDs: picked,
	}, nil); code != http.StatusCreated {
		t.Fatalf("first hold -> %d, want 201", code)
	}
	if code := postKeyed(t, srv, "/api/holds", "buyer-b", holdRequest{
		EventID: eventID, SeatIDs: picked,
	}, nil); code != http.StatusConflict {
		t.Fatalf("second buyer -> %d, want 409", code)
	}
}

// TestUnusableKeyIsRejected. Silently ignoring a key we cannot store would leave
// the client believing its retries are safe when they are not — which is worse
// than failing, because the whole value of the header is the promise it makes.
func TestUnusableKeyIsRejected(t *testing.T) {
	srv, cat, inv, _ := buildSystem(t, bank.Config{})
	eventID := seedShowing(t, cat, inv)
	picked := pickSeats(t, srv, eventID, 1)

	long := make([]byte, maxIdempotencyKey+1)
	for i := range long {
		long[i] = 'a'
	}

	// No newline case: net/http refuses to SEND a header value containing one, so
	// it cannot reach a handler over HTTP and a test for it would only be
	// exercising the client.
	for name, key := range map[string]string{
		"too long":        string(long),
		"comma":           "a,b",
		"whitespace":      "a b",
		"semicolon":       "a;b",
		"non-ascii":       "å",
		"percent-encoded": "a%20b",
	} {
		t.Run(name, func(t *testing.T) {
			var body struct{ Error string }
			code := postKeyed(t, srv, "/api/holds", key, holdRequest{
				EventID: eventID, SeatIDs: picked,
			}, &body)
			if code != http.StatusBadRequest {
				t.Fatalf("key %q -> %d, want 400", key, code)
			}
			if body.Error == "" {
				t.Fatal("400 carried no message saying what was wrong with the key")
			}
		})
	}

	// And nothing was claimed by any of those rejected requests.
	if code := post(t, srv, "/api/holds", holdRequest{EventID: eventID, SeatIDs: picked}, nil); code != http.StatusCreated {
		t.Fatalf("hold after rejected keys -> %d, want 201 — a 400 must not claim seats", code)
	}
}

// TestRetriedOrderReturnsTheSameOrder pins the idempotency orders ALREADY had,
// which is why no key column was added there.
//
// orders.hold_id is UNIQUE and the insert is an upsert on it, so the hold id is
// the natural key — a better one than a header, because a client cannot forget to
// send it or vary it between tries. This test is what stops that guarantee being
// removed by accident, since nothing in the orders code says out loud that the
// gateway depends on it.
func TestRetriedOrderReturnsTheSameOrder(t *testing.T) {
	srv, cat, inv, _ := buildSystem(t, bank.Config{})
	eventID := seedShowing(t, cat, inv)
	picked := pickSeats(t, srv, eventID, 2)

	var held struct {
		HoldID uuid.UUID `json:"hold_id"`
	}
	if code := postKeyed(t, srv, "/api/holds", "checkout-2", holdRequest{
		EventID: eventID, SeatIDs: picked,
	}, &held); code != http.StatusCreated {
		t.Fatalf("hold -> %d, want 201", code)
	}

	order := orderRequest{
		HoldID: held.HoldID, EventID: eventID, UserID: uuid.New(), AmountMinor: 2400,
	}

	var first, second struct {
		OrderID uuid.UUID `json:"order_id"`
		State   string    `json:"state"`
	}
	if code := postKeyed(t, srv, "/api/orders", held.HoldID.String(), order, &first); code != http.StatusCreated {
		t.Fatalf("first order -> %d, want 201", code)
	}
	if code := postKeyed(t, srv, "/api/orders", held.HoldID.String(), order, &second); code != http.StatusCreated {
		t.Fatalf("retried order -> %d, want 201", code)
	}
	if second.OrderID != first.OrderID {
		t.Fatalf("retried order_id = %s, want the original %s — a retry must not "+
			"start a second purchase for one hold", second.OrderID, first.OrderID)
	}
	if first.State != "confirmed" || second.State != "confirmed" {
		t.Fatalf("states = %q then %q, want confirmed both times", first.State, second.State)
	}
}
