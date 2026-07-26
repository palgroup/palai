package a2a

// A2A push-notification DELIVERY (E19 T4, spec §38, §3.5 D11/D12/D13).
//
// WHAT THIS IS AND IS NOT — read before naming it in a log line, a doc or a claim. This delivers an A2A
// `StreamResponse` payload to a client-registered webhook with an APPLICATION-SPECIFIC token header. It is
// NOT "spec-compliant push": the A2A specification defines the `token` FIELD but names no header to carry
// it (see PushTokenHeader), so a foreign peer is not claimed to understand our choice. Whether a real
// foreign A2A peer accepts these deliveries is E17 §6 leg 2 and is not proven anywhere in this repo — a
// loopback sink is not interop, and `a2a` therefore stays a preview capability.
//
// It rides the EXISTING signed outbound webhook sender (adapters/integrations/webhook): the same
// egress-vetted, IP-pinned transport the §21.6 delivery pump uses, and the same retry / dead-letter
// discipline (webhook.Classify / NextBackoff / RetryExhausted, hoisted in E19 T4 so there is one copy). No
// second delivery machine.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/packages/egress"
)

// PushTokenHeader carries PushNotificationConfig.token to the receiver.
//
// CONTRACT GAP — OUR CHOICE, NOT THE SPEC'S (§3.5 D11; fetched 2026-07-26 from
// https://a2a-protocol.org/latest/topics/streaming-and-async/): the specification defines the `token` field
// ("optional field for client-side validation") but "does not specify a dedicated HTTP header name for
// transmitting the token". This name is an application-specific decision. Nothing here may be described as
// spec-compliant push, and a foreign peer that expects a different header will not see the token — which is
// exactly why interop remains the §6 operator leg.
const PushTokenHeader = "X-A2A-Notification-Token"

// ErrPushRefused marks a notification the push policy refused outright (an invalid payload, a destination
// off the allowlist, no destination at all). Callers classify it terminal. An egress denial wraps
// egress.ErrDenied instead, so both are matchable with errors.Is.
var ErrPushRefused = errors.New("a2a: push notification refused by policy")

// errPushOverloaded is the in-flight bound's refusal: the delivery is dead-lettered rather than queued
// without limit, so a task with many registered targets cannot exhaust the process.
var errPushOverloaded = errors.New("a2a: push delivery capacity exhausted")

// Push delivery defaults. They are tighter than the §21.6 journal pump's (20 attempts / 72h): a task-status
// notification that lands three days late is noise, not news.
const (
	defaultPushMaxAttempts = 8
	defaultPushRetryWindow = 15 * time.Minute
	defaultPushBaseBackoff = 2 * time.Second
	defaultPushMaxBackoff  = 2 * time.Minute
	defaultPushTimeoutMS   = 10000
	defaultPushMaxInFlight = 32
	// maxPushRedirects bounds a redirect chain. Each hop is REVALIDATED (see PushPolicy), never blindly
	// followed.
	maxPushRedirects = 3
)

// PushPolicy is the deployment's push-delivery policy.
//
// CONTRACT (D12, same source): a server "SHOULD NOT blindly trust and send POST requests to any URL
// provided by a client"; the named mitigations are domain allowlisting, ownership verification and an
// egress firewall. AllowedHosts is the allowlist half; packages/egress is the firewall half. Ownership
// verification (proving the registering client controls the target) is NOT implemented — see the honest
// ceiling in Push.
type PushPolicy struct {
	// AllowedHosts, when non-empty, is the exact set of hosts a push target may name. Matching is on the
	// NORMALIZED WHOLE host (see hostAllowed) — never a prefix, suffix or substring test. Empty means no
	// host allowlist is configured and the egress policy alone governs; that is a deliberately weaker
	// posture and a deployment exposed to untrusted registrants should set it.
	AllowedHosts []string
	// AllowPrivate opens loopback/RFC1918 destinations for a self-host receiver. The metadata and
	// special-use ranges stay denied even under it (egress.VetIP).
	AllowPrivate bool
	TimeoutMS    int
	MaxAttempts  int
	RetryWindow  time.Duration
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	// MaxInFlight bounds the detached delivery goroutines.
	MaxInFlight int
}

