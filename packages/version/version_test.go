package version

import "testing"

func TestSupportedWindow(t *testing.T) {
	cases := []struct {
		name     string
		cp       string
		runner   string
		wantOK   bool
		wantHint string // a substring the rejection message must contain (empty when accepted)
	}{
		{"same version", "0.15.0", "0.15.0", true, ""},
		{"one minor behind", "0.15.0", "0.14.9", true, ""},
		{"two minors behind (edge, in window)", "0.15.0", "0.13.0", true, ""},
		{"three minors behind (rejected, hop message)", "0.15.0", "0.12.0", false, "0.13.0"},
		{"far behind names the oldest served minor", "0.15.2", "0.9.0", false, "0.13.0"},
		{"runner newer than control-plane", "0.15.0", "0.16.0", false, "newer than control-plane"},
		{"major mismatch", "1.0.0", "0.15.0", false, "major"},
		{"git-describe suffix parses to minor", "0.15.0-4-gdeadbee", "0.15.0-1-gcafef00", true, ""},
		{"v prefix tolerated", "v0.15.0", "v0.13.0", true, ""},
		{"unstamped runner skips the check", "0.15.0", "dev", true, ""},
		{"unstamped control-plane skips the check", "abc123def", "0.12.0", true, ""},
		{"both unstamped (from-source local up)", "dev", "1a2b3c4d-dirty", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, msg := Supported(c.cp, c.runner)
			if ok != c.wantOK {
				t.Fatalf("Supported(%q, %q) ok=%v, want %v (msg=%q)", c.cp, c.runner, ok, c.wantOK, msg)
			}
			if !ok && c.wantHint != "" && !contains(msg, c.wantHint) {
				t.Fatalf("rejection message %q does not name %q", msg, c.wantHint)
			}
			if ok && msg != "" {
				t.Fatalf("accepted skew returned a non-empty message %q", msg)
			}
		})
	}
}

func TestResolveFallsBackToDevOrVCS(t *testing.T) {
	// With no ldflags override, Resolve is the embedded VCS revision or "dev" — never empty, so the
	// applied_by stamp and the runner's advertised version are always populated.
	if got := Resolve(); got == "" {
		t.Fatal("Resolve returned an empty stamp")
	}
	// An explicit stamp always wins.
	old := Stamp
	t.Cleanup(func() { Stamp = old })
	Stamp = "0.15.0-7-gabc1234"
	if got := Resolve(); got != "0.15.0-7-gabc1234" {
		t.Fatalf("Resolve with an override = %q, want the override", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
