// Package egress is the shared SSRF-defense layer every outbound-HTTP caller vets through: the webhook
// sender, the web-research tool, and (later) the remote HTTP + MCP transports. The logic was lifted
// verbatim from the webhook sender (AUT-012) so there is ONE copy of the resolve→vet→pin idiom, not a
// per-consumer reimplementation. It is pure stdlib (net, net/url, context), so packages/ stays
// dependency-light and a tool need not import the webhook adapter to reach a public URL safely.
package egress

import (
	"context"
	"fmt"
	"net"
	"net/url"

	"errors"
)

// ErrDenied marks a destination the egress policy blocked (SSRF defense, AUT-012). Callers wrap it and
// match with errors.Is so a denial is classified terminal (never retried) up the chain.
var ErrDenied = errors.New("egress: destination denied by policy")

// Resolver is the DNS seam the dialer re-resolves through on every connect. Production uses
// net.DefaultResolver; a test injects a resolver that flips a name to prove rebinding is closed.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// specialUseCIDRs are ranges that are NEVER a valid egress destination and stay denied even under the
// allowPrivate flag: RFC6598 CGNAT (Alibaba metadata lives at 100.100.100.200), IETF protocol
// assignments, benchmarking, and future-use space. net.IP.IsPrivate/IsLinkLocal* miss these.
var specialUseCIDRs = parseCIDRs(
	"100.64.0.0/10", // RFC6598 shared address space / CGNAT (Alibaba metadata)
	"192.0.0.0/24",  // RFC6890 IETF protocol assignments
	"198.18.0.0/15", // RFC2544 benchmarking
	"240.0.0.0/4",   // RFC1112 reserved for future use
)

func parseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// VetIP is the single IP egress decision. Unspecified, multicast, link-local (which includes the
// 169.254.169.254 cloud-metadata address), and the special-use ranges above are NEVER allowed — not
// even under allowPrivate, because no legitimate egress reaches them. Loopback and private/ULA ranges
// are denied by default and opened only for a self-host destination via the explicit allowPrivate flag.
func VetIP(ip net.IP, allowPrivate bool) error {
	if ip == nil {
		return fmt.Errorf("%w: unparseable IP", ErrDenied)
	}
	switch {
	case ip.IsUnspecified(), ip.IsMulticast(), ip.IsLinkLocalUnicast(), ip.IsInterfaceLocalMulticast():
		return fmt.Errorf("%w: %s is a reserved/metadata address", ErrDenied, ip)
	}
	for _, n := range specialUseCIDRs {
		if n.Contains(ip) {
			return fmt.Errorf("%w: %s is a special-use address", ErrDenied, ip)
		}
	}
	if allowPrivate {
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return fmt.Errorf("%w: %s is a private/loopback address", ErrDenied, ip)
	}
	return nil
}

// VetURL enforces the static egress policy: the scheme must be https (http is a downgrade allowed only
// for a self-host destination via allowPrivate), and a literal-IP host must pass the same IP vet the
// per-connect dialer applies. A hostname is NOT resolved here — this is the cheap static gate on the hot
// path; hostname resolution is vetted for real at connect time by PinnedDialer (the authoritative layer)
// and, fail-fast, by VetResolved.
func VetURL(rawURL string, allowPrivate bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse destination: %w", err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowPrivate {
			return fmt.Errorf("%w: http is not allowed for a public destination (use https)", ErrDenied)
		}
	default:
		return fmt.Errorf("%w: scheme %q not allowed", ErrDenied, u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%w: empty host", ErrDenied)
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		return VetIP(ip, allowPrivate)
	}
	return nil
}

// VetResolved is the fail-fast gate: the static VetURL check PLUS, when the host is a name, a DNS
// resolution whose every answer is vetted — so a hostname that ALREADY points at a
// private/loopback/metadata range is rejected early rather than only at connect. Resolution failure is
// NOT a rejection: DNS can rebind, so the authoritative check is PinnedDialer at connect time; this only
// rejects a name it can prove resolves internal. A nil resolver uses net.DefaultResolver.
func VetResolved(ctx context.Context, resolver Resolver, rawURL string, allowPrivate bool) error {
	if err := VetURL(rawURL, allowPrivate); err != nil {
		return err
	}
	u, _ := url.Parse(rawURL) // VetURL already proved it parses
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return nil // a literal IP was already vetted by VetURL
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil // cannot resolve now — connect-time PinnedDialer is the authoritative gate
	}
	for _, a := range addrs {
		if err := VetIP(a.IP, allowPrivate); err != nil {
			return fmt.Errorf("%w: host resolves to a blocked address", ErrDenied)
		}
	}
	return nil
}

// PinnedDialer wraps a low-level dial with the authoritative connect-time gate: it re-resolves the host
// through the resolver, vets every candidate IP, and dials the FIRST vetted IP by address — never
// re-resolving the hostname. This is the resolve→vet→connect-the-same-IP idiom that closes the
// DNS-rebinding TOCTOU (AUT-012): a name that flips to an internal IP between attempts is denied on the
// attempt it flips, before any connection is made. A literal-IP addr is vetted and dialed directly.
func PinnedDialer(resolver Resolver, allowPrivate bool, dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var candidates []net.IP
		if ip := net.ParseIP(host); ip != nil {
			candidates = []net.IP{ip}
		} else {
			resolved, err := resolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, a := range resolved {
				candidates = append(candidates, a.IP)
			}
		}
		for _, ip := range candidates {
			if VetIP(ip, allowPrivate) == nil {
				return dial(ctx, network, net.JoinHostPort(ip.String(), port))
			}
		}
		return nil, ErrDenied
	}
}
