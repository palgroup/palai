package webhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPrivateAndLoopbackDestinationsDeniedByDefault pins the AUT-012 static half (§21.4): loopback,
// RFC1918, and link-local (incl. the cloud metadata IP) literal destinations are denied by default,
// and a self-host operator opens private ranges with the explicit allowlist flag — but the metadata
// IP stays denied even then.
func TestPrivateAndLoopbackDestinationsDeniedByDefault(t *testing.T) {
	deniedByDefault := []string{
		"http://127.0.0.1/hook",
		"http://10.0.0.5/hook",
		"http://192.168.1.1/hook",
		"http://172.16.0.1/hook",
		"http://169.254.169.254/latest/meta-data", // cloud metadata
		"http://[::1]/hook",
	}
	for _, u := range deniedByDefault {
		if err := VetDestinationURL(u, false); err == nil {
			t.Errorf("VetDestinationURL(%q, allowPrivate=false) = nil, want denied", u)
		}
	}
	// A non-http(s) scheme is refused regardless of the flag.
	if err := VetDestinationURL("file:///etc/passwd", true); err == nil {
		t.Error("VetDestinationURL(file://) = nil, want scheme denied")
	}
	// The self-host allowlist flag opens loopback/RFC1918 for a private receiver...
	for _, u := range []string{"http://127.0.0.1/hook", "http://10.0.0.5/hook"} {
		if err := VetDestinationURL(u, true); err != nil {
			t.Errorf("VetDestinationURL(%q, allowPrivate=true) = %v, want allowed", u, err)
		}
	}
	// ...but the metadata / link-local range is never allowed, even with the flag.
	if err := VetDestinationURL("http://169.254.169.254/latest/meta-data", true); err == nil {
		t.Error("VetDestinationURL(metadata IP, allowPrivate=true) = nil, want still denied")
	}
	// A public destination passes over https.
	if err := VetDestinationURL("https://hooks.example.com/x", false); err != nil {
		t.Errorf("VetDestinationURL(public https) = %v, want allowed", err)
	}
	// http to a public host is a downgrade: denied unless the self-host flag opts in.
	if err := VetDestinationURL("http://hooks.example.com/x", false); err == nil {
		t.Error("VetDestinationURL(public http, no flag) = nil, want denied (https required)")
	}
	if err := VetDestinationURL("http://hooks.example.com/x", true); err != nil {
		t.Errorf("VetDestinationURL(http + flag) = %v, want allowed (self-host)", err)
	}
}

// TestSSRFRegistrationVetRejectsResolvedInternalTargets is the fail-fast registration gate (AUT-012):
// a hostname that ALREADY resolves into a private/loopback/link-local/metadata range is rejected at
// registration — not only at delivery. Resolution failure is permissive (the pinned dial at send time
// is the authoritative gate), and a public resolution passes. The rejection carries no target in it.
func TestSSRFRegistrationVetRejectsResolvedInternalTargets(t *testing.T) {
	internal := map[string]net.IP{
		"localhost.attacker.example": net.ParseIP("127.0.0.1"),
		"private.attacker.example":   net.ParseIP("10.0.0.5"),
		"lan.attacker.example":       net.ParseIP("192.168.1.10"),
		"metadata.attacker.example":  net.ParseIP("169.254.169.254"), // cloud metadata via DNS
		"ula.attacker.example":       net.ParseIP("fc00::1"),
		"public.example":             net.ParseIP("93.184.216.34"),
	}
	resolver := resolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
		if ip, ok := internal[host]; ok {
			return []net.IPAddr{{IP: ip}}, nil
		}
		return nil, errors.New("NXDOMAIN")
	})

	for _, host := range []string{"localhost.attacker.example", "private.attacker.example", "lan.attacker.example", "metadata.attacker.example", "ula.attacker.example"} {
		if err := VetDestination(context.Background(), resolver, "https://"+host+"/hook", false); err == nil {
			t.Errorf("VetDestination(%s -> internal) = nil, want rejected at registration", host)
		} else if strings.Contains(err.Error(), host) {
			t.Errorf("VetDestination error leaked the target host: %v", err)
		}
	}
	// A host that resolves public is allowed.
	if err := VetDestination(context.Background(), resolver, "https://public.example/hook", false); err != nil {
		t.Errorf("VetDestination(public) = %v, want allowed", err)
	}
	// A name that cannot be resolved now is allowed — the authoritative gate is the pinned dial at
	// send time, which re-resolves and re-vets (a rebind cannot smuggle an internal IP past it).
	if err := VetDestination(context.Background(), resolver, "https://unresolvable.example/hook", false); err != nil {
		t.Errorf("VetDestination(unresolvable) = %v, want allowed (send-time is authoritative)", err)
	}
	// 'localhost' the literal name resolves to loopback through the real resolver — proven unreachable.
	if err := VetDestination(context.Background(), nil, "http://localhost/hook", false); err == nil {
		t.Error("VetDestination(http://localhost) = nil, want rejected")
	}
}