// PushFailure is one dead-lettered notification. It deliberately carries NO destination URL and NO token —
// a dead-letter record is written to logs and operator surfaces, and a push token is a shared secret.
type PushFailure struct {
	ConfigID       string
	NotificationID string
	Attempts       int
	StatusCode     int
	Err            error
}

func (f PushFailure) String() string {
	return fmt.Sprintf("a2a push %s (config %s) dead-lettered after %d attempts: status=%d err=%v",
		f.NotificationID, f.ConfigID, f.Attempts, f.StatusCode, f.Err)
}

// PushStats are the delivery counters (evidence + operator visibility).
type PushStats struct {
	Delivered int64
	Dead      int64
	Refused   int64
}

// WebhookPusher is the production Pusher. Push returns as soon as the notification is accepted; the retry
// schedule runs on a detached goroutine, because the callers are HTTP handlers that must not block for a
// receiver's backoff curve.
//
// ponytail: the pending set is IN-PROCESS. A crash between accept and delivery drops the notification — the
// honest ceiling, and the reason this is not called durable anywhere. Wait() drains in-flight deliveries for
// a graceful shutdown. The upgrade path is a durable row (the webhook_deliveries / queue_deliveries shape),
// which needs a table and therefore a migration; E19 is migration-free by construction.
type WebhookPusher struct {
	// Policy is read on every Push, so a deployment may re-point the allowlist without a rebuild.
	Policy PushPolicy
	// DeadLetter receives every notification that exhausted its retries or was refused mid-flight. Nil
	// discards it. A push failure NEVER propagates into the task/run result (§T4: a push error must not
	// erase the canonical outcome) — this seam is the only thing a failure touches.
	DeadLetter func(context.Context, PushFailure)

	sender *webhook.Sender
	sem    chan struct{}
	semOne sync.Once
	wg     sync.WaitGroup

	delivered atomic.Int64
	dead      atomic.Int64
	refused   atomic.Int64
}

// NewWebhookPusher wires the pusher onto an existing signed outbound sender — the same one the §21.6
// delivery pump uses. It is deliberately NOT constructed here: a deployment configures the sender's
// resolver / dialer / TLS once and both outbound surfaces share it.
func NewWebhookPusher(sender *webhook.Sender, policy PushPolicy) *WebhookPusher {
	return &WebhookPusher{Policy: policy, sender: sender}
}

// Compile-time proof this satisfies the Server's seam.
var _ Pusher = (*WebhookPusher)(nil)

// pushDelivery is one accepted notification: the exact bytes, and the single-use id that identifies them
// across every attempt.
type pushDelivery struct {
	cfg  PushNotificationConfig
	id   string
	body []byte
}

// Push validates the payload against the published StreamResponse contract, vets the CLIENT-SUPPLIED
// destination through the egress policy and the host allowlist, and then hands the delivery to its own
// retry schedule. A policy refusal is returned SYNCHRONOUSLY so the caller sees it; a transport failure is
// not (it retries, then dead-letters).
//
// HONEST CEILING: this implements the allowlist + egress-firewall half of D12. It does NOT implement
// ownership verification — nothing proves the client registering a target actually controls it. A
// deployment open to untrusted registrants needs AllowedHosts set; without it, any public https host a
// client names will be POSTed to.
func (p *WebhookPusher) Push(ctx context.Context, cfg PushNotificationConfig, resp StreamResponse) error {
	if err := resp.Validate(); err != nil {
		p.refused.Add(1)
		return err
	}
	if err := p.vet(cfg.URL); err != nil {
		p.refused.Add(1)
		return err
	}
	body, err := json.Marshal(resp)
	if err != nil {
		p.refused.Add(1)
		return fmt.Errorf("%w: marshal StreamResponse: %v", ErrPushRefused, err)
	}

	d := pushDelivery{cfg: cfg, id: newNotificationID(), body: body}
	// The request context dies when the handler returns, so the retry schedule runs on a detached one
	// bounded by the retry window — a push must outlive the response that triggered it.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.retryWindow())
	if !p.acquire() {
		cancel()
		p.deadLetter(ctx, d, 0, webhook.Result{Err: errPushOverloaded})
		return nil
	}
	p.wg.Add(1)
	go func() {
		defer cancel()
		defer p.wg.Done()
		defer p.release()
		p.deliver(runCtx, d)
	}()
	return nil
}

