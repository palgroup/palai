package egress

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

type resolverFunc func(ctx context.Context, host string) ([]net.IPAddr, error)

func (f resolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

// TestVetIPDeniesMetadataPrivateAndSpecialUse is the single-IP egress decision table (AUT-012): the
// reserved/metadata + special-use ranges are NEVER allowed (not even under allowPrivate), loopback and
// private/ULA are denied by default and opened only under the flag, and public addresses pass.
func TestVetIPDeniesMetadataPrivateAndSpecialUse(t *testing.T) {
	// Denied ALWAYS — even under allowPrivate (a self-host receiver has no business reaching these).
	alwaysDenied := []string{
		"169.254.169.254", // AWS/GCP/Azure link-local metadata
		"169.254.0.1",     // link-local v4
		"fe80::1",         // link-local v6
		"100.100.100.200", // Alibaba metadata (RFC6598 CGNAT 100.64.0.0/10)
		"100.64.0.1",      // CGNAT lower bound
		"192.0.0.1",       // RFC6890 IETF protocol assignments
		"198.18.0.1",      // RFC2544 benchmarking
		"240.0.0.1",       // RFC1112 future-use
		"224.0.0.1",       // multicast v4
		"ff02::1",         // multicast v6
		"0.0.0.0",         // unspecified v4
		"::",              // unspecified v6
	}
	for _, s := range alwaysDenied {
		for _, allowPrivate := range []bool{false, true} {
			if err := VetIP(net.ParseIP(s), allowPrivate); err == nil {
				t.Errorf("VetIP(%s, allowPrivate=%v) = nil, want denied (reserved/special-use)", s, allowPrivate)
			} else if !errors.Is(err, ErrDenied) {
				t.Errorf("VetIP(%s) error %v is not ErrDenied", s, err)
			}
		}
	}
	// Denied by default, opened by the flag: loopback, RFC1918, ULA.
	privateByDefault := []string{"127.0.0.1", "::1", "10.0.0.8", "192.168.1.1", "172.16.0.1", "fd00::1", "fc00::1"}
	for _, s := range privateByDefault {
		if err := VetIP(net.ParseIP(s), false); err == nil {
			t.Errorf("VetIP(%s, allowPrivate=false) = nil, want denied", s)
		}
		if err := VetIP(net.ParseIP(s), true); err != nil {
			t.Errorf("VetIP(%s, allowPrivate=true) = %v, want allowed", s, err)
		}
	}
	// Public addresses pass.
	for _, s := range []string{"93.184.216.34", "8.8.8.8", "2606:2800:220:1:248:1893:25c8:1946"} {
		if err := VetIP(net.ParseIP(s), false); err != nil {
			t.Errorf("VetIP(%s, allowPrivate=false) = %v, want allowed (public)", s, err)
		}
	}
	// A nil IP (unparseable) is denied.
	if err := VetIP(nil, true); err == nil {
		t.Error("VetIP(nil) = nil, want denied")
	}
}

// TestVetURLSchemeAndLiteralIP pins the static URL gate: https is required (http is a downgrade opened
// only under the flag), a non-http(s) scheme is refused regardless, and a literal-IP host is vetted.
func TestVetURLSchemeAndLiteralIP(t *testing.T) {
	if err := VetURL("https://example.com/", false); err != nil {
		t.Errorf("VetURL(public https) = %v, want allowed", err)
	}
	if err := VetURL("http://example.com/", false); err == nil {
		t.Error("VetURL(public http, no flag) = nil, want denied (https required)")
	}
	if err := VetURL("http://example.com/", true); err != nil {
		t.Errorf("VetURL(http + flag) = %v, want allowed (self-host downgrade)", err)
	}
	if err := VetURL("file:///etc/passwd", true); err == nil {
		t.Error("VetURL(file://) = nil, want scheme denied")
	}
	if err := VetURL("https:///path", false); err == nil {
		t.Error("VetURL(empty host) = nil, want denied")
	}
	for _, u := range []string{"https://169.254.169.254/", "https://127.0.0.1/", "https://[::1]/"} {
		if err := VetURL(u, false); err == nil {
			t.Errorf("VetURL(%s) = nil, want literal-IP denied", u)
		}
	}
}

// TestVetResolvedRejectsInternalResolutions is the fail-fast resolution gate: a hostname that resolves
// internal is rejected without leaking the host, an unresolvable name is permissive (the pinned dial is
// authoritative), and a public resolution passes.
func TestVetResolvedRejectsInternalResolutions(t *testing.T) {
	internal := map[string]net.IP{
		"metadata.attacker.example": net.ParseIP("169.254.169.254"),
		"private.attacker.example":  net.ParseIP("10.0.0.5"),
		"public.example":            net.ParseIP("93.184.216.34"),
	}
	resolver := resolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
		if ip, ok := internal[host]; ok {
			return []net.IPAddr{{IP: ip}}, nil
		}
		return nil, errors.New("NXDOMAIN")
	})
	for _, host := range []string{"metadata.attacker.example", "private.attacker.example"} {
		err := VetResolved(context.Background(), resolver, "https://"+host+"/x", false)
		if err == nil {
			t.Errorf("VetResolved(%s -> internal) = nil, want rejected", host)
		} else if strings.Contains(err.Error(), host) {
			t.Errorf("VetResolved error leaked the target host: %v", err)
		}
	}
	if err := VetResolved(context.Background(), resolver, "https://public.example/x", false); err != nil {
		t.Errorf("VetResolved(public) = %v, want allowed", err)
	}
	if err := VetResolved(context.Background(), resolver, "https://unresolvable.example/x", false); err != nil {
		t.Errorf("VetResolved(unresolvable) = %v, want allowed (dial-time is authoritative)", err)
	}
}

