package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHoldClaimsSeats(t *testing.T) {
	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, 3)
	ctx := context.Background()

	if _, _, err := s.Hold(ctx, event, seats, time.Minute, ""); err != nil {
		t.Fatalf("hold: %v", err)
	}

	held, err := s.CountByStatus(ctx, event, "held")
	if err != nil {
		t.Fatal(err)
	}
	if held != 3 {
		t.Fatalf("held = %d, want 3", held)
	}
	if err := s.CheckInvariants(ctx, event); err != nil {
		t.Fatalf("invariants violated: %v", err)
	}
}

func TestHoldIsAllOrNothing(t *testing.T) {
	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, 3)
	ctx := context.Background()

	// Take the middle seat, then ask for all three.
	if _, _, err := s.Hold(ctx, event, seats[1:2], time.Minute, ""); err != nil {
		t.Fatalf("first hold: %v", err)
	}

	_, _, err := s.Hold(ctx, event, seats, time.Minute, "")
	if !errors.Is(err, ErrSeatsUnavailable) {
		t.Fatalf("second hold err = %v, want ErrSeatsUnavailable", err)
	}

	// The failed request must have claimed NOTHING. Handing someone two of the
	// three seats they asked for is worse than handing them none.
	held, err := s.CountByStatus(ctx, event, "held")
	if err != nil {
		t.Fatal(err)
	}
	if held != 1 {
		t.Fatalf("held = %d after a failed all-or-nothing hold, want 1", held)
	}
}

func TestReleaseReturnsSeats(t *testing.T) {
	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, 2)
	ctx := context.Background()

	holdID, _, err := s.Hold(ctx, event, seats, time.Minute, "")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := s.Release(ctx, holdID, "test"); err != nil {
		t.Fatalf("release: %v", err)
	}

	available, err := s.CountByStatus(ctx, event, "available")
	if err != nil {
		t.Fatal(err)
	}
	if available != 2 {
		t.Fatalf("available = %d after release, want 2", available)
	}
	if err := s.CheckInvariants(ctx, event); err != nil {
		t.Fatalf("invariants violated: %v", err)
	}
}

func TestCommitSellsSeats(t *testing.T) {
	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, 2)
	ctx := context.Background()

	holdID, _, err := s.Hold(ctx, event, seats, time.Minute, "")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := s.Convert(ctx, holdID); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if err := s.Commit(ctx, holdID); err != nil {
		t.Fatalf("commit: %v", err)
	}

	sold, err := s.CountByStatus(ctx, event, "sold")
	if err != nil {
		t.Fatal(err)
	}
	if sold != 2 {
		t.Fatalf("sold = %d, want 2", sold)
	}
	if err := s.CheckInvariants(ctx, event); err != nil {
		t.Fatalf("invariants violated: %v", err)
	}
}

// TestCommitIsIdempotent — commit runs after money has moved, so the resumer will
// retry it after a crash. "Did my commit land before I died?" is a question the
// caller often cannot answer, so calling twice must be safe.
func TestCommitIsIdempotent(t *testing.T) {
	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, 1)
	ctx := context.Background()

	holdID, _, _ := s.Hold(ctx, event, seats, time.Minute, "")
	if err := s.Commit(ctx, holdID); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if err := s.Commit(ctx, holdID); err != nil {
		t.Fatalf("second commit must be a no-op, got: %v", err)
	}

	sold, _ := s.CountByStatus(ctx, event, "sold")
	if sold != 1 {
		t.Fatalf("sold = %d after two commits, want 1", sold)
	}
}

