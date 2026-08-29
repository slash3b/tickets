package catalog

import "testing"

func TestTiersDifferAndDescend(t *testing.T) {
	const blocks = 10

	var prices []int64
	seen := map[string]bool{}
	for i := range blocks {
		tier := TierFor(i, blocks, false)
		prices = append(prices, tier.PriceMinor)
		seen[tier.Name] = true
	}

	// Three bands, or the tiering is not doing anything.
	if len(seen) != 3 {
		t.Errorf("got %d distinct tiers across %d blocks, want 3: %v", len(seen), blocks, seen)
	}

	// NEVER ASCENDING. A cheaper seat nearer the stage would be a bug that only
	// shows up as a strange-looking seat map, which is the kind nobody reports.
	for i := 1; i < len(prices); i++ {
		if prices[i] > prices[i-1] {
			t.Errorf("block %d costs more than block %d (%d > %d) — prices must not rise with distance",
				i+1, i, prices[i], prices[i-1])
		}
	}

	// A cinema is one price, whatever index it is asked about.
	if a, b := TierFor(0, 1, true), TierFor(5, 10, true); a != b {
		t.Errorf("cinema pricing varies by section: %+v vs %+v", a, b)
	}
}
