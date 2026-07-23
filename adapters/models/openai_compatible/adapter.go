// Package openaicompatible is a generic OpenAI-compatible ChatCompletions adapter
// with an ACTIVE capability probe. The wire conversion is provider-one PARAMETRIZED
// by base URL (embedded, not re-copied) — the same text/stream/tool/schema fold, so
// there is exactly one ChatCompletions implementation in the tree. What this family
// adds is admission: before a run, the endpoint's observed capabilities
// (streaming / tool-calling / strict-JSON) are probed and cached in process, and a
// run that hard-requires a capability the endpoint lacks is REJECTED PRE-run with a
// clear error rather than failing mid-stream (spec §27.5, MOD-002).
package openaicompatible

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// ErrCapabilityUnsupported is the admission rejection: the endpoint does not support
// a capability the run hard-requires. It is returned PRE-run — the run's completion is
// never sent (the capability probe itself does reach the endpoint).
var ErrCapabilityUnsupported = errors.New("capability_unsupported")

// defaultProbeTTL is how long a probed capability record is trusted before a re-probe.
const defaultProbeTTL = 5 * time.Minute

// probeTimeout bounds a single capability probe so a dead endpoint can't hang admission.
const probeTimeout = 10 * time.Second

// probeMaxTokens is the output ceiling on a probe request. It is not 1: a real endpoint
// rejects a request whose ceiling is too low to emit any content ("max_tokens too low"),
// which would misreport a supported feature as unsupported. A small ceiling keeps the
// probe cheap while leaving room for the endpoint to accept and answer.
const probeMaxTokens = 16

// CapabilityRecord is what an OpenAI-compatible endpoint was observed to support.
// LastValidated stamps when the probe ran; a stale record is re-probed (never used
// unlabelled, spec §27.5).
type CapabilityRecord struct {
	Streaming      bool      `json:"streaming"`
	ToolCalls      bool      `json:"tool_calls"`
	StructuredJSON bool      `json:"structured_json"`
	LastValidated  time.Time `json:"last_validated"`
}

func (c CapabilityRecord) stale(ttl time.Duration, now time.Time) bool {
	return c.LastValidated.IsZero() || now.Sub(c.LastValidated) > ttl
}

// Adapter is provider-one's ChatCompletions conversion (embedded) plus an active
// capability probe. A nil Prober is materialized on first use with process defaults.
type Adapter struct {
	providerone.Adapter
	Prober *Prober
}

// Execute probes the endpoint's capabilities (cold or stale cache), rejects a run that
// hard-requires a capability the endpoint lacks BEFORE any provider call, then delegates
// the actual run to the embedded provider-one conversion.
func (a Adapter) Execute(ctx context.Context, req modelbroker.Request, secret string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	prober := a.Prober
	if prober == nil {
		prober = defaultProber()
	}
	record, err := prober.Probe(ctx, a.effectiveBaseURL(), secret, req.Model)
	if err != nil {
		return modelbroker.Result{}, fmt.Errorf("capability probe: %w", err)
	}
	if err := admit(a.effectiveBaseURL(), req, record); err != nil {
		return modelbroker.Result{}, err
	}
	return a.Adapter.Execute(ctx, req, secret, onDelta)
}

func (a Adapter) effectiveBaseURL() string {
	if a.Adapter.BaseURL != "" {
		return a.Adapter.BaseURL
	}
	return providerone.DefaultBaseURL
}

// admit reports the first hard requirement the record does not satisfy. A run streams
// unconditionally and hard-requires tool-calling when it advertises or forces tools and
// strict-JSON when it constrains output to a schema; each is checked before the run.
func admit(baseURL string, req modelbroker.Request, rec CapabilityRecord) error {
	if !rec.Streaming {
		return reject(baseURL, "streaming", rec)
	}
	if (len(req.Tools) > 0 || req.ForceToolCall) && !rec.ToolCalls {
		return reject(baseURL, "tool_calling", rec)
	}
	if req.OutputSchema != nil && !rec.StructuredJSON {
		return reject(baseURL, "strict_json_output", rec)
	}
	return nil
}

func reject(baseURL, capability string, rec CapabilityRecord) error {
	return fmt.Errorf("%w: endpoint %q does not support %s (probed %s)",
		ErrCapabilityUnsupported, redactBaseURL(baseURL), capability, rec.LastValidated.UTC().Format(time.RFC3339))
}

// redactBaseURL strips any credential-bearing query string or userinfo before an endpoint
// URL is embedded in an error or Result — a gateway may carry a token in the URL, and the
// error surfaces into logs and the run outcome (same credential-boundary care as the
// adapters' header discipline).
func redactBaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<endpoint>"
	}
	u.RawQuery = ""
	u.User = nil
	return u.String()
}

// Prober actively probes OpenAI-compatible endpoints and caches the result in process,
// keyed by base URL. ponytail: in-process cache keyed by base URL with a stale label —
// correct for a single-binary self-host; a persistent capability table / per-model keying
// is a later concern (E16 T6 routing, or 000035+ with owner approval).
type Prober struct {
	Client *http.Client
	TTL    time.Duration
	Now    func() time.Time

	mu      sync.Mutex
	records map[string]CapabilityRecord
}