// TestDNSRebindingReResolveDeniesFlippedTarget pins the AUT-012 dynamic half (§21.6): DNS is
// re-resolved through the egress policy on every attempt, and the connection is pinned to the vetted
// resolved IP (dialed by IP, never re-resolving the hostname), so a name that resolves public on the
// first attempt and private on the second is denied on the second — the rebinding TOCTOU is closed.
func TestDNSRebindingReResolveDeniesFlippedTarget(t *testing.T) {
	var lookups int
	resolver := resolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
		lookups++
		if lookups == 1 {
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil // public
		}
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.7")}}, nil // flipped to private
	})

	var dialedAddr string
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialedAddr = addr
		return nil, errors.New("dial stub: no real server")
	}
	sender := NewSender(WithResolver(resolver), WithDialContext(dial))
	dst := Destination{URL: "https://rebind.example.com/hook", TimeoutMS: 2000}

	// Attempt 1: resolves public, passes egress, and is DIALED BY THE RESOLVED IP (pinned) — the
	// stub dial fails, so this is a transport error (retryable), not an egress denial.
	r1 := sender.Deliver(context.Background(), dst, []byte("{}"))
	if r1.Terminal {
		t.Fatalf("attempt 1 to a public resolution = terminal deny, want a retryable transport error")
	}
	if !strings.HasPrefix(dialedAddr, "93.184.216.34:") {
		t.Fatalf("attempt 1 dialed %q, want the resolved public IP (pinned, no hostname re-resolution)", dialedAddr)
	}

	// Attempt 2: the name flips to a private IP. Egress denies it BEFORE dialing — the dial address
	// is unchanged from attempt 1, proving no connection to the flipped private target.
	dialedAddr = ""
	r2 := sender.Deliver(context.Background(), dst, []byte("{}"))
	if !r2.Terminal {
		t.Fatalf("attempt 2 to a flipped private resolution = %+v, want a terminal egress deny", r2)
	}
	if dialedAddr != "" {
		t.Fatalf("attempt 2 dialed %q, want NO dial (denied before connect)", dialedAddr)
	}
	if lookups != 2 {
		t.Fatalf("resolver called %d times, want a re-resolve per attempt (2)", lookups)
	}
}

// TestRedirectNotFollowed pins §21.6: redirects are not followed. A 3xx is a terminal deny and the
// Location target is never requested — proven with a real local HTTP server pair.
func TestRedirectNotFollowed(t *testing.T) {
	var locationHit bool
	location := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		locationHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer location.Close()

	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", location.URL)
			w.WriteHeader(code)
		}))
		sender := NewSender()
		res := sender.Deliver(context.Background(), Destination{URL: redirector.URL, AllowPrivate: true, TimeoutMS: 2000}, []byte("{}"))
		redirector.Close()
		if !res.Terminal {
			t.Fatalf("status %d: Deliver = %+v, want a terminal deny (redirect not followed)", code, res)
		}
	}
	if locationHit {
		t.Fatal("the redirect Location was requested — redirects must not be followed")
	}
}

