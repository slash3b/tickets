package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/slash3b/tickets/services/bank"
	"github.com/slash3b/tickets/services/simulator"
)

// TestSimulatorAgreesWithTheBackend is the check DESIGN.md calls the most
// important alarm in the system.
//
// The simulator counts its own successful purchases. The backend counts confirmed
// orders and sold seats. Those numbers come from independent systems and MUST
// agree. When they diverge, either an oversell or a lost order has happened —
// and this comparison is where it becomes visible first, before any dashboard.
func TestSimulatorAgreesWithTheBackend(t *testing.T) {
	srv, cat, inv, pool := buildSystem(t, bank.Config{}) // bank behaves
	seedShowing(t, cat, inv)                             // 50 seats

	cfg := simulator.DefaultConfig()
	cfg.Mix = map[simulator.Profile]float64{simulator.ProfileDecisive: 1.0} // everyone buys
	sim := simulator.New(srv.URL, cfg)

	ctx := context.Background()
	const sessions = 25
	for range sessions {
		sim.RunOne(ctx, simulator.ProfileDecisive)
	}

	t.Logf("simulator: %s", sim.Stats.String())

	if sim.Stats.Errors.Load() != 0 {
		t.Fatalf("simulator saw %d errors", sim.Stats.Errors.Load())
	}
	bought := sim.Stats.Bought.Load()
	if bought == 0 {
		t.Fatal("simulator bought nothing; the run proves nothing")
	}

	// What the backend says.
	var confirmed, sold int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM orders.orders WHERE state = 'confirmed'`).Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM inventory.event_seats WHERE status = 'sold'`).Scan(&sold); err != nil {
		t.Fatal(err)
	}

	if confirmed != bought {
		t.Fatalf("DIVERGENCE: simulator bought %d, backend confirmed %d — an order "+
			"was lost or double-counted", bought, confirmed)
	}
	// One seat per decisive buyer.
	if sold != bought {
		t.Fatalf("DIVERGENCE: simulator bought %d, backend sold %d seats", bought, sold)
	}
}

// TestSimulatorProfilesExerciseTheRightPaths — a mix that never abandons or
// releases would leave the sweepers untested under real conditions.
func TestSimulatorProfilesExerciseTheRightPaths(t *testing.T) {
	srv, cat, inv, pool := buildSystem(t, bank.Config{})
	seedShowing(t, cat, inv)

	sim := simulator.New(srv.URL, simulator.DefaultConfig())
	ctx := context.Background()

	sim.RunOne(ctx, simulator.ProfileAbandoner)
	sim.RunOne(ctx, simulator.ProfilePicky)
	sim.RunOne(ctx, simulator.ProfileGroup)
	sim.RunOne(ctx, simulator.ProfileBrowser)

	t.Logf("simulator: %s", sim.Stats.String())

	if sim.Stats.Errors.Load() != 0 {
		t.Fatalf("errors: %d", sim.Stats.Errors.Load())
	}
	if sim.Stats.Abandoned.Load() != 1 {
		t.Fatalf("abandoned = %d, want 1 — nothing else exercises the expiry sweeper",
			sim.Stats.Abandoned.Load())
	}

	// Each profile leaves the seats in a different state, and that spread is the
	// point of having profiles at all:
	//
	//   abandoner  holds and vanishes      -> 1 seat HELD, waiting for the sweeper
	//   picky      holds then releases     -> 0 seats, given straight back
	//   group      holds 3 AND BUYS THEM   -> 3 seats SOLD
	//   browser    never holds anything    -> 0 seats
	var held, sold int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM inventory.event_seats WHERE status = 'held'`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM inventory.event_seats WHERE status = 'sold'`).Scan(&sold); err != nil {
		t.Fatal(err)
	}

	if held != 1 {
		t.Fatalf("held = %d, want 1 — only the abandoner's seat should still be "+
			"held, and it is the only thing that exercises the expiry sweeper", held)
	}
	if sold != 3 {
		t.Fatalf("sold = %d, want 3 — the group buyer takes three seats together", sold)
	}
	if sim.Stats.Bought.Load() != 1 {
		t.Fatalf("bought = %d, want 1 (the group)", sim.Stats.Bought.Load())
	}
}

// TestSimulatorUsesOnlyThePublicAPI is a guard on the design, not the code.
// The simulator is a CLIENT, not a peer — it gets no privileged path in. If this
// ever needs relaxing, the API is wrong, not the test.
func TestSimulatorUsesOnlyThePublicAPI(t *testing.T) {
	srv, cat, inv, _ := buildSystem(t, bank.Config{})
	seedShowing(t, cat, inv)

	// Constructed with nothing but a base URL — no stores, no database handle.
	sim := simulator.New(srv.URL, simulator.DefaultConfig())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sim.RunOne(ctx, simulator.ProfileDecisive)

	if sim.Stats.Bought.Load() != 1 {
		t.Fatalf("bought = %d, want 1 — a buyer with only HTTP must be able to buy",
			sim.Stats.Bought.Load())
	}
}
