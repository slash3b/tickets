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

func TestCommitSellsSeats(t *testing.T) {
	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, 2)
	ctx := context.Background()

	holdID, err := s.Hold(ctx, event, seats, time.Minute)
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

	holdID, _ := s.Hold(ctx, event, seats, time.Minute)
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

	holdID, _ := s.Hold(ctx, event, seats, time.Minute)
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
