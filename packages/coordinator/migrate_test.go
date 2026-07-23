package coordinator

import "testing"

// TestParseBool pins the strict boolean parser the fail-closed preflight gates rely on: recognized
// spellings map to their value with ok=true, and anything else reports ok=false so the caller rejects a
// typo instead of silently reading it as false (SF2).
func TestParseBool(t *testing.T) {
	cases := []struct {
		in    string
		value bool
		ok    bool
	}{
		{"1", true, true}, {"true", true, true}, {"YES", true, true}, {"on", true, true},
		{"0", false, true}, {"false", false, true}, {"No", false, true}, {"off", false, true},
		{"required", false, false}, {"2", false, false}, {"", false, false},
	}
	for _, c := range cases {
		value, ok := parseBool(c.in)
		if value != c.value || ok != c.ok {
			t.Errorf("parseBool(%q) = (%v, %v), want (%v, %v)", c.in, value, ok, c.value, c.ok)
		}
	}
}