// TestCommitOnReleasedHoldSignalsRefund — the hard deadline released the seats
// while a payment was in flight. Committing must fail in a way that says "refund",
// not silently succeed or silently do nothing.
func TestCommitOnReleasedHoldSignalsRefund(t *testing.T) {
	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, 1)
	ctx := context.Background()

	holdID, _, _ := s.Hold(ctx, event, seats, time.Minute, "")
	if err := s.Release(ctx, holdID, "hard_deadline"); err != nil {
		t.Fatalf("release: %v", err)
	}

	err := s.Commit(ctx, holdID)
	if !errors.Is(err, ErrHoldReleased) {
		t.Fatalf("err = %v, want ErrHoldReleased — this is the refund path", err)
	}

	sold, _ := s.CountByStatus(ctx, event, "sold")
	if sold != 0 {
		t.Fatalf("sold = %d, want 0 — the seats were already gone", sold)
	}
}

// TestSeatIDsForHoldCarriesTheEvent guards a bug that reached the cluster.
//
// Seat change messages are fanned out to browsers BY EVENT ID. The first version
// of this function returned only the seats, so the sold and released messages
// went out with an empty event id and reached nobody — a seat would appear as
// held on everyone's map and never clear, which is worse than having no live
// updates at all.
func TestSeatIDsForHoldCarriesTheEvent(t *testing.T) {
	s, eventID := newTestStore(t)
	seats := seedSeats(t, s, eventID, 3)
	ctx := context.Background()

	holdID, _, err := s.Hold(ctx, eventID, seats[:2], time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}

	gotEvent, gotSeats, err := s.SeatIDsForHold(ctx, holdID)
	if err != nil {
		t.Fatal(err)
	}
	if gotEvent != eventID {
		t.Errorf("event = %s, want %s — an empty event id means the change reaches no browser",
			gotEvent, eventID)
	}
	if len(gotSeats) != 2 {
		t.Errorf("got %d seats, want 2", len(gotSeats))
	}

	// And it must survive the commit having consumed the hold, because that is
	// the ordering the server relies on: read first, then commit.
	if err := s.Commit(ctx, holdID); err != nil {
		t.Fatal(err)
	}
}

// TestHoldIsIdempotent is the whole point of the idempotency key.
//
// The failure it guards against is NOT a double booking — the claim statement
// already makes that impossible. It is the retry after a lost response, which
// without a key finds the seats held by the caller itself, reports
// ErrSeatsUnavailable, and strands the hold id so nothing can release the seats
// before the TTL.
func TestHoldIsIdempotent(t *testing.T) {
	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, 3)
	ctx := context.Background()

	first, firstExpiry, err := s.Hold(ctx, event, seats, time.Minute, "key-abc")
	if err != nil {
		t.Fatalf("first hold: %v", err)
	}

	// The retry. Same key, same seats, and it must NOT be told it lost the race.
	second, secondExpiry, err := s.Hold(ctx, event, seats, time.Minute, "key-abc")
	if err != nil {
		t.Fatalf("replayed hold: %v, want the original hold back", err)
	}
	if second != first {
		t.Fatalf("replayed hold = %s, want the original %s", second, first)
	}

	// THE ORIGINAL EXPIRY, not a fresh one. A retry arriving later does not buy
	// the caller more time, and returning now+ttl would quietly tell it that it did.
	if !secondExpiry.Equal(firstExpiry) {
		t.Fatalf("replayed expiry = %s, want the original %s", secondExpiry, firstExpiry)
	}

	// And exactly one set of seats was claimed, by exactly one hold.
	held, err := s.CountByStatus(ctx, event, "held")
	if err != nil {
		t.Fatal(err)
	}
	if held != 3 {
		t.Fatalf("held = %d after a hold and its replay, want 3", held)
	}
	if err := s.CheckInvariants(ctx, event); err != nil {
		t.Fatalf("invariants violated: %v", err)
	}
}

// TestHoldWithDifferentKeysStillContends: the key must not become a way to take
// seats somebody else is holding.
func TestHoldWithDifferentKeysStillContends(t *testing.T) {
	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, 2)
	ctx := context.Background()

	if _, _, err := s.Hold(ctx, event, seats, time.Minute, "key-one"); err != nil {
		t.Fatalf("first hold: %v", err)
	}
	_, _, err := s.Hold(ctx, event, seats, time.Minute, "key-two")
	if !errors.Is(err, ErrSeatsUnavailable) {
		t.Fatalf("second hold err = %v, want ErrSeatsUnavailable", err)
	}
}

