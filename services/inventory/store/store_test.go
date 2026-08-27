package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHoldClaimsSeats(t *testing.T) {
	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, 3)
	ctx := context.Background()

	if _, err := s.Hold(ctx, event, seats, time.Minute); err != nil {
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
	if _, err := s.Hold(ctx, event, seats[1:2], time.Minute); err != nil {
		t.Fatalf("first hold: %v", err)
	}

	_, err := s.Hold(ctx, event, seats, time.Minute)
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

	holdID, err := s.Hold(ctx, event, seats, time.Minute)
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
