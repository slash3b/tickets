package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/slash3b/tickets/services/bank"
)

// TestNothingSellsBeforeOnSale is milestone 8's whole guarantee.
//
// THE ENFORCEMENT IS STRUCTURAL, NOT A CHECK. Nothing in the request path reads
// on_sale_at and compares it to the clock. Instead inventory simply has no rows
// for an event until its seats are opened, so a hold fails the way it fails for
// any seat that does not exist. That removes the window between reading the clock
// and taking the seat, which is precisely the kind of gap this system spends its
// time closing everywhere else.
func TestNothingSellsBeforeOnSale(t *testing.T) {
	srv, cat, inv, _ := buildSystem(t, bank.Config{})
	ctx := t.Context()

	venue, err := cat.CreateVenue(ctx, "Test Arena", "arena")
	if err != nil {
		t.Fatal(err)
	}
	sectionID, err := cat.AddSection(ctx, venue.ID, "Block 1", 4, 5)
	if err != nil {
		t.Fatal(err)
	}

	// On sale in an hour: the concert case, and the first time this system has an
	// event that exists but cannot be bought.
	onSaleAt := time.Now().Add(time.Hour)
	event, err := cat.CreateEvent(ctx, venue.ID, "Not Yet", time.Now().Add(48*time.Hour), onSaleAt)
	if err != nil {
		t.Fatal(err)
	}

	// It must NOT appear as buyable...
	var onSale struct{ Events []Event }
	get(t, srv, "/api/events", &onSale)
	for _, e := range onSale.Events {
		if e.ID == event.ID {
			t.Fatalf("an event that is not on sale yet appeared in /api/events")
		}
	}

	// ...but it must be visible, or nobody can be told a concert is coming.
	var upcoming struct{ Events []Event }
	if code := get(t, srv, "/api/events/upcoming", &upcoming); code != http.StatusOK {
		t.Fatalf("GET /api/events/upcoming -> %d", code)
	}
	var found bool
	for _, e := range upcoming.Events {
		if e.ID == event.ID {
			found = true
			if e.OnSaleAt.IsZero() {
				t.Error("upcoming event carries no on_sale_at, so nothing can say when it opens")
			}
		}
	}
	if !found {
		t.Fatalf("event missing from /api/events/upcoming")
	}

	// Seats exist in the catalog, and holding one must still fail, because
	// inventory has never been told about them.
	seats, err := cat.SectionSeats(ctx, sectionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seats) != 20 {
		t.Fatalf("catalog has %d seats, want 20", len(seats))
	}
	code := post(t, srv, "/api/holds", holdRequest{
		EventID: event.ID, SeatIDs: []uuid.UUID{seats[0].ID},
	}, nil)
	if code == http.StatusCreated {
		t.Fatalf("held a seat before the sale opened")
	}

	// The on-sale itself: exactly what the workers loop does, which is BOTH the
	// inventory rows and the catalog flag that makes the event listable.
	seatIDs := make([]uuid.UUID, len(seats))
	for i, s := range seats {
		seatIDs[i] = s.ID
	}
	if _, err := inv.OpenEvent(ctx, event.ID, seatIDs); err != nil {
		t.Fatal(err)
	}
	if err := cat.MarkSeatsOpened(ctx, event.ID); err != nil {
		t.Fatal(err)
	}

	// And now the same request succeeds, with nothing else having changed.
	if code := post(t, srv, "/api/holds", holdRequest{
		EventID: event.ID, SeatIDs: []uuid.UUID{seats[0].ID},
	}, nil); code != http.StatusCreated {
		t.Fatalf("POST /api/holds after on-sale -> %d, want 201", code)
	}
}

// TestArenaRowLabelsSurvivePastZ guards a bug that a cinema could never expose.
//
// The seat generator used chr(64 + r), which is correct for eight rows and
// silently wrong for fifty: row 27 came out "[" and row 40 "h". Nobody would have
// noticed until an arena existed.
func TestArenaRowLabelsSurvivePastZ(t *testing.T) {
	_, cat, _, _ := buildSystem(t, bank.Config{})
	ctx := t.Context()

	venue, err := cat.CreateVenue(ctx, "Label Arena", "arena")
	if err != nil {
		t.Fatal(err)
	}
	sectionID, err := cat.AddSection(ctx, venue.ID, "Block 1", 53, 1)
	if err != nil {
		t.Fatal(err)
	}
	seats, err := cat.SectionSeats(ctx, sectionID)
	if err != nil {
		t.Fatal(err)
	}

	labels := map[string]bool{}
	for _, s := range seats {
		labels[s.RowLabel] = true
		for _, c := range s.RowLabel {
			if c < 'A' || c > 'Z' {
				t.Fatalf("row label %q contains a non-letter", s.RowLabel)
			}
		}
	}
	for _, want := range []string{"A", "Z", "AA", "AZ", "BA"} {
		if !labels[want] {
			t.Errorf("no row labelled %q; got %d distinct labels", want, len(labels))
		}
	}
}
