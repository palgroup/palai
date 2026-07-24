package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/palgroup/palai/packages/egress"
)

// This file is the A2A 1.0 CLIENT (E17 Task 3, spec §38.5-38.7): the OUTBOUND half that dials a registered
// remote A2A agent as an external child-run executor or a tool-like specialist. It is SECURITY-CRITICAL and
// its whole reason to exist is the trust boundary — a remote agent is UNTRUSTED:
//
//   * SSRF (crown): every card/endpoint retrieval is vetted through packages/egress (VetURL static gate +
//     PinnedDialer authoritative connect-time gate that closes the DNS-rebinding TOCTOU). A redirect FORCES
//     revalidation — the new target is re-vetted through egress and refused if it points internal (169.254.*
//     metadata, loopback, RFC1918). A remote card/endpoint at a private/internal address is REFUSED.
//   * NO credential inheritance (A2A-005/SUB-007): the ONLY outbound Authorization is the remote connection's
//     OWN credential, redeemed from its auth_connection_ref secret_ref HANDLE at call time. There is
//     structurally no field or parameter through which the parent run's or the platform's token could reach
//     this client — a remote agent can never read or inherit the caller's credential.
//   * UNTRUSTED output (A2A-005): a remote reply is DATA. RemoteResult carries Output/Artifacts the caller
//     places in the tool-result layer; the type has NO field through which a remote could instruct the parent
//     or grant a capability. Output is size-bounded like any untrusted ingress.
//   * Extension-URI allowlist: a remote card advertising an A2A extension outside the registered agent's
//     allowlist is REFUSED (card-poisoning containment).
//   * Version negotiation (§38.7): a card that no longer advertises the pinned protocol version fails
//     EXPLICITLY — the client never silently degrades to a version it did not negotiate.
//   * Pushed file (A2A-004 client half): an inbound remote file part is ingested + scanned into a stored
//     artifact (never a privileged instruction); a file part with no Files sink is REFUSED, never dropped.
//
// HONEST CEILING (§6): the loopback interop drives this client against the ON-MAIN T2 server in the SAME repo
// — a real A2A 1.0 exchange, but NOT a foreign-peer interop claim (that is §6 leg 2), so a2a stays "preview".

// clientVersions is the set of A2A protocol versions this client speaks. A registered agent's pinned version
// AND its live card must both name one of these, or negotiation fails explicitly (§38.7). Today: 1.0 only.
var clientVersions = map[string]bool{ProtocolVersion: true}

// Client errors. They wrap egress.ErrDenied where the denial is an egress-policy one, so a caller classifies
// an SSRF refusal terminal via errors.Is(err, egress.ErrDenied); the A2A-specific refusals are their own
// sentinels a caller can match.
var (
	// ErrVersionUnsupported is an explicit version-negotiation failure (§38.7): the remote card does not
	// advertise the pinned version this client negotiated, or the pin is a version this client does not speak.
	ErrVersionUnsupported = errors.New("a2a client: unsupported protocol version")
	// ErrExtensionNotAllowed is a card-poisoning refusal: the remote card advertises an extension URI outside
	// the registered agent's allowlist.
	ErrExtensionNotAllowed = errors.New("a2a client: remote extension URI not on the allowlist")
	// ErrNoSecretResolver marks a dispatch that needs the remote connection's credential but has no resolver
	// wired — it fails closed rather than dialing unauthenticated or (never) reaching for the parent token.
	ErrNoSecretResolver = errors.New("a2a client: agent needs an auth connection but no secret resolver is wired")
	// ErrFileDropWouldOccur marks a pushed remote file part with no Files sink to scan+store it. Failing
	// (rather than silently dropping) mirrors the server half's M-6 posture — a hidden data-loss path is worse.
	ErrFileDropWouldOccur = errors.New("a2a client: remote pushed a file part but no scanning ingest sink is wired")
	// ErrRemoteProtocol marks a malformed or error remote response.
	ErrRemoteProtocol = errors.New("a2a client: remote protocol error")
)

const (
	maxCardBytes   = 256 * 1024 // an Agent Card is small; bound the untrusted card body
	defaultTimeout = 30 * time.Second
	maxRedirects   = 5
	trustUntrusted = "untrusted" // the fixed trust class of every remote reply (A2A-005)
)

