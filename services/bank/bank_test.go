package bank

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestConfigRoundTrips guards the operator page against showing a number it never
// read. The sliders were write-only and rendered their hardcoded default after
// every reload, so the page could report a tame 5% while the bank was declining
// everything.
func TestConfigRoundTrips(t *testing.T) {
	b := New(Config{DeclineRate: 0.05, TimeoutRate: 0.01})

	if got := b.Config(); got.DeclineRate != 0.05 {
		t.Fatalf("decline rate = %v, want the value it was built with", got.DeclineRate)
	}

	b.SetConfig(Config{DeclineRate: 1, TimeoutRate: 0.5})
	if got := b.Config(); got.DeclineRate != 1 || got.TimeoutRate != 0.5 {
		t.Fatalf("config = %+v, want what was just set", got)
	}
}

// TestGetConfigOverHTTP: the operator page reads this on load, so a 405 here is
// the page silently falling back to its defaults again.
func TestGetConfigOverHTTP(t *testing.T) {
	srv := httptest.NewServer(New(Config{DeclineRate: 0.42, TimeoutRate: 0.07}).Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/config")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /config -> %d, want 200", res.StatusCode)
	}
	var got Config
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.DeclineRate != 0.42 || got.TimeoutRate != 0.07 {
		t.Fatalf("config = %+v, want the live settings", got)
	}
}