// NewProber returns a Prober with process defaults (no-retry client, 5-minute TTL).
func NewProber() *Prober {
	return &Prober{records: map[string]CapabilityRecord{}}
}

// defaultProber backs an Adapter constructed without one. A package-level default keeps
// the in-process cache shared across broker copies of the same value-type Adapter.
var defaultProberOnce = sync.OnceValue(NewProber)

func defaultProber() *Prober { return defaultProberOnce() }

func (p *Prober) ttl() time.Duration {
	if p.TTL > 0 {
		return p.TTL
	}
	return defaultProbeTTL
}

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Prober) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{}
}

// Preload installs a capability record for baseURL, bypassing the network probe. An
// operator can declare a known endpoint's capabilities; tests use it to isolate the
// wire path from the probe. The record is stamped validated now.
func (p *Prober) Preload(baseURL string, rec CapabilityRecord) {
	rec.LastValidated = p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.records == nil {
		p.records = map[string]CapabilityRecord{}
	}
	p.records[baseURL] = rec
}

// Probe returns the endpoint's capability record, actively probing when the cache is
// cold or stale. The result is cached in process keyed by base URL.
func (p *Prober) Probe(ctx context.Context, baseURL, secret, model string) (CapabilityRecord, error) {
	now := p.now()
	p.mu.Lock()
	if rec, ok := p.records[baseURL]; ok && !rec.stale(p.ttl(), now) {
		p.mu.Unlock()
		return rec, nil
	}
	p.mu.Unlock()

	rec, err := p.observe(ctx, baseURL, secret, model)
	if err != nil {
		return CapabilityRecord{}, err
	}
	rec.LastValidated = p.now()

	p.mu.Lock()
	if p.records == nil {
		p.records = map[string]CapabilityRecord{}
	}
	p.records[baseURL] = rec
	p.mu.Unlock()
	return rec, nil
}

// observe sends one small probe request per capability and reads acceptance from the
// HTTP status: a 2xx means the endpoint accepted the feature-bearing request. ponytail:
// acceptance-based observation — an endpoint that accepts the tools field but silently
// never emits a tool_call is reported as supporting it; distinguishing that needs a real
// generation and is a §6 operator leg (real vLLM/Ollama). The local fake models the
// honest reject-on-unsupported-feature contract.
func (p *Prober) observe(ctx context.Context, baseURL, secret, model string) (CapabilityRecord, error) {
	base := []map[string]any{{"role": "user", "content": "ping"}}
	streaming, err := p.accepts(ctx, baseURL, secret, map[string]any{
		"model": model, "messages": base, "max_tokens": probeMaxTokens, "stream": true,
	})
	if err != nil {
		return CapabilityRecord{}, err
	}
	tools, err := p.accepts(ctx, baseURL, secret, map[string]any{
		"model": model, "messages": base, "max_tokens": probeMaxTokens,
		"tools": []map[string]any{{"type": "function", "function": map[string]any{
			"name": "__probe__", "parameters": map[string]any{"type": "object"},
		}}},
	})
	if err != nil {
		return CapabilityRecord{}, err
	}
	structured, err := p.accepts(ctx, baseURL, secret, map[string]any{
		"model": model, "messages": base, "max_tokens": probeMaxTokens,
		// A strict endpoint requires a well-formed schema: an object with a properties
		// map and additionalProperties:false (OpenAI 400s a strict json_schema missing
		// either), so a real strict-capable endpoint is not misreported as unsupported.
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{
			"name": "__probe__", "strict": true, "schema": map[string]any{
				"type": "object", "properties": map[string]any{}, "additionalProperties": false,
			},
		}},
	})
	if err != nil {
		return CapabilityRecord{}, err
	}
	return CapabilityRecord{Streaming: streaming, ToolCalls: tools, StructuredJSON: structured}, nil
}

// accepts POSTs one probe body and classifies the outcome: a 2xx means the endpoint
// SUPPORTS the feature; a 400/422 means it REJECTED this specific feature (unsupported);
// anything else (401/403/408/429/5xx, or a transport error) is a TRANSIENT/auth
// condition that is NOT a capability signal and returns an error — so a rate-limit or
// auth blip is never cached as an all-false record (which would turn a momentary blip
// into a full TTL of misleading "unsupported" rejects). ponytail: 400/422 == feature
// rejection is the acceptance-probe heuristic; an endpoint fronting a newer OpenAI model
// that needs max_completion_tokens 400s every probe → all-false → total reject. That
// (and a server that accepts the tools field but never emits a tool_call, e.g.
// vLLM/Ollama) is a §6 operator leg — a declared-capability override, not a wire fix.
func (p *Prober) accepts(ctx context.Context, baseURL, secret string, body map[string]any) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	raw, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(raw))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret) // sole use of the credential in a probe

	resp, err := p.client().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	// Drain a little so the connection can be reused; the body itself is not inspected.
	_, _ = resp.Body.Read(make([]byte, 512))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity:
		return false, nil // the endpoint rejected THIS feature — genuinely unsupported
	default:
		return false, fmt.Errorf("probe of %q got HTTP %d (transient/auth, not a capability signal)", redactBaseURL(baseURL), resp.StatusCode)
	}
}
