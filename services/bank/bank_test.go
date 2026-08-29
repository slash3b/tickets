package bank

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// TestSetConfigMergesRatherThanReplaces guards the fake bank's latency against
// the operator page.
//
// That page sends only decline_rate and timeout_rate. When setConfig decoded into
// a zero Config, every nudge of a slider set min_latency and max_latency to zero
// — deleting the 20-300ms this service exists to simulate. Nothing errored;
// payments simply became instant, which is both the least realistic behaviour it
// can have and the hardest to spot.
func TestSetConfigMergesRatherThanReplaces(t *testing.T) {
	b := New(Config{
		MinLatency:  20 * time.Millisecond,
		MaxLatency:  300 * time.Millisecond,
		DeclineRate: 0.05,
		TimeoutRate: 0.01,
	})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	// Exactly what the slider sends: the two rates, nothing else.
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/config",
		strings.NewReader(`{"decline_rate":0.5,"timeout_rate":0.2}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT /config -> %d, want 204", res.StatusCode)
	}

	got := b.Config()
	if got.DeclineRate != 0.5 || got.TimeoutRate != 0.2 {
		t.Errorf("rates = %v/%v, want the values just sent", got.DeclineRate, got.TimeoutRate)
	}
	if got.MinLatency != 20*time.Millisecond || got.MaxLatency != 300*time.Millisecond {
		t.Fatalf("latency = %v..%v, want it untouched — a partial update must not "+
			"zero the fields it did not mention", got.MinLatency, got.MaxLatency)
	}
}
