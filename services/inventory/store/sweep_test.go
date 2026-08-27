package store

import (
	"context"
	"testing"
	"time"
)

// backdate makes a hold look older than it is, so expiry can be tested without
// sleeping through a real TTL.
func backdate(t *testing.T, s *Store, holdID any, expires, hard time.Duration) {
	t.Helper()
	if _, err := s.db.Exec(context.Background(),
		`UPDATE inventory.holds
		    SET expires_at = now() + $2::interval, hard_deadline = now() + $3::interval
		  WHERE id = $1`,
		holdID, expires.String(), hard.String()); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

func TestSweepExpiredReleasesActiveHolds(t *testing.T) {
	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, 2)
	ctx := context.Background()

	holdID, err := s.Hold(ctx, event, seats, time.Minute)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	backdate(t, s, holdID, -time.Second, time.Hour) // TTL passed, deadline far off

	n, err := s.SweepExpired(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Fatalf("swept %d seats, want 2", n)
	}

	available, err := s.CountByStatus(ctx, event, "available")
	if err != nil {
		t.Fatal(err)
	}
	if available != 2 {
		t.Fatalf("available = %d after sweep, want 2", available)
	}
	if err := s.CheckInvariants(ctx, event); err != nil {
		t.Fatalf("invariants violated: %v", err)
	}
}

// TestSweepExpiredIgnoresConvertingHolds is the test this whole design exists to
// pass. A hold whose payment is in flight must survive its short TTL — otherwise
// a slow bank costs a customer money for a seat that got sold to someone else.
func TestSweepExpiredIgnoresConvertingHolds(t *testing.T) {
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

	// TTL is long past. The hard deadline is not.
	backdate(t, s, holdID, -10*time.Minute, time.Hour)

	n, err := s.SweepExpired(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("sweep released %d seats from a CONVERTING hold — a slow bank would "+
			"now cost a customer money for seats sold to someone else", n)
	}

	held, err := s.CountByStatus(ctx, event, "held")
	if err != nil {
		t.Fatal(err)
	}
	if held != 2 {
		t.Fatalf("held = %d, want 2 — converting holds must survive the TTL sweep", held)
	}
}

func TestSweepHardDeadlineReleasesStuckConverting(t *testing.T) {
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
	backdate(t, s, holdID, -time.Hour, -time.Second) // both deadlines passed

	ids, err := s.SweepHardDeadline(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(ids) != 1 || ids[0] != holdID {
		t.Fatalf("returned %v, want exactly [%s] — these need reconciliation", ids, holdID)
	}

	available, err := s.CountByStatus(ctx, event, "available")
	if err != nil {
		t.Fatal(err)
	}
	if available != 2 {
		t.Fatalf("available = %d, want 2 — a hold cannot be immortal", available)
	}
	if err := s.CheckInvariants(ctx, event); err != nil {
		t.Fatalf("invariants violated: %v", err)
	}
}