// TestRedirectToInternalTargetNotDialed proves a public receiver cannot bounce a delivery into the
// cloud metadata IP via a 302: the redirect is a terminal deny and the metadata address is never
// dialed (AUT-012 — the redirect vector cannot reach an internal target).
func TestRedirectToInternalTargetNotDialed(t *testing.T) {
	var dialedMetadata bool
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		// The redirector itself is legitimately dialed at 127.0.0.1; only a dial to the metadata IP
		// (the redirect target) proves the SSRF vector opened.
		if strings.HasPrefix(addr, "169.254.169.254:") {
			dialedMetadata = true
		}
		return (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
	}
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://169.254.169.254/latest/meta-data")
		w.WriteHeader(http.StatusFound)
	}))
	defer redirector.Close()

	sender := NewSender(WithDialContext(dial))
	res := sender.Deliver(context.Background(), Destination{URL: redirector.URL, AllowPrivate: true, TimeoutMS: 2000}, []byte("{}"))
	if !res.Terminal {
		t.Fatalf("redirect to metadata = %+v, want a terminal deny", res)
	}
	if dialedMetadata {
		t.Fatal("the metadata IP was dialed via the redirect — SSRF redirect vector is open")
	}
}

// TestSignedDeliveryToRealReceiverVerifiesServerSide is the runnable live smoke (user policy): a real
// outbound HTTPS delivery to a real local TLS receiver that verifies the HMAC signature server-side
// over the exact raw body, and a by-construction leak scan proving the signing secret never rides the
// wire. Loopback is reached through the explicit allowlist flag (a self-host receiver), which the
// default-deny test above proves is required.
func TestSignedDeliveryToRealReceiverVerifiesServerSide(t *testing.T) {
	secret := []byte("whsec_live_smoke_secret")
	deliveryID := "whd_live_1"
	body := []byte(`{"type":"run.completed.v1","data":{"run_id":"run_live"}}`)

	var verifiedServerSide bool
	var wireBytesHadSecret bool
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		// Leak scan by construction: the secret must never appear in headers or body.
		if strings.Contains(headerBlob(r)+string(raw), string(secret)) {
			wireBytesHadSecret = true
		}
		id := r.Header.Get(HeaderID)
		tsHeader := r.Header.Get(HeaderTimestamp)
		ts, ok := parseUnixHeader(tsHeader)
		if !ok || !Verify(secret, id, ts, raw, r.Header.Get(HeaderSignature), time.Now(), 5*time.Minute) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		verifiedServerSide = true
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	pool := x509.NewCertPool()
	pool.AddCert(receiver.Certificate())
	sender := NewSender(WithTLSConfig(&tls.Config{RootCAs: pool}))

	signer := NewSigner(secret)
	headers := signer.Headers(deliveryID, time.Now(), 1, body)
	res := sender.Deliver(context.Background(), Destination{
		URL:          receiver.URL,
		AllowPrivate: true,
		TimeoutMS:    5000,
		Headers:      headers,
	}, body)

	if res.Err != nil {
		t.Fatalf("live delivery error = %v", res.Err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("live delivery status = %d, want 200 (server-side HMAC verify failed)", res.StatusCode)
	}
	if !verifiedServerSide {
		t.Fatal("the receiver did not verify the signature server-side")
	}
	if wireBytesHadSecret {
		t.Fatal("the signing secret leaked onto the wire")
	}
}

func headerBlob(r *http.Request) string {
	var b strings.Builder
	for k, vs := range r.Header {
		for _, v := range vs {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\n")
		}
	}
	return b.String()
}

type resolverFunc func(ctx context.Context, host string) ([]net.IPAddr, error)

func (f resolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}
