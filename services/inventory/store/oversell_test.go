//go:build oversell

// MANUALLY TRIGGERED ONLY.
//
//	go test -tags oversell ./services/inventory/...
//	go test -tags oversell -run TestNo -v ./services/inventory/store/
//
// The build tag means `go test ./...` does not even COMPILE this file, so it can
// never run by accident in CI or on a laptop. That is deliberate: it fires a
// thousand concurrent goroutines at a database and is a load test wearing a unit
// test's clothes. Run it when you have changed the claim primitive, and read the
// output rather than just the pass/fail.
package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestNoOversellUnderContention is the test milestone 1 exists to pass.
//
// 1000 goroutines, 10 seats, 100 contenders per seat. Exactly 10 may win. If 11
// ever win, the claim primitive is broken and every milestone built on top of it
// is built on sand.
func TestNoOversellUnderContention(t *testing.T) {
	const (
		seatCount = 10
		attempts  = 1000
	)

	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, seatCount)
	ctx := context.Background()

	var (
		won        atomic.Int64
		lost       atomic.Int64
		errored    atomic.Int64
		start      = make(chan struct{})
		wg         sync.WaitGroup
		errSamples = make(chan error, 16)
	)

	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Every goroutine targets ONE seat, round-robin, so each seat has
			// exactly 100 contenders racing for it.
			seat := []uuid.UUID{seats[i%seatCount]}

			<-start // release them all at once — a staggered start proves nothing

			switch _, err := s.Hold(ctx, event, seat, time.Minute); {
			case err == nil:
				won.Add(1)
			case errors.Is(err, ErrSeatsUnavailable):
				lost.Add(1)
			default:
				errored.Add(1)
				select {
				case errSamples <- err:
				default:
				}
			}
		}(i)
	}

	began := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(began)

	close(errSamples)
	for err := range errSamples {
		t.Logf("unexpected error: %v", err)
	}

	t.Logf("%d attempts on %d seats in %s (%.0f/sec) — won=%d lost=%d errored=%d",
		attempts, seatCount, elapsed.Round(time.Millisecond),
		float64(attempts)/elapsed.Seconds(), won.Load(), lost.Load(), errored.Load())

	if got := won.Load(); got != seatCount {
		t.Fatalf("OVERSELL OR UNDERSELL: %d holds succeeded for %d seats", got, seatCount)
	}
	if n := errored.Load(); n > 0 {
		t.Fatalf("%d attempts failed with an unexpected error — see samples above", n)
	}

	held, err := s.CountByStatus(ctx, event, "held")
	if err != nil {
		t.Fatal(err)
	}
	if held != seatCount {
		t.Fatalf("database says %d seats held, want %d", held, seatCount)
	}

	// The independent check. The counters above are what the code THINKS
	// happened; this asks the database directly.
	if err := s.CheckInvariants(ctx, event); err != nil {
		t.Fatalf("invariants violated: %v", err)
	}
}

// TestNoOversellWithOverlappingMultiSeat exercises the deadlock path.
//
// Overlapping multi-seat requests are what make Postgres take row locks in
// conflicting orders (SQLSTATE 40P01). The claim primitive sorts seat ids and
// retries victims; this test is what proves both actually work. A failure here
// shows up as errored > 0 rather than as an oversell.
func TestNoOversellWithOverlappingMultiSeat(t *testing.T) {
	const (
		seatCount = 12
		attempts  = 300
		groupSize = 3
	)

	s, event := newTestStore(t)
	seats := seedSeats(t, s, event, seatCount)
	ctx := context.Background()

	var won, lost, errored atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Sliding window of 3 adjacent seats — consecutive requests overlap in
			// two of three seats, which is the worst case for lock ordering. This
			// is also the realistic case: "three seats together".
			group := make([]uuid.UUID, groupSize)
			for j := range group {
				group[j] = seats[(i+j)%seatCount]
			}
			// Deliberately unsorted here — sorting is the primitive's job, and
			// handing it pre-sorted input would test nothing.

			<-start

			switch _, err := s.Hold(ctx, event, group, time.Minute); {
			case err == nil:
				won.Add(1)
			case errors.Is(err, ErrSeatsUnavailable):
				lost.Add(1)
			default:
				errored.Add(1)
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}

	close(start)
	wg.Wait()

	t.Logf("won=%d lost=%d errored=%d", won.Load(), lost.Load(), errored.Load())

	// At most 4 disjoint groups of 3 fit in 12 seats; overlap usually means fewer.
	if got := won.Load(); got > seatCount/groupSize {
		t.Fatalf("OVERSELL: %d groups of %d won from %d seats", got, groupSize, seatCount)
	}

	held, err := s.CountByStatus(ctx, event, "held")
	if err != nil {
		t.Fatal(err)
	}
	if int64(held) != won.Load()*groupSize {
		t.Fatalf("held = %d but %d groups won × %d seats = %d",
			held, won.Load(), groupSize, won.Load()*groupSize)
	}
	if err := s.CheckInvariants(ctx, event); err != nil {
		t.Fatalf("invariants violated: %v", err)
	}
}
