package a2a

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/packages/egress"
)

// The A2A push-delivery suite (E19 T4, §3.5 D11/D12). Every guarantee here is written against the PUBLISHED
// A2A contract, cited inline:
//
//	CONTRACT (fetched 2026-07-25, https://a2a-protocol.org/latest/topics/streaming-and-async/):
//	  * "The A2A protocol defines the HTTP body payload as a `StreamResponse` object, matching the format
//	    used in streaming operations" — one of task | message | statusUpdate | artifactUpdate. NOT a Task.
//	  * Servers "SHOULD NOT blindly trust and send POST requests to any URL provided by a client";
//	    mitigations are domain allowlisting + ownership verification (+ an egress firewall).
//	  * "Notifications SHOULD include a timestamp. The webhook SHOULD reject notifications that are too old."
//	  * "Consider using unique, single-use identifiers (for example, JWT's `jti` claim or event IDs)."
//	  * The receiver must "rigorously verify the authenticity of incoming notification requests"
//	    (JWT signature / HMAC / API key).
//	  * PushNotificationConfig carries url, token ("client-side validation"), authentication, id — and the
//	    doc "does not specify a dedicated HTTP header name for transmitting the token". PushTokenHeader is
//	    therefore OUR choice (D11) and no test here asserts a foreign peer would understand it.

// --- sink harness -------------------------------------------------------------------------------------

// recordedPush is one POST a sink observed: the exact bytes and the headers that carried them.
type recordedPush struct {
	body    []byte
	headers http.Header
}

// pushSink is a loopback HTTPS receiver. status is consulted per request, so a test can hold the sink down
// for N attempts and then let it recover.
type pushSink struct {
	*httptest.Server
	mu     sync.Mutex
	got    []recordedPush
	status func(n int) int
}

func newPushSink(t *testing.T, status func(n int) int) *pushSink {
	t.Helper()
	s := &pushSink{status: status}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.got = append(s.got, recordedPush{body: body, headers: r.Header.Clone()})
		n := len(s.got)
		s.mu.Unlock()
		code := http.StatusOK
		if s.status != nil {
			code = s.status(n)
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *pushSink) received() []recordedPush {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedPush(nil), s.got...)
}

// redirectSink answers every request with a 302 to target — the SSRF-via-redirect shape.
func newRedirectSink(t *testing.T, target string) *httptest.Server {
	t.Helper()
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}))
	t.Cleanup(s.Close)
	return s
}

// trustAll builds the TLS config that trusts the given httptest certificates, so a loopback HTTPS sink is a
// REAL https delivery (not an http downgrade) without a public CA.
func trustAll(servers ...*httptest.Server) *tls.Config {
	pool := x509.NewCertPool()
	for _, s := range servers {
		pool.AddCert(s.Certificate())
	}
	return &tls.Config{RootCAs: pool}
}

// testPusher wires a WebhookPusher at the loopback sinks with a fast retry schedule (the tests must not
// sleep for the production curve). AllowPrivate opens 127.0.0.1 — the sinks ARE loopback; the metadata and
// special-use ranges stay denied even so (egress.VetIP).
func testPusher(t *testing.T, policy PushPolicy, servers ...*httptest.Server) (*WebhookPusher, *pushDeadLetters) {
	t.Helper()
	if policy.MaxAttempts == 0 {
		policy.MaxAttempts = 3
	}
	policy.AllowPrivate = true
	policy.BaseBackoff = time.Millisecond
	policy.MaxBackoff = 2 * time.Millisecond
	dead := &pushDeadLetters{}
	p := NewWebhookPusher(webhook.NewSender(webhook.WithTLSConfig(trustAll(servers...))), policy)
	p.DeadLetter = dead.record
	t.Cleanup(func() { p.Wait() })
	return p, dead
}

type pushDeadLetters struct {
	mu  sync.Mutex
	got []PushFailure
}

func (d *pushDeadLetters) record(_ context.Context, f PushFailure) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.got = append(d.got, f)
}

func (d *pushDeadLetters) list() []PushFailure {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]PushFailure(nil), d.got...)
}

func taskResponse() StreamResponse {
	return StreamResponse{Task: &Task{
		ID: "a2atask_1", ContextID: "a2actx_1", Kind: "task",
		Status: TaskStatus{State: TaskStateCompleted, Timestamp: "2026-07-25T00:00:00Z"},
	}}
}

// --- D11: the wire shape is a StreamResponse -----------------------------------------------------------