// RemoteAgent is a registered outbound remote A2A agent (a2a_remote_agents, §38.5): the trust envelope the
// client enforces on every dial. AuthConnectionRef is a secret_ref HANDLE for the remote connection's OWN
// credential — the parent/platform token is DELIBERATELY not a field here (A2A-005/SUB-007: no inheritance).
type RemoteAgent struct {
	ID                   string
	Organization         string
	Project              string
	Name                 string
	CardURL              string
	Endpoint             string // the interface base; message:send lives at Endpoint+"/message:send"
	ProtocolVersion      string // the negotiated pin; a card must still advertise it or dispatch fails (§38.7)
	AuthConnectionRef    string
	AllowedInputModes    []string
	AllowedOutputModes   []string
	AllowedExtensionURIs []string
	DataPolicy           string
	MaxCostCents         int
	TimeoutMS            int
	MaxOutputBytes       int
}

// RemoteSecretResolver redeems a remote connection's auth secret_ref handle for its bearer bytes at call time
// (the MCP SecretResolver pattern). It is scoped to (org, ref): it can resolve ONLY a secret the tenant owns,
// and it is the SOLE source of the outbound Authorization. There is no path to hand the client any other
// credential — that is the structural no-credential-inheritance guarantee (A2A-005/SUB-007).
type RemoteSecretResolver func(org, ref string) ([]byte, error)

// ClientConfig wires the client. Resolver/Dial/AllowPrivate mirror the MCP HTTP transport egress knobs;
// AllowPrivate is the test-harness-only self-host flag (production leaves it false, so loopback/RFC1918 are
// refused). Files ingests + scans a pushed remote file part (A2A-004 client half); nil ⇒ a file part is
// refused, never dropped.
type ClientConfig struct {
	Secrets      RemoteSecretResolver
	Files        Files
	Resolver     egress.Resolver
	Dial         func(ctx context.Context, network, addr string) (net.Conn, error)
	AllowPrivate bool
	Clock        func() time.Time
}

// Client is the A2A 1.0 outbound client. It holds no per-agent state — each method takes the RemoteAgent so a
// remote task id is only ever routed to the SAME agent's endpoint (remote task ids are connection-scoped).
type Client struct {
	cfg ClientConfig
}

func NewClient(cfg ClientConfig) *Client { return &Client{cfg: cfg} }

func (c *Client) now() time.Time {
	if c.cfg.Clock != nil {
		return c.cfg.Clock()
	}
	return time.Now()
}

// RemoteRequest is one dispatch to a remote agent. Objective is the MINIMUM context the remote receives (per
// the data_policy 'minimum' pin): just the task text, no parent artifacts by default. RunID is the CANONICAL
// run this dispatch belongs to — it scopes any ingested pushed-file artifact, never anything the remote picks.
type RemoteRequest struct {
	RunID     string
	Objective string
}

// RemoteResult is the UNTRUSTED outcome of a remote dispatch (A2A-005). Output/Artifacts are DATA for the
// tool-result layer; TrustClass is fixed to "untrusted". The type has NO capability/instruction field — a
// remote can return text or artifacts, never a grant. RemoteTaskID/RemoteContextID are connection-scoped: they
// are meaningful ONLY against this same agent's endpoint (a client never resolves them globally).
type RemoteResult struct {
	RemoteTaskID        string
	RemoteContextID     string
	State               TaskState
	Output              string
	Artifacts           []Artifact
	IngestedArtifactIDs []string
	TrustClass          string
}

