package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/slash3b/tickets/services/bank"
	catalogstore "github.com/slash3b/tickets/services/catalog/store"
	inventorystore "github.com/slash3b/tickets/services/inventory/store"
	ordersstore "github.com/slash3b/tickets/services/orders/store"
	paystore "github.com/slash3b/tickets/services/payments/store"
	"github.com/slash3b/tickets/services/payments/bankclient"
)

// paymentsAdapter is the bridge orders needs: charge an order, once.
type paymentsAdapter struct {
	store *paystore.Store
	bank  *bankclient.Client
}

func (p paymentsAdapter) Charge(ctx context.Context, orderID uuid.UUID, amountMinor int64) (ordersstore.PaymentOutcome, string, error) {
	pay, err := p.store.Create(ctx, orderID, amountMinor)
	if err != nil {
		return "", "", err
	}
	charge, err := p.bank.AuthorizeAndReconcile(ctx, pay.IdempotencyKey, amountMinor)
	switch {
	case err == nil:
		return ordersstore.PaymentSucceeded, "", p.store.Resolve(ctx, orderID, paystore.StateSucceeded, charge.ID, "")
	case charge != nil && charge.Status == "declined":
		return ordersstore.PaymentFailed, charge.DeclineCode,
			p.store.Resolve(ctx, orderID, paystore.StateFailed, "", charge.DeclineCode)
	default:
		return ordersstore.PaymentUnknown, "", p.store.MarkUnknown(ctx, orderID)
	}
}

// buildSystem wires every service together against one real database and one real
// (fake) bank, and returns the gateway's HTTP surface.
func buildSystem(t *testing.T, bankCfg bank.Config) (*httptest.Server, *catalogstore.Store, *inventorystore.Store) {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — run `make pg-up` first")
	}
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	for _, schema := range []string{
		catalogstore.SchemaSQL, inventorystore.SchemaSQL,
		ordersstore.SchemaSQL, paystore.SchemaSQL,
	} {
		if _, err := pool.Exec(ctx, schema); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	// Each test starts from nothing; the resumer and reconciler are global by
	// design and would otherwise act on other tests' rows.
	for _, q := range []string{
		`TRUNCATE catalog.venues CASCADE`, `TRUNCATE inventory.holds CASCADE`,
		`TRUNCATE inventory.event_seats`, `TRUNCATE orders.orders CASCADE`,
		`TRUNCATE payments.payments`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("truncate: %v", err)
		}
	}

	cat := catalogstore.New(pool)
	inv := inventorystore.New(pool)
	ord := ordersstore.New(pool)
	pay := paystore.New(pool)

	b := bank.New(bankCfg)
	bankSrv := httptest.NewServer(b.Handler())
	t.Cleanup(bankSrv.Close)
	client := bankclient.New(bankSrv.URL, 2*time.Second)

	saga := ordersstore.NewSaga(ord, InventoryAdapter{S: inv},
		paymentsAdapter{store: pay, bank: client})

	api := New(
		CatalogAdapter{S: cat},
		InventoryAdapter{S: inv},
		OrdersAdapter{S: ord, Saga: saga},
		5*time.Minute,
	)

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, cat, inv
}

