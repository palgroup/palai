package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// maxExcerpt bounds the response body captured for the sanitized attempt view (spec §21.6): a
// misbehaving receiver cannot make an attempt row unbounded, and the excerpt carries no secret.
const maxExcerpt = 2048

// errRedirectDenied is returned from the client's CheckRedirect so a 3xx never follows its Location
// (spec §21.6) and the sender can classify it as a terminal deny rather than a retryable error.
var errRedirectDenied = errors.New("webhook: redirect not followed")

// errEgressDenied marks a destination the egress policy blocked (SSRF defense, AUT-012).
var errEgressDenied = errors.New("webhook: destination denied by egress policy")

// Resolver is the DNS seam the sender re-resolves through on every attempt. Production uses
// net.DefaultResolver; a test injects a resolver that flips a name to prove rebinding is closed.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Destination is one attempt's target and policy. Headers already carries the signed §21.5 headers
// plus any resolved fixed headers; the sender does not sign (the pump composes the Signer).
type Destination struct {
	URL          string
	AllowPrivate bool // the endpoint's self-host egress allowlist flag (§21.4)
	TimeoutMS    int
	Headers      map[string]string
}

// Result is one attempt's sanitized outcome (spec §21.6). StatusCode is 0 when no HTTP response was
// received (a transport error). Terminal marks an egress/redirect denial that must never be retried.
type Result struct {
	StatusCode int
	DurationMS int64
	Excerpt    string
	Err        error
	Terminal   bool
}

// Sender performs one egress-safe HTTP delivery attempt (spec §21.6): a bounded timeout, redirects
// denied, DNS re-resolved through the egress policy per attempt, and the connection pinned to the
// vetted resolved IP so a rebind cannot swap the target between vet and connect (TOCTOU closed).
type Sender struct {
	resolver  Resolver
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	tlsConfig *tls.Config
	now       func() time.Time
}

// Option configures a Sender.
type Option func(*Sender)

// WithResolver injects the DNS resolver (default net.DefaultResolver).
func WithResolver(r Resolver) Option { return func(s *Sender) { s.resolver = r } }

// WithDialContext injects the low-level dialer (default net.Dialer); the sender always hands it a
// vetted resolved IP, never a hostname.
func WithDialContext(d func(ctx context.Context, network, addr string) (net.Conn, error)) Option {
	return func(s *Sender) { s.dial = d }
}

// WithTLSConfig injects a TLS config (a test trusts an httptest server's self-signed cert; production
// leaves it nil for the system roots).
func WithTLSConfig(c *tls.Config) Option { return func(s *Sender) { s.tlsConfig = c } }

// NewSender builds a sender with production defaults.
func NewSender(opts ...Option) *Sender {
	s := &Sender{resolver: net.DefaultResolver, dial: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Deliver performs one attempt. It vets the static URL, then POSTs the body over a per-attempt client
// whose transport re-resolves and pins the destination IP, denies redirects, and disables keep-alives
// (so every attempt re-resolves). A denied destination or a redirect is a terminal Result.
func (s *Sender) Deliver(ctx context.Context, dst Destination, body []byte) Result {
	start := s.now()
	if err := VetDestinationURL(dst.URL, dst.AllowPrivate); err != nil {
		return Result{Err: err, Terminal: true, DurationMS: s.elapsed(start)}
	}
	timeout := time.Duration(dst.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	transport := &http.Transport{
		DisableKeepAlives:   true, // force a fresh dial (and re-resolve) every attempt
		TLSClientConfig:     s.tlsConfig,
		TLSHandshakeTimeout: timeout,
		DialContext:         s.pinnedDial(dst.AllowPrivate),
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errRedirectDenied // never follow; the Location is never requested
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dst.URL, bytes.NewReader(body))
	if err != nil {
		return Result{Err: err, Terminal: true, DurationMS: s.elapsed(start)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "palai-webhooks/1")
	for k, v := range dst.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		// A denied redirect or a denied egress target is terminal; any other transport error retries.
		terminal := errors.Is(err, errRedirectDenied) || errors.Is(err, errEgressDenied)
		return Result{Err: unwrapURLError(err), Terminal: terminal, DurationMS: s.elapsed(start)}
	}
	defer resp.Body.Close()
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, maxExcerpt))
	return Result{StatusCode: resp.StatusCode, Excerpt: string(excerpt), DurationMS: s.elapsed(start)}
}

func (s *Sender) elapsed(start time.Time) int64 { return s.now().Sub(start).Milliseconds() }

// pinnedDial re-resolves the host through the injected resolver, vets every candidate IP against the
// egress policy, and dials the FIRST vetted IP by address — never re-resolving the hostname. This is
// the resolve→vet→connect-the-same-IP idiom that closes the DNS-rebinding TOCTOU (AUT-012).
func (s *Sender) pinnedDial(allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var candidates []net.IP
		if ip := net.ParseIP(host); ip != nil {
			candidates = []net.IP{ip}
		} else {
			resolved, err := s.resolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, a := range resolved {
				candidates = append(candidates, a.IP)
			}
		}
		for _, ip := range candidates {
			if vetIP(ip, allowPrivate) == nil {
				return s.dial(ctx, network, net.JoinHostPort(ip.String(), port))
			}
		}
		return nil, errEgressDenied
	}
}

// VetDestinationURL enforces the static egress policy (spec §21.4): only http(s), and a literal-IP
// host must pass the same IP vet the per-attempt dialer applies. A hostname is not resolved here —
// its resolution is vetted at connect time (VetDestinationURL guards create-time; pinnedDial guards
// attempt-time), so a name that resolves private is caught even if it looked benign at create.
func VetDestinationURL(rawURL string, allowPrivate bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse destination: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("%w: scheme %q not allowed", errEgressDenied, u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%w: empty host", errEgressDenied)
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		return vetIP(ip, allowPrivate)
	}
	return nil
}

// vetIP is the single IP egress decision. Unspecified, multicast, and link-local (which includes the
// 169.254.169.254 cloud-metadata address) are NEVER allowed — not even under the allowlist flag,
// because a webhook has no legitimate reason to reach them. Loopback and private/ULA ranges are
// denied by default and opened only for a self-host receiver via the explicit allowlist flag (§21.4).
func vetIP(ip net.IP, allowPrivate bool) error {
	if ip == nil {
		return fmt.Errorf("%w: unparseable IP", errEgressDenied)
	}
	switch {
	case ip.IsUnspecified(), ip.IsMulticast(), ip.IsLinkLocalUnicast(), ip.IsInterfaceLocalMulticast():
		return fmt.Errorf("%w: %s is a reserved/metadata address", errEgressDenied, ip)
	}
	if allowPrivate {
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return fmt.Errorf("%w: %s is a private/loopback address", errEgressDenied, ip)
	}
	return nil
}

// unwrapURLError strips the *url.Error envelope so the stored attempt error is the sanitized cause,
// not "Post \"<full url>\": ..." which would echo the destination into the attempt view.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// parseUnixHeader parses a Webhook-Timestamp header value into a time. Exported for the receiver-side
// verify path the SDK helper and the live smoke share.
func parseUnixHeader(v string) (time.Time, bool) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}