// Wait blocks until every accepted delivery has settled (delivered or dead-lettered). It is the graceful
// drain hook and the test seam; it does NOT make the pending set durable.
func (p *WebhookPusher) Wait() { p.wg.Wait() }

// Stats reports the delivery counters.
func (p *WebhookPusher) Stats() PushStats {
	return PushStats{Delivered: p.delivered.Load(), Dead: p.dead.Load(), Refused: p.refused.Load()}
}

// vet is the pre-flight destination gate: the shared egress policy (https-only unless AllowPrivate, no
// embedded credentials, no internal/metadata literal IP) and then the host allowlist.
//
// A hostname is NOT resolved here. That is the same posture the webhook sender takes: resolution is vetted
// authoritatively at CONNECT time by the pinned dialer (which closes the DNS-rebinding TOCTOU), so a name
// that resolves internal is refused there and classified terminal — it just fails asynchronously rather
// than at accept.
func (p *WebhookPusher) vet(rawURL string) error {
	if strings.TrimSpace(rawURL) == "" {
		return fmt.Errorf("%w: the push config names no destination url", ErrPushRefused)
	}
	if err := egress.VetURL(rawURL, p.Policy.AllowPrivate); err != nil {
		return err
	}
	u, err := url.Parse(rawURL)
	if err != nil { // unreachable: VetURL already parsed it
		return fmt.Errorf("%w: unparseable destination", ErrPushRefused)
	}
	return p.hostAllowed(u)
}

// hostAllowed matches the destination host against PushPolicy.AllowedHosts.
//
// SUP-2 (every membership comparison is guilty until a RED test proves otherwise): the comparison is
// equality on the NORMALIZED WHOLE host — lowercased and stripped of the trailing root dot — never a
// prefix, suffix or substring test. Each defeat this closes is attacker-registrable:
// `evil-sink.example.test` (prefix-extended), `sink.example.test.evil.test` (the classic domain-suffix
// defeat), and `https://sink.example.test@evil.test/` — where the allowed name is USERINFO and url.Hostname
// correctly reports `evil.test`. A path or query containing the allowed name is likewise not a host.
func (p *WebhookPusher) hostAllowed(u *url.URL) error {
	if len(p.Policy.AllowedHosts) == 0 {
		return nil // no allowlist configured: the egress policy alone governs (see PushPolicy)
	}
	host := normalizeHost(u.Hostname())
	for _, allowed := range p.Policy.AllowedHosts {
		if host != "" && normalizeHost(allowed) == host {
			return nil
		}
	}
	return fmt.Errorf("%w: the destination host is not on the push allowlist", ErrPushRefused)
}

func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// deliver runs one notification's attempt schedule under the shared webhook retry discipline: attempt,
// classify, back off with full jitter, and dead-letter on a terminal outcome or an exhausted bound.
func (p *WebhookPusher) deliver(ctx context.Context, d pushDelivery) {
	policy := webhook.DeliveryPolicy{MaxAttempts: p.maxAttempts(), RetryWindow: p.retryWindow()}
	first := time.Now()
	for attempt := 1; ; attempt++ {
		res := p.attempt(ctx, d, attempt)
		switch webhook.Classify(res) {
		case webhook.OutcomeComplete:
			p.delivered.Add(1)
			return
		case webhook.OutcomeDead:
			p.deadLetter(ctx, d, attempt, res)
			return
		}
		if webhook.RetryExhausted(attempt, first, time.Now(), policy) {
			p.deadLetter(ctx, d, attempt, res)
			return
		}
		select {
		case <-ctx.Done():
			p.deadLetter(ctx, d, attempt, res)
			return
		case <-time.After(webhook.NextBackoff(attempt, p.baseBackoff(), p.maxBackoff())):
		}
	}
}