// FetchCard retrieves + validates a remote Agent Card (§38.5): it vets the card URL through egress, GETs it
// (redirects revalidated), negotiates the protocol version (§38.7 explicit fail), and refuses any advertised
// extension URI outside the agent's allowlist. It is the registration/refresh gate; a dispatch composes it.
func (c *Client) FetchCard(ctx context.Context, agent RemoteAgent) (Card, error) {
	resp, err := c.do(ctx, agent, http.MethodGet, agent.CardURL, nil)
	if err != nil {
		return Card{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Card{}, fmt.Errorf("%w: card http status %d", ErrRemoteProtocol, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCardBytes))
	if err != nil {
		return Card{}, fmt.Errorf("%w: read card: %v", ErrRemoteProtocol, err)
	}
	var card Card
	if err := json.Unmarshal(body, &card); err != nil {
		return Card{}, fmt.Errorf("%w: parse card: %v", ErrRemoteProtocol, err)
	}
	if err := negotiateVersion(agent, card); err != nil {
		return Card{}, err
	}
	if err := checkExtensions(agent, card); err != nil {
		return Card{}, err
	}
	return card, nil
}

// negotiateVersion is the client half of §38.7: the pinned version must be one this client speaks, and the
// live card must still advertise it (top-level protocolVersion OR any supported interface). Anything else is
// an EXPLICIT failure — the client never silently talks a version it did not negotiate.
func negotiateVersion(agent RemoteAgent, card Card) error {
	pin := agent.ProtocolVersion
	if pin == "" {
		pin = ProtocolVersion
	}
	if !clientVersions[pin] {
		return fmt.Errorf("%w: this client does not speak %q", ErrVersionUnsupported, pin)
	}
	if card.ProtocolVersion == pin {
		return nil
	}
	for _, iface := range card.SupportedInterfaces {
		if iface.ProtocolVersion == pin {
			return nil
		}
	}
	return fmt.Errorf("%w: remote card advertises %q, not the negotiated %q", ErrVersionUnsupported, card.ProtocolVersion, pin)
}

// checkExtensions refuses a card advertising an extension URI outside the agent's allowlist (card-poisoning
// containment). An empty allowlist means NO extensions are permitted — a required extension the client cannot
// honor is a refusal, never a silent accept.
func checkExtensions(agent RemoteAgent, card Card) error {
	allowed := make(map[string]bool, len(agent.AllowedExtensionURIs))
	for _, u := range agent.AllowedExtensionURIs {
		allowed[u] = true
	}
	for _, ext := range card.Capabilities.Extensions {
		if !allowed[ext.URI] {
			return fmt.Errorf("%w: %q", ErrExtensionNotAllowed, ext.URI)
		}
	}
	return nil
}

// SendMessage dispatches an objective to the remote agent's message:send and returns the UNTRUSTED result. It
// vets the endpoint through egress, sends ONLY the objective (minimum context), redeems the remote
// connection's OWN credential, and ingests + scans any pushed file part in the reply (A2A-004 client half).
func (c *Client) SendMessage(ctx context.Context, agent RemoteAgent, req RemoteRequest) (RemoteResult, error) {
	msg := Message{
		Role:      "user",
		Parts:     []Part{{Kind: "text", Text: req.Objective}},
		MessageID: c.mintID(),
	}
	resp, err := c.do(ctx, agent, http.MethodPost, agent.Endpoint+"/message:send", sendParams{Message: msg})
	if err != nil {
		return RemoteResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return RemoteResult{}, fmt.Errorf("%w: message:send http status %d", ErrRemoteProtocol, resp.StatusCode)
	}
	limit := agent.MaxOutputBytes
	if limit <= 0 {
		limit = maxCardBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)))
	if err != nil {
		return RemoteResult{}, fmt.Errorf("%w: read reply: %v", ErrRemoteProtocol, err)
	}
	return c.parseReply(ctx, agent, req, body)
}

// GetTask reads a remote task by its connection-scoped id. The id is ONLY resolvable against the SAME agent's
// endpoint: routing it through a different RemoteAgent dials a different remote that never minted it. This is
// the structural enforcement of "remote task ids are connection-scoped" — the id is never a global key.
func (c *Client) GetTask(ctx context.Context, agent RemoteAgent, remoteTaskID string) (Task, error) {
	resp, err := c.do(ctx, agent, http.MethodGet, agent.Endpoint+"/tasks/"+remoteTaskID, nil)
	if err != nil {
		return Task{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Task{}, fmt.Errorf("%w: tasks GET http status %d", ErrRemoteProtocol, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCardBytes))
	if err != nil {
		return Task{}, fmt.Errorf("%w: read task: %v", ErrRemoteProtocol, err)
	}
	var task Task
	if err := json.Unmarshal(body, &task); err != nil {
		return Task{}, fmt.Errorf("%w: parse task: %v", ErrRemoteProtocol, err)
	}
	return task, nil
}

// parseReply turns a message:send reply (a Task or a direct Message) into an UNTRUSTED RemoteResult, ingesting
// + scanning any file part it carries. A file part with no Files sink is REFUSED (never dropped).
func (c *Client) parseReply(ctx context.Context, agent RemoteAgent, req RemoteRequest, body []byte) (RemoteResult, error) {
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return RemoteResult{}, fmt.Errorf("%w: parse reply: %v", ErrRemoteProtocol, err)
	}
	res := RemoteResult{TrustClass: trustUntrusted}
	switch probe.Kind {
	case "message": // a direct, synchronously-complete reply
		var msg Message
		if err := json.Unmarshal(body, &msg); err != nil {
			return RemoteResult{}, fmt.Errorf("%w: parse message reply: %v", ErrRemoteProtocol, err)
		}
		res.State = TaskStateCompleted
		res.RemoteContextID = msg.ContextID
		res.Output = textOf(msg.Parts)
		return res, nil
	default: // a Task (kind "task", or an empty/unknown kind we treat conservatively as a trackable task)
		var task Task
		if err := json.Unmarshal(body, &task); err != nil {
			return RemoteResult{}, fmt.Errorf("%w: parse task reply: %v", ErrRemoteProtocol, err)
		}
		res.RemoteTaskID = task.ID
		res.RemoteContextID = task.ContextID
		res.State = task.Status.State
		res.Artifacts = task.Artifacts
		res.Output = artifactText(task.Artifacts)
		ids, err := c.ingestPushedFiles(ctx, agent, req, task.Artifacts)
		if err != nil {
			return RemoteResult{}, err
		}
		res.IngestedArtifactIDs = ids
		return res, nil
	}
}

