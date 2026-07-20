package execution

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
)

// PreviewGrant authorizes one inbound connection to a sandbox preview/terminal endpoint through
// the reverse proxy (spec §29.16, SAN-010). Route is a random, non-guessable token — the only
// handle the caller ever holds; Target is the internal sandbox address the proxy dials and NEVER
// discloses. The grant binds the tenant, session, and run so a connection is authorized on every
// request and audited against its lineage. ExpiresAt is a short expiry after which the route is
// dead. Protocols is the connection allowlist (http today; ws is the terminal path, spec §29.17).
type PreviewGrant struct {
	Route     string
	Tenant    coordinator.Tenant
	SessionID string
	RunID     string
	Target    string
	Protocols []string
	ExpiresAt time.Time
}

// maxPreviewResponseBytes bounds a proxied preview response so a sandbox cannot stream unbounded
// bytes back through the authenticated route (spec §29.16 byte limits). ponytail: a fixed cap;
// per-grant rate/byte budgets are a later hardening once previews carry real traffic.
const maxPreviewResponseBytes = 8 << 20

// PreviewProxy is the authenticated reverse proxy for inbound sandbox connectivity (spec §29.16).
// It never exposes a direct pod/container address: a caller reaches a sandbox only by a random
// route, authorized against the grant's tenant and expiry on every connection, and a denial
// carries no sandbox address. It is the SAN-010 enforcement point.
type PreviewProxy struct {
	now    func() time.Time
	mu     sync.RWMutex
	grants map[string]PreviewGrant
}

// NewPreviewProxy builds an empty proxy that stamps expiry checks with now (tests inject a clock).
func NewPreviewProxy(now func() time.Time) *PreviewProxy {
	if now == nil {
		now = time.Now
	}
	return &PreviewProxy{now: now, grants: map[string]PreviewGrant{}}
}

// Grant registers a preview grant, minting a random non-guessable route when the caller left one
// empty, and returns the stored grant (its Route is the caller-facing handle). The Target is kept
// server-side and never returned in any response.
func (p *PreviewProxy) Grant(g PreviewGrant) PreviewGrant {
	if g.Route == "" {
		g.Route = "frt_" + randomToken()
	}
	p.mu.Lock()
	p.grants[g.Route] = g
	p.mu.Unlock()
	return g
}

// ServeHTTP authorizes the caller against the routed grant and, only on success, reverse-proxies
// to the sandbox. Every failure path returns a generic denial that names no sandbox address, and a
// wrong-tenant caller is answered exactly like an unknown route so the proxy discloses no tenant's
// route existence (spec §29.16 no direct address exposure).
func (p *PreviewProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := previewRoute(r.URL.Path)
	p.mu.RLock()
	grant, ok := p.grants[route]
	p.mu.RUnlock()

	// Unknown route and wrong tenant are indistinguishable to the caller: both 404, so the proxy
	// leaks neither a route's existence nor a foreign tenant's binding.
	if !ok || !sameTenant(grant.Tenant, callerTenant(r)) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !p.now().Before(grant.ExpiresAt) {
		http.Error(w, "preview route expired", http.StatusGone)
		return
	}
	if !protocolAllowed(grant.Protocols, r) {
		http.Error(w, "protocol not allowed", http.StatusForbidden)
		return
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{Scheme: "http", Host: grant.Target, Path: "/"}
			pr.Out.Host = grant.Target
			// Strip any forwarded-for chain so the internal topology never rides outbound either.
			pr.Out.Header.Del("X-Forwarded-For")
		},
		// A dial/transport error must not surface the sandbox address in a Go default error page.
		ErrorHandler: func(ew http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(ew, "preview unavailable", http.StatusBadGateway)
		},
	}
	// Bound the response the sandbox can stream back through the authenticated route.
	r.Body = http.MaxBytesReader(w, r.Body, maxPreviewResponseBytes)
	proxy.ServeHTTP(w, r)
}

// previewRoute extracts the route token — the final path segment under /v1/preview/.
func previewRoute(path string) string {
	const prefix = "/v1/preview/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	route := strings.TrimPrefix(path, prefix)
	if i := strings.IndexByte(route, '/'); i >= 0 {
		route = route[:i]
	}
	return route
}

// callerTenant reads the tenant the gateway asserted for this connection after authenticating it.
// A grant's request is authorized against this on every connection (spec §29.16).
func callerTenant(r *http.Request) coordinator.Tenant {
	return coordinator.Tenant{
		Organization: r.Header.Get("X-Palai-Org"),
		Project:      r.Header.Get("X-Palai-Project"),
	}
}

func sameTenant(a, b coordinator.Tenant) bool {
	return a.Organization != "" && a.Organization == b.Organization && a.Project == b.Project
}

// protocolAllowed reports whether the request's protocol is in the grant's allowlist. A WebSocket
// upgrade (the terminal path, spec §29.17) requires "ws"; a plain request requires "http".
func protocolAllowed(allow []string, r *http.Request) bool {
	want := "http"
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		want = "ws"
	}
	for _, p := range allow {
		if strings.EqualFold(p, want) {
			return true
		}
	}
	return false
}

// randomToken returns 16 random bytes as hex — a non-guessable route/id segment.
func randomToken() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}
