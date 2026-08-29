package gateway

import "testing"

// TestResolveLayout covers the venue presets and the custom overrides.
//
// THE NAMING RULE IS THE POINT OF MOST OF THESE CASES. Venues are found by name
// and built only when missing, so two different shapes must not share a default
// name — otherwise the second operator to ask for a custom room silently gets the
// first one's seating chart and no error anywhere says so.
func TestResolveLayout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		req       createShowingRequest
		wantName  string
		wantKind  string
		wantSeats int
		wantErr   bool
	}{
		{name: "empty means arena", req: createShowingRequest{},
			wantName: "Homelab Arena", wantKind: "arena", wantSeats: 20000},
		{name: "arena preset", req: createShowingRequest{Venue: "arena"},
			wantName: "Homelab Arena", wantKind: "arena", wantSeats: 20000},
		{name: "cinema preset", req: createShowingRequest{Venue: "cinema"},
			wantName: "Cineplex Screen 1", wantKind: "cinema", wantSeats: 96},
		{name: "unknown venue is rejected, not guessed",
			req: createShowingRequest{Venue: "stadium"}, wantErr: true},

		{name: "custom with no overrides is the arena shape under its own name",
			req:      createShowingRequest{Venue: "custom"},
			wantName: "Custom 10x50x40", wantKind: "arena", wantSeats: 20000},
		{name: "a zero field takes the preset value",
			req:      createShowingRequest{Venue: "custom", Rows: 10},
			wantName: "Custom 10x10x40", wantKind: "arena", wantSeats: 4000},
		{name: "one section is a screen, not an arena",
			req:      createShowingRequest{Venue: "custom", Sections: 1, Rows: 20, SeatsPerRow: 30},
			wantName: "Custom 1x20x30", wantKind: "cinema", wantSeats: 600},
		{name: "an explicit name wins over the generated one",
			req:      createShowingRequest{Venue: "custom", VenueName: "  The Old Vic  ", Sections: 2, Rows: 10, SeatsPerRow: 10},
			wantName: "The Old Vic", wantKind: "arena", wantSeats: 200},

		{name: "different shapes get different default names — the trap this rule exists for",
			req:      createShowingRequest{Venue: "custom", Sections: 3, Rows: 3, SeatsPerRow: 3},
			wantName: "Custom 3x3x3", wantKind: "arena", wantSeats: 27},

		{name: "too many sections", req: createShowingRequest{Venue: "custom", Sections: maxSections + 1}, wantErr: true},
		{name: "too many rows", req: createShowingRequest{Venue: "custom", Rows: maxRows + 1}, wantErr: true},
		{name: "row too wide", req: createShowingRequest{Venue: "custom", SeatsPerRow: maxPerRow + 1}, wantErr: true},
		{name: "under every individual cap but far too many seats",
			req:     createShowingRequest{Venue: "custom", Sections: 40, Rows: 500, SeatsPerRow: 100},
			wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolve(tc.req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolve(%+v) = %+v, want an error", tc.req, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.name != tc.wantName {
				t.Errorf("name = %q, want %q", got.name, tc.wantName)
			}
			if got.kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", got.kind, tc.wantKind)
			}
			if got.seats() != tc.wantSeats {
				t.Errorf("seats = %d, want %d", got.seats(), tc.wantSeats)
			}
		})
	}
}

// TestDefaultNamesAreUniquePerShape states the invariant directly rather than
// leaving it implied by the cases above.
func TestDefaultNamesAreUniquePerShape(t *testing.T) {
	seen := map[string]string{}
	for _, r := range []createShowingRequest{
		{Venue: "custom", Sections: 1, Rows: 2, SeatsPerRow: 3},
		{Venue: "custom", Sections: 3, Rows: 2, SeatsPerRow: 1},
		{Venue: "custom", Sections: 2, Rows: 1, SeatsPerRow: 3},
		{Venue: "custom", Sections: 1, Rows: 6, SeatsPerRow: 1},
	} {
		l, err := resolve(r)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		// Every one of these is six seats. The name must still differ.
		if prev, dup := seen[l.name]; dup {
			t.Fatalf("shape %+v and %s share the name %q", r, prev, l.name)
		}
		seen[l.name] = l.name
	}
}
