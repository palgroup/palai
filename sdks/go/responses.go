package palai

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CallOption tunes one request's transport controls (idempotency key, deadline, retry budget).
type CallOption func(*requestOptions)

// WithIdempotencyKey overrides the auto-generated stable idempotency key for a create.
func WithIdempotencyKey(k string) CallOption { return func(o *requestOptions) { o.idempotencyKey = k } }

// WithRequestTimeout overrides the total per-request deadline for one call.
func WithRequestTimeout(d time.Duration) CallOption {
	return func(o *requestOptions) { o.timeout = &d }
}

// WithRequestMaxRetries overrides the retry budget for one call.
func WithRequestMaxRetries(n int) CallOption { return func(o *requestOptions) { o.maxRetries = &n } }

// Responses is the /v1/responses resource group: create, retrieve, list, cancel, and stream.
type Responses struct{ client *Client }

// Create posts a new response and returns the queued Response handle. A stable Idempotency-Key is
// minted once per call and reused across transport retries, so a retried create settles exactly one
// response; WithIdempotencyKey supplies your own.
func (r *Responses) Create(ctx context.Context, req ResponseCreateRequest, opts ...CallOption) (*Response, error) {
	o := requestOptions{body: req, idempotencyKey: newIdempotencyKey()}
	for _, opt := range opts {
		opt(&o)
	}
	var resp Response
	if err := r.client.doJSON(ctx, http.MethodPost, "/v1/responses", o, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Retrieve reads a stored response. A 404 yields a NotFound *APIError; a 410 (retention-purged) a
// Gone *APIError — both carry the stable code and are inspectable via Class()/Code().
func (r *Responses) Retrieve(ctx context.Context, responseID string, opts ...CallOption) (*Response, error) {
	o := requestOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	var resp Response
	if err := r.client.doJSON(ctx, http.MethodGet, "/v1/responses/"+escapePathSegment(responseID), o, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// List returns a tenant-scoped page of run history (the shared opaque cursor + basic filters).
func (r *Responses) List(ctx context.Context, params ListParams, opts ...CallOption) (*Page[Response], error) {
	o := requestOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	var page Page[Response]
	if err := r.client.doJSON(ctx, http.MethodGet, listPath("/v1/responses", params), o, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// Cancel requests best-effort cancellation of an in-flight response (accepted as 202). Canceling is
// naturally idempotent, so it is safe to re-send after a network blip.
func (r *Responses) Cancel(ctx context.Context, responseID string, opts ...CallOption) error {
	o := requestOptions{idempotent: true}
	for _, opt := range opts {
		opt(&o)
	}
	return r.client.doJSON(ctx, http.MethodPost, "/v1/responses/"+escapePathSegment(responseID)+"/cancel", o, nil)
}

// Stream creates a response and returns a resumable, typed event stream over its session. The
// create fires eagerly (its POST shares Create's exact encoding, incl. the Idempotency-Key), then
// the returned stream reconnects with Last-Event-ID on a drop.
//
// ponytail: eager create (vs TS's lazy-on-first-consume) — a v0 ergonomic; the wire contract is
// identical. Make it lazy only if a caller needs to defer the create.
func (r *Responses) Stream(ctx context.Context, req ResponseCreateRequest, opts ...CallOption) (*ResponseStream, error) {
	created, err := r.Create(ctx, req, opts...)
	if err != nil {
		return nil, err
	}
	return &ResponseStream{
		client:        r.client,
		responseID:    created.ID,
		sessionID:     created.SessionID,
		maxReconnects: 5,
		backoffBaseMs: 100,
		backoffMaxMs:  5_000,
	}, nil
}

// listPath appends the shared list query to a path, in the fixed order (after, limit, status,
// created_after, created_before) the TS SDK's URLSearchParams emits — so cross-language routing is
// byte-identical. A zero/empty param adds no key.
func listPath(base string, p ListParams) string {
	var parts []string
	add := func(k, v string) { parts = append(parts, queryEscape(k)+"="+queryEscape(v)) }
	if p.After != "" {
		add("after", p.After)
	}
	if p.Limit != 0 {
		add("limit", strconv.Itoa(p.Limit))
	}
	if p.Status != "" {
		add("status", p.Status)
	}
	if p.CreatedAfter != "" {
		add("created_after", p.CreatedAfter)
	}
	if p.CreatedBefore != "" {
		add("created_before", p.CreatedBefore)
	}
	if len(parts) == 0 {
		return base
	}
	return base + "?" + strings.Join(parts, "&")
}