// TestHoldKeylessIsUnchanged: an empty key must behave exactly as before, and
// several keyless holds must coexist.
//
// This is the NULLIF in holdOnce. Storing ” rather than NULL would let the
// partial unique index admit exactly ONE keyless hold and fail every other hold
// in the system with a duplicate key — which would be a total outage introduced
// by a feature that is supposed to be optional.
func TestHoldKeylessIsUnchanged(t *testing.T) {
	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, 4)
	ctx := context.Background()

	first, _, err := s.Hold(ctx, event, seats[:2], time.Minute, "")
	if err != nil {
		t.Fatalf("first keyless hold: %v", err)
	}
	second, _, err := s.Hold(ctx, event, seats[2:], time.Minute, "")
	if err != nil {
		t.Fatalf("second keyless hold: %v", err)
	}
	if first == second {
		t.Fatal("two keyless holds share an id; they must be independent")
	}
}

// TestHoldReplayAfterReleaseIsAConflict pins the one semantic that is a judgement
// call rather than a mechanism.
//
// A key names ONE hold, for good. Once that hold has ended there is nothing left
// to replay, and claiming fresh seats under the same key would turn it into a
// request id — the caller would end up with two holds it believes are one.
func TestHoldReplayAfterReleaseIsAConflict(t *testing.T) {
	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, 2)
	ctx := context.Background()

	holdID, _, err := s.Hold(ctx, event, seats, time.Minute, "key-spent")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := s.Release(ctx, holdID, "cancelled"); err != nil {
		t.Fatalf("release: %v", err)
	}

	// The seats are free again, so a FRESH key would succeed here. The spent one
	// must not.
	if _, _, err := s.Hold(ctx, event, seats, time.Minute, "key-spent"); !errors.Is(err, ErrSeatsUnavailable) {
		t.Fatalf("replay of a released hold err = %v, want ErrSeatsUnavailable", err)
	}
	if _, _, err := s.Hold(ctx, event, seats, time.Minute, "key-fresh"); err != nil {
		t.Fatalf("fresh key after release: %v, want success", err)
	}
}

// TestConcurrentSameKeyYieldsOneHold exercises the path the unique index exists
// for, which the sequential replay test never reaches.
//
// Two requests carrying the same key arrive at once. One inserts; the other
// blocks on the index until the first transaction resolves and then fails with
// 23505. That loser must NOT report a lost race — it must re-read and return the
// winner's hold, because from the caller's point of view this is one request.
func TestConcurrentSameKeyYieldsOneHold(t *testing.T) {
	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, 2)
	ctx := context.Background()

	const racers = 8
	type result struct {
		id  uuid.UUID
		err error
	}
	results := make(chan result, racers)

	start := make(chan struct{})
	for range racers {
		go func() {
			<-start
			id, _, err := s.Hold(ctx, event, seats, time.Minute, "one-key")
			results <- result{id, err}
		}()
	}
	close(start)

	ids := map[uuid.UUID]int{}
	for range racers {
		r := <-results
		if r.err != nil {
			t.Errorf("racer failed: %v — every caller with this key must get the one hold", r.err)
			continue
		}
		ids[r.id]++
	}
	if len(ids) != 1 {
		t.Fatalf("got %d distinct hold ids, want 1: %v", len(ids), ids)
	}

	// And the seats were claimed exactly once.
	held, err := s.CountByStatus(ctx, event, "held")
	if err != nil {
		t.Fatal(err)
	}
	if held != 2 {
		t.Fatalf("held = %d after %d concurrent same-key holds, want 2", held, racers)
	}
	if err := s.CheckInvariants(ctx, event); err != nil {
		t.Fatalf("invariants violated: %v", err)
	}
}
