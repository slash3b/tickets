package simulator

import (
	"math"
	"testing"
)

// TestDefaultMixPacesOneShowingPerDay guards the arithmetic in DefaultConfig.
//
// The first mix sold the day's 96 seats in about ninety minutes, and the symptom
// was mistaken for broken instrumentation twice: with nothing left to sell, holds,
// orders, payments and the bank emit nothing, and an idle half of a system looks
// exactly like an unreported one.
func TestDefaultMixPacesOneShowingPerDay(t *testing.T) {
	const seatsPerShowing = 96

	cfg := DefaultConfig()

	// A mix that does not sum to 1 silently biases toward browser, because
	// pickProfile falls through to it. That would slow sales without anyone
	// choosing to, so it is worth failing on.
	var total float64
	for _, share := range cfg.Mix {
		total += share
	}
	if math.Abs(total-1) > 1e-9 {
		t.Fatalf("mix sums to %v, want exactly 1 — the remainder silently becomes browser", total)
	}

	// ONLY THESE TWO CONSUME STOCK. picky releases its hold and abandoner lets it
	// expire, so both make load without ever reducing the seat count. Getting this
	// wrong is what produced a 90-minute sellout from a mix that looked moderate.
	seatsPerArrival := cfg.Mix[ProfileDecisive]*1 + cfg.Mix[ProfileGroup]*float64(cfg.GroupSize)
	perDay := seatsPerArrival * cfg.ArrivalsPerMinute * 60 * 24

	if perDay < seatsPerShowing*0.85 || perDay > seatsPerShowing*1.15 {
		t.Errorf("mix sells %.0f seats/day, want within 15%% of %d — the showing should last roughly a day",
			perDay, seatsPerShowing)
	}

	// The point of pacing by mix rather than by rate: the system must stay busy.
	sessionsPerDay := cfg.ArrivalsPerMinute * 60 * 24
	if sessionsPerDay < 1000 {
		t.Errorf("only %.0f sessions/day — pacing was done with the rate dial, which "+
			"leaves the system silent between arrivals", sessionsPerDay)
	}

	// Every profile has to actually happen often enough to exercise its path.
	// The abandoner is the only thing that ever drives the expiry sweeper under
	// real conditions, so a mix that rounds it away removes the sweeper's coverage.
	for _, p := range []Profile{ProfileDecisive, ProfilePicky, ProfileGroup, ProfileAbandoner} {
		if n := cfg.Mix[p] * sessionsPerDay; n < 10 {
			t.Errorf("profile %q runs %.1f times/day, too rare to exercise its path", p, n)
		}
	}

	t.Logf("%.0f sessions/day, %.1f seats/day (stock %d)", sessionsPerDay, perDay, seatsPerShowing)
	for _, p := range []Profile{ProfileBrowser, ProfileDecisive, ProfilePicky, ProfileGroup, ProfileAbandoner} {
		t.Logf("  %-10s %5.1f sessions/day", p, cfg.Mix[p]*sessionsPerDay)
	}
}