// attempt makes one delivery attempt. Every attempt of one notification replays the SAME bytes under the
// SAME id — a retry is the same event, so a receiver deduplicating on the id sees at-least-once delivery
// rather than N distinct notifications.
//
// CONTRACT (D12, same source): "Notifications SHOULD include a timestamp. The webhook SHOULD reject
// notifications that are too old", and "consider using unique, single-use identifiers (for example, JWT's
// `jti` claim or event IDs)". The receiver must "rigorously verify the authenticity of incoming
// notification requests" — HMAC is one of the named methods, so the config's token keys an HMAC-SHA-256
// over the EXACT bytes (webhook.Verify is the receiver-side routine) and also rides PushTokenHeader for the
// simple client-side comparison the `token` field describes. Without a token there is no authenticity at
// all, only the replay metadata; that is the client's choice to make and it is not silently upgraded.
func (p *WebhookPusher) attempt(ctx context.Context, d pushDelivery, n int) webhook.Result {
	ts := time.Now()
	headers := map[string]string{
		"User-Agent":            "palai-a2a-push/1",
		webhook.HeaderID:        d.id,
		webhook.HeaderTimestamp: strconv.FormatInt(ts.Unix(), 10),
		webhook.HeaderAttempt:   strconv.Itoa(n),
		"Content-Type":          "application/json",
	}
	if d.cfg.Token != "" {
		for k, v := range webhook.NewSigner([]byte(d.cfg.Token)).Headers(d.id, ts, n, d.body) {
			headers[k] = v
		}
		headers[PushTokenHeader] = d.cfg.Token
	}
	return p.sender.Deliver(ctx, webhook.Destination{
		URL:          d.cfg.URL,
		AllowPrivate: p.Policy.AllowPrivate,
		TimeoutMS:    p.timeoutMS(),
		Headers:      headers,
		// Redirect REVALIDATION, not a blanket deny (D12): each hop re-runs the egress gate, and
		// RedirectCheck re-applies the host allowlist because a redirect changes the host AFTER vet().
		MaxRedirects:  maxPushRedirects,
		RedirectCheck: p.hostAllowed,
	}, d.body)
}

// deadLetter records a terminal push failure. It touches NOTHING else: the canonical run/task result is
// already durable and a failed notification never rewrites it (the SLK-006 invariant).
func (p *WebhookPusher) deadLetter(ctx context.Context, d pushDelivery, attempts int, res webhook.Result) {
	p.dead.Add(1)
	if p.DeadLetter == nil {
		return
	}
	p.DeadLetter(ctx, PushFailure{
		ConfigID: d.cfg.ID, NotificationID: d.id, Attempts: attempts,
		StatusCode: res.StatusCode, Err: res.Err,
	})
}

// newNotificationID mints the single-use notification id (the D12 nonce). It is minted once per
// notification and reused across that notification's attempts.
func newNotificationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a monotonic fallback keeps ids unique rather than empty.
		return "a2apush_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "a2apush_" + hex.EncodeToString(b[:])
}

func (p *WebhookPusher) acquire() bool {
	p.semOne.Do(func() {
		n := p.Policy.MaxInFlight
		if n <= 0 {
			n = defaultPushMaxInFlight
		}
		p.sem = make(chan struct{}, n)
	})
	select {
	case p.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (p *WebhookPusher) release() { <-p.sem }

func (p *WebhookPusher) maxAttempts() int {
	if p.Policy.MaxAttempts > 0 {
		return p.Policy.MaxAttempts
	}
	return defaultPushMaxAttempts
}

func (p *WebhookPusher) retryWindow() time.Duration {
	if p.Policy.RetryWindow > 0 {
		return p.Policy.RetryWindow
	}
	return defaultPushRetryWindow
}

func (p *WebhookPusher) baseBackoff() time.Duration {
	if p.Policy.BaseBackoff > 0 {
		return p.Policy.BaseBackoff
	}
	return defaultPushBaseBackoff
}

func (p *WebhookPusher) maxBackoff() time.Duration {
	if p.Policy.MaxBackoff > 0 {
		return p.Policy.MaxBackoff
	}
	return defaultPushMaxBackoff
}

func (p *WebhookPusher) timeoutMS() int {
	if p.Policy.TimeoutMS > 0 {
		return p.Policy.TimeoutMS
	}
	return defaultPushTimeoutMS
}
