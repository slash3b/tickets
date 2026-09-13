package obs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap/zapcore"
)

// TestCustomerIDIsSanitised guards the fact that this value comes from whoever is
// calling and ends up in an HTTP header on every downstream hop.
//
// Baggage is propagated as a header, so a comma or a semicolon in the value would
// corrupt that header for every service behind this one. There is no legitimate
// customer id that needs those characters, so anything unexpected is dropped
// rather than escaped.
func TestCustomerIDIsSanitised(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"a plain id", "ui-slash3b", "ui-slash3b"},
		{"a uuid", "9f3ac0de-1111-4222-8333-444455556666", "9f3ac0de-1111-4222-8333-444455556666"},
		{"dots and underscores", "sim.buyer_42", "sim.buyer_42"},
		{"trimmed", "  ui-slash3b  ", "ui-slash3b"},
		{"empty", "", ""},

		// The ones that matter: each would break the baggage header downstream.
		{"a comma splits baggage members", "a,b", ""},
		{"a semicolon splits properties", "a;b", ""},
		{"an equals splits key from value", "a=b", ""},
		{"whitespace inside", "two words", ""},
		{"a newline", "a\nb", ""},
		{"absurdly long", string(make([]byte, 200)), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitiseTag(tc.in); got != tc.want {
				t.Errorf("sanitiseTag(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestClientTagsRideBaggage is the whole point of the change: the profile has to
// reach services the simulator never talks to.
//
// It used to be recoverable only by prefix-matching the customer id — the
// simulator writes "sim-<profile>-<8 hex>" — which made "every picky buyer" a
// LIKE over a composite string, and would break outright the day a profile name
// contains a dash. As baggage it is an equality on a field, on every hop.
func TestClientTagsRideBaggage(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.Header.Set(CustomerHeader, "sim-picky-1a2b3c4d")
	req.Header.Set(ProfileHeader, "picky")

	ctx := WithClientTags(context.Background(), req)

	if got := CustomerFromContext(ctx); got != "sim-picky-1a2b3c4d" {
		t.Errorf("customer = %q, want the header value", got)
	}
	if got := ProfileFromContext(ctx); got != "picky" {
		t.Errorf("profile = %q, want picky", got)
	}

	// A REAL BROWSER SENDS NEITHER, and the absence must stay an absence rather
	// than an empty-string field on every line the gateway ever logs.
	bare := WithClientTags(context.Background(), httptest.NewRequest(http.MethodGet, "/", nil))
	if f := ProfileField(bare); f.Type != zapcore.SkipType {
		t.Errorf("ProfileField with no header = %v, want a skipped field", f)
	}
	if f := CustomerField(bare); f.Type != zapcore.SkipType {
		t.Errorf("CustomerField with no header = %v, want a skipped field", f)
	}

	// The sanitiser applies to the profile too — it arrives at the same
	// unauthenticated door from the same strangers.
	dirty := httptest.NewRequest(http.MethodGet, "/", nil)
	dirty.Header.Set(ProfileHeader, "picky,evil=1")
	if got := ProfileFromContext(WithClientTags(context.Background(), dirty)); got != "" {
		t.Errorf("profile = %q, want it dropped for corrupting the baggage header", got)
	}
}