// seedShowing creates a cinema, an event on sale now, and opens its seats.
func seedShowing(t *testing.T, cat *catalogstore.Store, inv *inventorystore.Store) (eventID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	venue, err := cat.CreateVenue(ctx, "Cineplex Screen 1", "cinema")
	if err != nil {
		t.Fatal(err)
	}
	sectionID, err := cat.AddSection(ctx, venue.ID, "Stalls", 5, 10) // 50 seats
	if err != nil {
		t.Fatal(err)
	}
	event, err := cat.CreateEvent(ctx, venue.ID, "Dune: Part Three",
		time.Now().Add(48*time.Hour), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.SetPrice(ctx, event.ID, sectionID, 1200); err != nil {
		t.Fatal(err)
	}

	// Catalog says which seats exist; INVENTORY opens them. Catalog never writes
	// seat status, even for the initial load.
	seatIDs, err := cat.SeatIDsForEvent(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	n, err := inv.OpenEvent(ctx, event.ID, seatIDs)
	if err != nil {
		t.Fatal(err)
	}
	if n != 50 {
		t.Fatalf("opened %d seats, want 50", n)
	}
	return event.ID
}

func get(t *testing.T, srv *httptest.Server, path string, into any) int {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if into != nil {
		_ = json.NewDecoder(resp.Body).Decode(into)
	}
	return resp.StatusCode
}

func post(t *testing.T, srv *httptest.Server, path string, body, into any) int {
	t.Helper()
	buf, _ := json.Marshal(body)
	resp, err := srv.Client().Post(srv.URL+path, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if into != nil {
		_ = json.NewDecoder(resp.Body).Decode(into)
	}
	return resp.StatusCode
}

// TestBuyATicketEndToEnd is the first time the whole system does its job:
// browse, pick seats, hold them, pay, and end up with seats that are sold.
func TestBuyATicketEndToEnd(t *testing.T) {
	srv, cat, inv := buildSystem(t, bank.Config{}) // bank behaves
	eventID := seedShowing(t, cat, inv)

	// Browse.
	var events struct{ Events []Event }
	if code := get(t, srv, "/api/events", &events); code != http.StatusOK {
		t.Fatalf("GET /api/events -> %d", code)
	}
	if len(events.Events) != 1 {
		t.Fatalf("listed %d events, want 1", len(events.Events))
	}

	// Sections, then one section's seats — never the whole event.
	var sections struct{ Sections []Section }
	get(t, srv, "/api/events/"+eventID.String()+"/sections", &sections)
	if len(sections.Sections) != 1 || sections.Sections[0].Seats != 50 {
		t.Fatalf("sections = %+v", sections.Sections)
	}

	var seatmap struct{ Seats []Seat }
	get(t, srv, "/api/events/"+eventID.String()+"/sections/"+sections.Sections[0].ID.String(), &seatmap)
	if len(seatmap.Seats) != 50 {
		t.Fatalf("got %d seats, want 50", len(seatmap.Seats))
	}
	if seatmap.Seats[0].Status != "available" {
		t.Fatalf("seat status = %q, want available", seatmap.Seats[0].Status)
	}

	// Hold three together.
	picked := []uuid.UUID{seatmap.Seats[0].ID, seatmap.Seats[1].ID, seatmap.Seats[2].ID}
	var held struct {
		HoldID uuid.UUID `json:"hold_id"`
	}
	if code := post(t, srv, "/api/holds", holdRequest{EventID: eventID, SeatIDs: picked}, &held); code != http.StatusCreated {
		t.Fatalf("POST /api/holds -> %d", code)
	}

	// The map must now show them held — the read model reflects inventory.
	get(t, srv, "/api/events/"+eventID.String()+"/sections/"+sections.Sections[0].ID.String(), &seatmap)
	if seatmap.Seats[0].Status != "held" {
		t.Fatalf("seat 0 status = %q after a hold, want held", seatmap.Seats[0].Status)
	}

	// Buy.
	var placed struct {
		OrderID uuid.UUID `json:"order_id"`
		State   string    `json:"state"`
	}
	if code := post(t, srv, "/api/orders", orderRequest{
		HoldID: held.HoldID, EventID: eventID, UserID: uuid.New(), AmountMinor: 3600,
	}, &placed); code != http.StatusCreated {
		t.Fatalf("POST /api/orders -> %d", code)
	}
	if placed.State != "confirmed" {
		t.Fatalf("order state = %q, want confirmed", placed.State)
	}

	// And the seats are sold.
	get(t, srv, "/api/events/"+eventID.String()+"/sections/"+sections.Sections[0].ID.String(), &seatmap)
	sold := 0
	for _, s := range seatmap.Seats {
		if s.Status == "sold" {
			sold++
		}
	}
	if sold != 3 {
		t.Fatalf("%d seats sold, want 3", sold)
	}
}

// TestLosingTheRaceIs409 — two buyers want the same seat. The loser must get a
// 409 they can act on, not a 500.
func TestLosingTheRaceIs409(t *testing.T) {
	srv, cat, inv := buildSystem(t, bank.Config{})
	eventID := seedShowing(t, cat, inv)

	var sections struct{ Sections []Section }
	get(t, srv, "/api/events/"+eventID.String()+"/sections", &sections)
	var seatmap struct{ Seats []Seat }
	get(t, srv, "/api/events/"+eventID.String()+"/sections/"+sections.Sections[0].ID.String(), &seatmap)

	seat := []uuid.UUID{seatmap.Seats[7].ID}
	if code := post(t, srv, "/api/holds", holdRequest{EventID: eventID, SeatIDs: seat}, nil); code != http.StatusCreated {
		t.Fatalf("first hold -> %d, want 201", code)
	}

	var errBody struct{ Error string }
	code := post(t, srv, "/api/holds", holdRequest{EventID: eventID, SeatIDs: seat}, &errBody)
	if code != http.StatusConflict {
		t.Fatalf("second hold -> %d, want 409 — losing a race is a normal outcome, "+
			"and the SPA has to tell them apart from a server fault", code)
	}
	if errBody.Error == "" {
		t.Fatal("409 carried no message for the user")
	}
}

// TestDeclinedPaymentFreesTheSeats — a declined card must not leave seats stuck.
func TestDeclinedPaymentFreesTheSeats(t *testing.T) {
	srv, cat, inv := buildSystem(t, bank.Config{DeclineRate: 1.0}) // always decline
	eventID := seedShowing(t, cat, inv)

	var sections struct{ Sections []Section }
	get(t, srv, "/api/events/"+eventID.String()+"/sections", &sections)
	var seatmap struct{ Seats []Seat }
	get(t, srv, "/api/events/"+eventID.String()+"/sections/"+sections.Sections[0].ID.String(), &seatmap)

	picked := []uuid.UUID{seatmap.Seats[0].ID}
	var held struct {
		HoldID uuid.UUID `json:"hold_id"`
	}
	post(t, srv, "/api/holds", holdRequest{EventID: eventID, SeatIDs: picked}, &held)

	var placed struct{ State string }
	post(t, srv, "/api/orders", orderRequest{
		HoldID: held.HoldID, EventID: eventID, UserID: uuid.New(), AmountMinor: 1200,
	}, &placed)
	if placed.State != "failed" {
		t.Fatalf("order state = %q, want failed", placed.State)
	}

	get(t, srv, "/api/events/"+eventID.String()+"/sections/"+sections.Sections[0].ID.String(), &seatmap)
	if seatmap.Seats[0].Status != "available" {
		t.Fatalf("seat status = %q after a declined payment, want available — "+
			"a decline must give the seats back", seatmap.Seats[0].Status)
	}
}