// TestPinnedDialerResolvesVetsAndPins closes the DNS-rebinding TOCTOU: a private resolution is denied
// with ZERO dial, a public resolution dials the resolved IP (never re-resolving the hostname), and a
// name that flips public→private between attempts is denied on the second — dialed by IP, pinned.
func TestPinnedDialerResolvesVetsAndPins(t *testing.T) {
	var lookups int
	resolver := resolverFunc(func(_ context.Context, _ string) ([]net.IPAddr, error) {
		lookups++
		if lookups == 1 {
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil // public
		}
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.7")}}, nil // flipped private
	})
	var dialed string
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = addr
		return nil, errors.New("dial stub")
	}
	pinned := PinnedDialer(resolver, false, dial)

	// Attempt 1: public — dialed BY THE RESOLVED IP, not the hostname.
	dialed = ""
	if _, err := pinned(context.Background(), "tcp", "rebind.example.com:443"); err == nil {
		t.Fatal("attempt 1: want the dial-stub transport error")
	} else if errors.Is(err, ErrDenied) {
		t.Fatalf("attempt 1 to a public resolution = ErrDenied, want a dial-through: %v", err)
	}
	if !strings.HasPrefix(dialed, "93.184.216.34:") {
		t.Fatalf("attempt 1 dialed %q, want the resolved public IP (pinned)", dialed)
	}

	// Attempt 2: flipped private — denied BEFORE dialing.
	dialed = ""
	if _, err := pinned(context.Background(), "tcp", "rebind.example.com:443"); !errors.Is(err, ErrDenied) {
		t.Fatalf("attempt 2 to a flipped private resolution = %v, want ErrDenied", err)
	}
	if dialed != "" {
		t.Fatalf("attempt 2 dialed %q, want NO dial (denied before connect)", dialed)
	}
	if lookups != 2 {
		t.Fatalf("resolver called %d times, want a re-resolve per attempt (2)", lookups)
	}

	// A literal-IP address is vetted and dialed by that IP without any resolution.
	before := lookups
	dialed = ""
	if _, err := pinned(context.Background(), "tcp", "8.8.8.8:443"); err == nil || errors.Is(err, ErrDenied) {
		t.Fatalf("literal public IP: want dial-through, got %v", err)
	}
	if !strings.HasPrefix(dialed, "8.8.8.8:") || lookups != before {
		t.Fatalf("literal IP dialed %q (lookups %d→%d), want a direct dial with no resolution", dialed, before, lookups)
	}
}
