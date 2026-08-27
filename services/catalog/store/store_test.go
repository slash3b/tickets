package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)


func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — run `make pg-up` first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, SchemaSQL); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE catalog.venues CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return New(pool)
}

func TestSeedACinemaAndListIt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	venue, err := s.CreateVenue(ctx, "Cineplex Screen 1", "cinema")
	if err != nil {
		t.Fatal(err)
	}
	// 8 rows of 12 — a realistic small screen, 96 seats.
	sectionID, err := s.AddSection(ctx, venue.ID, "Stalls", 8, 12)
	if err != nil {
		t.Fatal(err)
	}

	event, err := s.CreateEvent(ctx, venue.ID, "Dune: Part Three",
		time.Now().Add(48*time.Hour), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrice(ctx, event.ID, sectionID, 1200); err != nil {
		t.Fatal(err)
	}

	events, err := s.ListOnSale(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Title != "Dune: Part Three" {
		t.Fatalf("on sale = %+v, want the one event", events)
	}
	if events[0].VenueName != "Cineplex Screen 1" {
		t.Fatalf("venue name = %q", events[0].VenueName)
	}

	sections, err := s.Sections(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0].Seats != 96 {
		t.Fatalf("sections = %+v, want one with 96 seats", sections[0])
	}

	seats, err := s.SectionSeats(ctx, sectionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seats) != 96 {
		t.Fatalf("seats = %d, want 96", len(seats))
	}
	if seats[0].RowLabel != "A" || seats[0].Number != 1 {
		t.Fatalf("first seat = %s%d, want A1", seats[0].RowLabel, seats[0].Number)
	}
}

// TestEventNotYetOnSaleIsHidden — on_sale_at is the whole concert case, so it had
// better actually filter.
func TestEventNotYetOnSaleIsHidden(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	venue, _ := s.CreateVenue(ctx, "Arena", "arena")
	if _, err := s.CreateEvent(ctx, venue.ID, "Lady Gaga",
		time.Now().Add(30*24*time.Hour), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	events, err := s.ListOnSale(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("returned %d events; one that is not on sale yet must not be listed", len(events))
	}
}