// ingestPushedFiles scans + stores every inbound file part as an artifact under the CANONICAL run (A2A-004
// client half). The raw bytes never become an instruction — they land as a scanned artifact the run may read
// as tool-result data. A file part with no Files sink is REFUSED, mirroring the server half's no-silent-drop.
func (c *Client) ingestPushedFiles(ctx context.Context, agent RemoteAgent, req RemoteRequest, artifacts []Artifact) ([]string, error) {
	var ids []string
	for _, a := range artifacts {
		for _, p := range a.Parts {
			if p.Kind != "file" || p.File == nil {
				continue
			}
			if c.cfg.Files == nil {
				return nil, ErrFileDropWouldOccur
			}
			id, err := c.cfg.Files.Ingest(ctx, agent.Organization, agent.Project, req.RunID, *p.File)
			if err != nil {
				return nil, fmt.Errorf("%w: scan/ingest pushed file: %v", ErrRemoteProtocol, err)
			}
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// do issues one outbound HTTP request under the full egress trust boundary: the URL is statically vetted
// (https-only unless AllowPrivate, no embedded credentials, literal-IP internal refused), the transport pins
// every connect to a re-resolved + vetted IP (DNS-rebinding TOCTOU closed), and every REDIRECT is revalidated
// through the same egress gate (an internal redirect target is refused, never followed). The ONLY
// Authorization is the remote connection's OWN redeemed credential — never the caller's.
func (c *Client) do(ctx context.Context, agent RemoteAgent, method, rawURL string, body any) (*http.Response, error) {
	if err := egress.VetURL(rawURL, c.cfg.AllowPrivate); err != nil {
		return nil, err
	}
	bearer, err := c.resolveBearer(agent)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%w: marshal request: %v", ErrRemoteProtocol, err)
		}
		rdr = bytes.NewReader(blob)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.httpClientFor(agent).Do(req)
	if err != nil {
		// Preserve the chain (%w) so an egress.ErrDenied — a rebinding name the pinned dialer refused, or a
		// redirect the revalidation refused — stays matchable as a terminal policy denial.
		return nil, fmt.Errorf("a2a client: request: %w", err)
	}
	return resp, nil
}

// httpClientFor builds a per-agent http.Client bounded by the agent's timeout pin, pinning every dial through
// egress and REVALIDATING every redirect: a redirect target is re-vetted through egress.VetURL and refused if
// it points internal (or embeds credentials, or downgrades scheme). maxRedirects bounds a redirect loop.
func (c *Client) httpClientFor(agent RemoteAgent) *http.Client {
	timeout := defaultTimeout
	if agent.TimeoutMS > 0 {
		timeout = time.Duration(agent.TimeoutMS) * time.Millisecond
	}
	dial := c.cfg.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	transport := &http.Transport{
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: timeout,
		DialContext:         egress.PinnedDialer(c.cfg.Resolver, c.cfg.AllowPrivate, dial),
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("%w: too many redirects", ErrRemoteProtocol)
			}
			// Revalidation: the redirect target routes through the SAME egress gate as the original — an
			// internal/credentialed/downgraded hop is refused here, before the pinned dialer even sees it.
			return egress.VetURL(req.URL.String(), c.cfg.AllowPrivate)
		},
	}
}

// resolveBearer redeems the agent's auth_connection_ref for its bearer at call time. No ref ⇒ no auth (an
// unauthenticated remote). A ref with no resolver fails CLOSED (ErrNoSecretResolver) rather than dialing
// unauthenticated — and there is NO code path that could substitute the parent/platform token here.
func (c *Client) resolveBearer(agent RemoteAgent) (string, error) {
	if agent.AuthConnectionRef == "" {
		return "", nil
	}
	if c.cfg.Secrets == nil {
		return "", ErrNoSecretResolver
	}
	b, err := c.cfg.Secrets(agent.Organization, agent.AuthConnectionRef)
	if err != nil {
		return "", fmt.Errorf("a2a client: resolve auth connection: %w", err)
	}
	return string(b), nil
}

func (c *Client) mintID() string {
	return fmt.Sprintf("a2acli_%d", c.now().UnixNano())
}

// textOf / artifactText concatenate the untrusted text parts of a reply into the plain-text output.
func textOf(parts []Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == "text" && p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func artifactText(artifacts []Artifact) string {
	var b strings.Builder
	for _, a := range artifacts {
		if t := textOf(a.Parts); t != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(t)
		}
	}
	return b.String()
}
