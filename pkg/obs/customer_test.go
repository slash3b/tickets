package obs

import "testing"

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
			if got := sanitiseCustomerID(tc.in); got != tc.want {
				t.Errorf("sanitiseCustomerID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
