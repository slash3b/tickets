package catalog

// Seat pricing by section.
//
// EVERY BLOCK COSTING THE SAME WAS A PLACEHOLDER and DESIGN.md flagged it as an
// open question: "Do concert seats need price tiers that differ by section? Real
// arenas do." They do, and the answer turned out to matter for more than realism
// — a flat price makes every section equally attractive, so a rush spreads
// evenly and the contention is uniform. Real on-sales are not uniform: the floor
// goes first and fastest, which is where the interesting contention lives.
//
// The tiers are a function of position rather than a table, so a venue with a
// different number of blocks needs no new code.

// Tier is a price band within a venue.
type Tier struct {
	Name       string
	PriceMinor int64
}

// TierFor returns the band for the nth section of a venue with total sections.
//
// Front third is the floor, middle third the tiers, back third the gods — which
// is roughly how every arena on earth is laid out and, more importantly, gives
// three prices that differ enough to change behaviour.
func TierFor(index, total int, cinema bool) Tier {
	if cinema {
		// A single screen has one price. Splitting 96 seats into bands would be
		// theatre rather than modelling.
		return Tier{Name: "Stalls", PriceMinor: 1200}
	}
	if total <= 0 {
		total = 1
	}
	switch {
	case index*3 < total:
		return Tier{Name: "Floor", PriceMinor: 15000}
	case index*3 < total*2:
		return Tier{Name: "Tier", PriceMinor: 9500}
	default:
		return Tier{Name: "Upper", PriceMinor: 5500}
	}
}