// TestPushBodyIsAStreamResponseNotATask pins D11: the POSTed body is a StreamResponse envelope whose SINGLE
// member discriminates the notification. A bare Task at the top level (the shape a `Push(cfg, []byte)`
// opaque payload invited) is NOT the contract.
func TestPushBodyIsAStreamResponseNotATask(t *testing.T) {
	sink := newPushSink(t, nil)
	p, _ := testPusher(t, PushPolicy{}, sink.Server)

	if err := p.Push(context.Background(), PushNotificationConfig{ID: "pc1", URL: sink.URL + "/hook"}, taskResponse()); err != nil {
		t.Fatalf("Push = %v, want accepted", err)
	}
	p.Wait()

	got := sink.received()
	if len(got) != 1 {
		t.Fatalf("sink saw %d deliveries, want 1", len(got))
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(got[0].body, &envelope); err != nil {
		t.Fatalf("body is not JSON: %v; %s", err, got[0].body)
	}
	if _, ok := envelope["task"]; !ok {
		t.Fatalf("body has no `task` member — the contract is a StreamResponse (task|message|statusUpdate|artifactUpdate); got %s", got[0].body)
	}
	// A StreamResponse is an envelope: the Task's own fields must NOT be hoisted to the top level.
	for _, hoisted := range []string{"id", "contextId", "status", "kind"} {
		if _, ok := envelope[hoisted]; ok {
			t.Fatalf("body hoisted Task field %q to the top level — that is a Task, not a StreamResponse; %s", hoisted, got[0].body)
		}
	}
	var back StreamResponse
	if err := json.Unmarshal(got[0].body, &back); err != nil || back.Task == nil || back.Task.ID != "a2atask_1" {
		t.Fatalf("body does not round-trip as a StreamResponse: err=%v %+v", err, back)
	}
}

// TestStreamResponseRequiresExactlyOneMember pins the union invariant the type exists to carry: zero members
// is not a notification and two members is not a discriminated one.
func TestStreamResponseRequiresExactlyOneMember(t *testing.T) {
	if err := (StreamResponse{}).Validate(); err == nil {
		t.Error("empty StreamResponse validated; want an error (no member is not a notification)")
	}
	two := StreamResponse{Task: &Task{ID: "t"}, Message: &Message{Role: "agent"}}
	if err := two.Validate(); err == nil {
		t.Error("two-member StreamResponse validated; want an error (the member name IS the discriminator)")
	}
	for name, sr := range map[string]StreamResponse{
		"task":           {Task: &Task{ID: "t"}},
		"message":        {Message: &Message{Role: "agent"}},
		"statusUpdate":   {StatusUpdate: &StatusUpdate{TaskID: "t"}},
		"artifactUpdate": {ArtifactUpdate: &ArtifactUpdate{TaskID: "t"}},
	} {
		if err := sr.Validate(); err != nil {
			t.Errorf("StreamResponse{%s}.Validate() = %v, want ok", name, err)
		}
	}
	// A rejected payload never reaches the wire.
	sink := newPushSink(t, nil)
	p, _ := testPusher(t, PushPolicy{}, sink.Server)
	if err := p.Push(context.Background(), PushNotificationConfig{ID: "pc1", URL: sink.URL + "/hook"}, StreamResponse{}); err == nil {
		t.Error("Push(empty StreamResponse) = nil, want refused")
	}
	p.Wait()
	if n := len(sink.received()); n != 0 {
		t.Errorf("sink saw %d deliveries for an invalid payload, want 0", n)
	}
}

// --- D12: SSRF ------------------------------------------------------------------------------------------

// TestPushRefusesSSRFTargets is the D12 RED guard: the webhook URL is CLIENT-supplied, so every delivery
// routes through packages/egress. The metadata address, private ranges, a scheme downgrade and an embedded
// credential are each refused BEFORE any socket is opened, and the refusal is terminal (errors.Is
// egress.ErrDenied) so it never retries.
func TestPushRefusesSSRFTargets(t *testing.T) {
	p, _ := testPusher(t, PushPolicy{})
	// AllowPrivate is ON (the loopback-sink posture); these must be refused even so.
	for _, target := range []string{
		"https://169.254.169.254/latest/meta-data", // cloud metadata (link-local) — never allowed
		"https://100.100.100.200/latest/meta-data", // Alibaba metadata (RFC6598) — never allowed
		"https://192.0.0.1/hook",                   // IETF protocol assignments
		"https://240.0.0.1/hook",                   // future-use
		"file:///etc/passwd",                       // not an HTTP scheme
		"https://user:pass@sink.example.test/hook", // embedded credentials leak an Authorization header
		"", // no destination at all
	} {
		err := p.Push(context.Background(), PushNotificationConfig{ID: "pc", URL: target}, taskResponse())
		if err == nil {
			t.Errorf("Push(%q) = nil, want refused", target)
			continue
		}
		if !errors.Is(err, egress.ErrDenied) && !errors.Is(err, ErrPushRefused) {
			t.Errorf("Push(%q) error = %v; want an egress/push denial a caller can classify terminal", target, err)
		}
	}
	// With the self-host flag OFF, a private/loopback target and an http downgrade are refused too.
	strict, _ := testPusher(t, PushPolicy{})
	strict.Policy.AllowPrivate = false
	for _, target := range []string{"https://10.0.0.5/hook", "https://127.0.0.1/hook", "http://sink.example.test/hook"} {
		if err := strict.Push(context.Background(), PushNotificationConfig{ID: "pc", URL: target}, taskResponse()); err == nil {
			t.Errorf("Push(%q, allowPrivate=false) = nil, want refused", target)
		}
	}
}

// TestPushAllowlistMatchesTheWholeHost is the SUP-2 guard (every membership comparison is guilty): the
// allowlist compares NORMALIZED WHOLE host values. A substring/prefix/suffix match would let
// `sink.example.test.evil.test` and `evil-sink.example.test` through — both are attacker-registrable.
func TestPushAllowlistMatchesTheWholeHost(t *testing.T) {
	p, _ := testPusher(t, PushPolicy{AllowedHosts: []string{"Sink.Example.Test"}})
	refused := []string{
		"https://evil-sink.example.test/hook",      // prefix-extended
		"https://sink.example.test.evil.test/x",    // suffix-extended (the classic domain-suffix defeat)
		"https://sink.example.tes/hook",            // truncated
		"https://xsink.example.test/hook",          // one char in front
		"https://sink.example.test@evil.test/x",    // userinfo that LOOKS like the allowed host
		"https://evil.test/?h=sink.example.test",   // the allowed host as a query value
		"https://evil.test/sink.example.test/hook", // the allowed host as a path segment
	}
	for _, target := range refused {
		if err := p.Push(context.Background(), PushNotificationConfig{ID: "pc", URL: target}, taskResponse()); err == nil {
			t.Errorf("Push(%q) = nil, want refused by the host allowlist", target)
		}
	}
	// Normalization (case, trailing root dot) is a match, not a miss — the allowlist must not be defeated by
	// an equivalent spelling either.
	for _, target := range []string{"https://sink.example.test/hook", "https://SINK.EXAMPLE.TEST/hook", "https://sink.example.test./hook"} {
		err := p.Push(context.Background(), PushNotificationConfig{ID: "pc", URL: target}, taskResponse())
		if err != nil && errors.Is(err, ErrPushRefused) {
			t.Errorf("Push(%q) = %v, want the allowlist to MATCH (normalized whole-host equality)", target, err)
		}
	}
	p.Wait()
}

// TestPushRevalidatesRedirectsRatherThanDenyingThemAll pins the E17-T3 A2A-client pattern (D12): a redirect
// is re-vetted through the SAME egress + allowlist gate. A redirect that LANDS on a private/metadata range
// is refused, and — the half a blanket deny cannot satisfy — a redirect that lands on an ALLOWED public
// target is followed and delivered.
func TestPushRevalidatesRedirectsRatherThanDenyingThemAll(t *testing.T) {
	final := newPushSink(t, nil)
	toMetadata := newRedirectSink(t, "https://169.254.169.254/latest/meta-data")
	toFinal := newRedirectSink(t, final.URL+"/hook")

	p, _ := testPusher(t, PushPolicy{}, final.Server, toMetadata, toFinal)

	// A redirect that lands internal is refused; the metadata host is never requested.
	if err := p.Push(context.Background(), PushNotificationConfig{ID: "pc1", URL: toMetadata.URL + "/hook"}, taskResponse()); err != nil {
		t.Fatalf("Push to a redirecting sink = %v, want accepted for delivery", err)
	}
	p.Wait()
	if n := len(final.received()); n != 0 {
		t.Fatalf("the metadata-redirect delivery reached the sink %d times, want 0", n)
	}

	// A redirect that lands on an allowed target IS followed — this is what distinguishes revalidation from
	// the blanket redirect deny the outbound webhook pump uses.
	if err := p.Push(context.Background(), PushNotificationConfig{ID: "pc2", URL: toFinal.URL + "/hook"}, taskResponse()); err != nil {
		t.Fatalf("Push to a sink redirecting to an allowed target = %v, want accepted", err)
	}
	p.Wait()
	if n := len(final.received()); n != 1 {
		t.Fatalf("revalidated redirect delivered %d times, want 1 (a blanket redirect deny would be 0)", n)
	}
}

// --- D12: authenticity + replay -------------------------------------------------------------------------

// TestPushCarriesTokenTimestampAndSingleUseID pins the authenticity + replay-protection contract: the
// config's token rides OUR application-specific header (PushTokenHeader — the spec names none, D11) AND
// keys an HMAC over the exact bytes, and every delivery carries a timestamp and a unique id. Tampering with
// one byte breaks the MAC.
func TestPushCarriesTokenTimestampAndSingleUseID(t *testing.T) {
	sink := newPushSink(t, nil)
	p, _ := testPusher(t, PushPolicy{}, sink.Server)
	const token = "PUSH_SHARED_SECRET"

	before := time.Now().Add(-time.Second)
	for _, id := range []string{"pc1", "pc2"} {
		if err := p.Push(context.Background(), PushNotificationConfig{ID: id, URL: sink.URL + "/hook", Token: token}, taskResponse()); err != nil {
			t.Fatalf("Push(%s) = %v", id, err)
		}
	}
	p.Wait()

	got := sink.received()
	if len(got) != 2 {
		t.Fatalf("sink saw %d deliveries, want 2", len(got))
	}
	ids := map[string]bool{}
	for i, rec := range got {
		if v := rec.headers.Get(PushTokenHeader); v != token {
			t.Errorf("delivery %d %s = %q, want the config token", i, PushTokenHeader, v)
		}
		id := rec.headers.Get(webhook.HeaderID)
		if id == "" {
			t.Fatalf("delivery %d carries no %s (the single-use notification id)", i, webhook.HeaderID)
		}
		if ids[id] {
			t.Errorf("delivery %d reused notification id %q — the id must be single-use", i, id)
		}
		ids[id] = true

		unix, err := strconv.ParseInt(rec.headers.Get(webhook.HeaderTimestamp), 10, 64)
		if err != nil {
			t.Fatalf("delivery %d timestamp header %q is unparseable: %v", i, rec.headers.Get(webhook.HeaderTimestamp), err)
		}
		ts := time.Unix(unix, 0)
		if ts.Before(before) || ts.After(time.Now().Add(time.Second)) {
			t.Errorf("delivery %d timestamp %v is outside the send window", i, ts)
		}
		// Authenticity: the receiver verifies the HMAC over the RAW body under the shared token.
		if !webhook.Verify([]byte(token), id, ts, rec.body, rec.headers.Get(webhook.HeaderSignature), time.Now(), 5*time.Minute) {
			t.Errorf("delivery %d signature does not verify under the config token", i)
		}
		// Tamper: one flipped byte must break it.
		tampered := append([]byte(nil), rec.body...)
		tampered[len(tampered)-2] ^= 0x20
		if webhook.Verify([]byte(token), id, ts, tampered, rec.headers.Get(webhook.HeaderSignature), time.Now(), 5*time.Minute) {
			t.Errorf("delivery %d signature verified over TAMPERED bytes", i)
		}
		// A replayed body under a DIFFERENT id must not verify — the id is bound into the signed input.
		if webhook.Verify([]byte(token), id+"x", ts, rec.body, rec.headers.Get(webhook.HeaderSignature), time.Now(), 5*time.Minute) {
			t.Errorf("delivery %d signature verified under a different notification id (replay window open)", i)
		}
	}
	// The token is a secret: it must never appear in the delivered BODY, only in the header/MAC.
	for i, rec := range got {
		if strings.Contains(string(rec.body), token) {
			t.Errorf("delivery %d body contains the push token: %s", i, rec.body)
		}
	}
}

// --- webhook-pump discipline: retry + dead-letter --------------------------------------------------------

// TestPushRetriesWithoutLossWhileTheSinkIsDown is the loss-less-retry guarantee: a sink that is down for the
// first attempts does not lose the notification — the SAME bytes under the SAME single-use id are redelivered
// until the sink recovers, with the attempt counter advancing (the webhook_pump discipline).
func TestPushRetriesWithoutLossWhileTheSinkIsDown(t *testing.T) {
	sink := newPushSink(t, func(n int) int {
		if n < 3 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	p, dead := testPusher(t, PushPolicy{MaxAttempts: 5}, sink.Server)

	if err := p.Push(context.Background(), PushNotificationConfig{ID: "pc1", URL: sink.URL + "/hook", Token: "tk"}, taskResponse()); err != nil {
		t.Fatalf("Push = %v", err)
	}
	p.Wait()

	got := sink.received()
	if len(got) != 3 {
		t.Fatalf("sink saw %d attempts, want 3 (two 503s then a 200)", len(got))
	}
	for i := 1; i < len(got); i++ {
		if string(got[i].body) != string(got[0].body) {
			t.Fatalf("attempt %d redelivered DIFFERENT bytes — a retry must replay the same notification", i+1)
		}
		if got[i].headers.Get(webhook.HeaderID) != got[0].headers.Get(webhook.HeaderID) {
			t.Fatalf("attempt %d used a new notification id — a retry is the SAME event, so the receiver can dedupe it", i+1)
		}
	}
	for i, rec := range got {
		if want := strconv.Itoa(i + 1); rec.headers.Get(webhook.HeaderAttempt) != want {
			t.Errorf("attempt %d header %s = %q, want %q", i+1, webhook.HeaderAttempt, rec.headers.Get(webhook.HeaderAttempt), want)
		}
	}
	if n := len(dead.list()); n != 0 {
		t.Errorf("a recovered delivery dead-lettered %d times, want 0", n)
	}
	if s := p.Stats(); s.Delivered != 1 || s.Dead != 0 {
		t.Errorf("stats = %+v, want Delivered=1 Dead=0", s)
	}
}

// TestPushDeadLettersAfterTheAttemptBound pins the dead-letter half: a sink that never recovers exhausts the
// bound and the delivery lands in the dead-letter seam with its cause — bounded, not retried forever.
func TestPushDeadLettersAfterTheAttemptBound(t *testing.T) {
	sink := newPushSink(t, func(int) int { return http.StatusInternalServerError })
	p, dead := testPusher(t, PushPolicy{MaxAttempts: 3}, sink.Server)

	if err := p.Push(context.Background(), PushNotificationConfig{ID: "pc1", URL: sink.URL + "/hook", Token: "tk"}, taskResponse()); err != nil {
		t.Fatalf("Push = %v", err)
	}
	p.Wait()

	if n := len(sink.received()); n != 3 {
		t.Fatalf("sink saw %d attempts, want exactly the 3-attempt bound", n)
	}
	failures := dead.list()
	if len(failures) != 1 {
		t.Fatalf("dead-letter fired %d times, want 1", len(failures))
	}
	if failures[0].ConfigID != "pc1" || failures[0].Attempts != 3 || failures[0].NotificationID == "" {
		t.Errorf("dead letter = %+v, want config pc1 / 3 attempts / a notification id", failures[0])
	}
	if failures[0].StatusCode != http.StatusInternalServerError {
		t.Errorf("dead letter status = %d, want the last observed 500", failures[0].StatusCode)
	}
	// A dead letter must NOT carry the shared secret into whatever records it.
	if strings.Contains(failures[0].String(), "tk") {
		t.Errorf("dead-letter record leaks the push token: %s", failures[0].String())
	}
	if s := p.Stats(); s.Delivered != 0 || s.Dead != 1 {
		t.Errorf("stats = %+v, want Delivered=0 Dead=1", s)
	}
}

// TestPushTerminalRejectIsNotRetried pins the retry-multiplication guard: a 4xx that is not a documented
// retryable code is terminal — one attempt, then dead. Retrying a permanent reject multiplies load on a
// receiver that already said no.
func TestPushTerminalRejectIsNotRetried(t *testing.T) {
	sink := newPushSink(t, func(int) int { return http.StatusForbidden })
	p, dead := testPusher(t, PushPolicy{MaxAttempts: 5}, sink.Server)

	if err := p.Push(context.Background(), PushNotificationConfig{ID: "pc1", URL: sink.URL + "/hook"}, taskResponse()); err != nil {
		t.Fatalf("Push = %v", err)
	}
	p.Wait()

	if n := len(sink.received()); n != 1 {
		t.Fatalf("a 403 was attempted %d times, want 1 (terminal, never retried)", n)
	}
	if n := len(dead.list()); n != 1 {
		t.Fatalf("dead-letter fired %d times, want 1", n)
	}
}
