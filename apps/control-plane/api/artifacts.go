package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// ArtifactAPI is the store seam for the artifact surface (E13 Task 5, DAT-006/MCI-004): the
// once-never-opened READ half of the E09 write-path, plus the ONE write verb below. The internal/artifacts
// Reader implements it over the durable, tenant-scoped artifacts rows and the control-plane-only object
// store (spec §24 — the S3 credential never leaves that boundary). Tiers that never serve artifacts pass
// nil so the routes stay unmounted. Every method is scoped by the verified identity, never a request field
// — a wrong-tenant or unknown id is an indistinguishable miss (NotFound), so the surface leaks zero
// cross-tenant existence (§22.6 non-disclosure).
type ArtifactAPI interface {
	// GetArtifact returns one artifact's metadata JSON (§22.6 classification + integrity fields), or
	// NotFound for an unknown or foreign id.
	GetArtifact(ctx context.Context, scope middleware.Scope, id string) (ArtifactResult, error)
	// OpenArtifactContent opens an artifact's bytes for a streaming download: on a hit the caller drains
	// and closes Reader. NotFound is an unknown/foreign id OR a row whose object the store no longer holds.
	OpenArtifactContent(ctx context.Context, scope middleware.Scope, id string) (ArtifactContent, error)
	// ListRunArtifacts lists the artifacts a response's run produced, tenant-scoped. NotFound is an
	// unknown or foreign response id (no existence disclosure); a known run with no artifacts is an empty
	// list, not a miss.
	ListRunArtifacts(ctx context.Context, scope middleware.Scope, responseID string) (ArtifactResult, error)
	// CreateInboundArtifact persists bytes an API CLIENT supplied, run-less, and returns the id a run's
	// `image_ref` content item names. mediaType is the caller's SNIFFED type — the handler sniffs, never
	// the client's Content-Type — and the id is MINTED HERE, never chosen by the client (see the handler).
	CreateInboundArtifact(ctx context.Context, scope middleware.Scope, content []byte, mediaType string) (ArtifactIngestResult, error)
}

// ArtifactIngestResult is a completed ingest: the minted id the caller puts in a run's input.
type ArtifactIngestResult struct {
	ID string
}

// ArtifactResult is a metadata or list projection: Body is the JSON written verbatim on a hit; NotFound
// renders the non-disclosing 404.
type ArtifactResult struct {
	Body     []byte
	NotFound bool
}

// ArtifactContent is a streaming download. On a hit Reader carries the object's bytes — the handler
// io.Copy-streams and closes it, never buffering the whole object in control-plane memory; SizeBytes sets
// Content-Length, MediaType the Content-Type, and Digest the RFC 9530 Content-Digest for byte-integrity.
// NotFound renders the non-disclosing 404 and carries a nil Reader.
type ArtifactContent struct {
	Reader    io.ReadCloser
	SizeBytes int64
	MediaType string
	Digest    string
	NotFound  bool
}

// defaultArtifactMediaType is the download Content-Type when the row recorded no media type: an opaque
// octet stream the client saves by name, never a type guessed from the bytes.
const defaultArtifactMediaType = "application/octet-stream"

// maxInboundArtifactBytes caps ONE ingested artifact.
//
// IT IS THIS SURFACE'S OWN COPY OF A CEILING THAT ALSO EXISTS DOWNSTREAM, deliberately, because
// execution.maxImageBytes is the LAST bound rather than the only one: that one refuses an oversized image
// when a run is already born and a human is already waiting, and it does so by dropping a marker into the
// conversation. Refusing at the door is a 413 the client can say something about, and it is also the only
// bound that keeps the bytes out of the object store in the first place. The two numbers are equal on
// purpose — an ingest ceiling ABOVE the dispatch ceiling would store bytes no run could ever read.
//
// The api package does not import internal/execution (the dependency runs the other way), so this is a
// literal rather than a shared constant; TestIngestCeilingMatchesDispatchCeiling pins the two together by
// reading execution's own source, so a change to either side reddens rather than drifts.
const maxInboundArtifactBytes = 5 << 20 // 5 MiB

// inboundArtifactMediaTypes is the closed set of things this surface accepts, keyed by SNIFFED media type.
//
// It is an ALLOW-LIST OVER SNIFFED BYTES, which is two decisions rather than one. Sniffing (rather than
// believing the request's Content-Type) is because a header is the client's word about bytes the server is
// holding — the same rule slack.FetchImage follows, and the reason a shell script sent as `image/png` is
// refused here. A closed list (rather than "anything that sniffs as image/*") is because the consumer is a
// vision endpoint with its own accepted set: this is the intersection of what an OpenAI-compatible vision
// API takes (PNG, JPEG, WEBP, non-animated GIF) and what execution.decodeContentParts will forward. A type
// outside it is refused at the door rather than stored and then rejected mid-run at the provider.
//
// THIS IS AN IMAGE INGEST, NOT A GENERAL FILE UPLOAD, and the narrowness is the point: a public write verb
// that accepted arbitrary bytes would be an object store with an API key in front of it. Widening it is a
// deliberate act that has to answer "which reader consumes this type?".
var inboundArtifactMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

type artifactHandler struct {
	artifacts ArtifactAPI
}

// create ingests bytes a client supplied and answers with the artifact id a run's input can name — the
// WRITE half the public API never had, and without which an `image_ref` content item is unreachable to
// anything outside this process (an integration relay that consumes Palai as a customer, e.g. the Slack
// bot, has no other way to put a screenshot in front of a model).
//
// THE ID IS MINTED HERE, NOT ACCEPTED FROM THE CLIENT, and that is the one place this surface deliberately
// differs from the in-process path it replaces (storage/queries/artifacts.sql's InsertInboundArtifact takes
// a caller-derived id). That id was derived because the OLD caller's idempotency reservation hashed the run
// input the id appears in, so a minted id would have turned a Slack redelivery into a CONFLICT instead of a
// replay. An API client's create carries its OWN Idempotency-Key covering its OWN body, so nothing here
// depends on the id being a pure function of anything — and a client-chosen primary key on a public write
// verb is a squatting surface that would have to be defended for no remaining benefit.
//
// THE BYTES ARE THE ONLY THING READ FROM THE BODY. No filename, no provenance, no classification is taken
// from the request: the row's provenance is written server-side (see the store), because provenance is read
// back by operators and served over the retrieval API, which is exactly where untrusted text must not be.
func (h *artifactHandler) create(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	// One byte past the ceiling, so an oversize body is DETECTED rather than silently truncated into a
	// corrupt image. Content-Length is not consulted: it is absent on a chunked request and is in any case
	// a claim, while this is a measurement.
	content, err := io.ReadAll(io.LimitReader(r.Body, maxInboundArtifactBytes+1))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return
	}
	if len(content) == 0 {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body carried no bytes")
		return
	}
	if len(content) > maxInboundArtifactBytes {
		middleware.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "payload_too_large",
			"an artifact may not exceed "+strconv.Itoa(maxInboundArtifactBytes)+" bytes")
		return
	}
	// SNIFFED, not declared: http.DetectContentType implements the WHATWG algorithm over the first 512
	// bytes and recognises the image families above by magic number. The request's own Content-Type is
	// never consulted — it is the client's word about bytes the server is holding.
	mediaType := strings.TrimSpace(strings.Split(http.DetectContentType(content), ";")[0])
	if !inboundArtifactMediaTypes[mediaType] {
		middleware.WriteProblem(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"an artifact's bytes must be a PNG, JPEG, GIF or WEBP image")
		return
	}
	out, err := h.artifacts.CreateInboundArtifact(r.Context(), scope, content, mediaType)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	body, err := json.Marshal(map[string]any{
		"id":         out.ID,
		"object":     "artifact",
		"media_type": mediaType,
		"size_bytes": len(content),
	})
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

// get returns an artifact's metadata within the verified scope. A hit is 200 with the store's JSON; an
// unknown or foreign id is 404 (never leaking a foreign artifact's existence).
func (h *artifactHandler) get(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.artifacts.GetArtifact(r.Context(), scope, r.PathValue("artifact_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if out.NotFound {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such artifact in this project")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Body)
}

// content streams an artifact's bytes from the object store within the verified scope. HONEST CEILING:
// this is an authenticated DIRECT streaming download — a pre-signed URL policy + expiry ceremony is E13-H
// (DAT-006's remainder). The bytes flow straight through io.Copy, so a large artifact never lands wholesale
// in control-plane memory. An unknown/foreign id, or a row whose object is gone, is the non-disclosing 404.
func (h *artifactHandler) content(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.artifacts.OpenArtifactContent(r.Context(), scope, r.PathValue("artifact_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if out.NotFound {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such artifact in this project")
		return
	}
	defer out.Reader.Close()
	mediaType := out.MediaType
	if mediaType == "" {
		mediaType = defaultArtifactMediaType
	}
	w.Header().Set("Content-Type", mediaType)
	if out.Digest != "" {
		w.Header().Set("Content-Digest", out.Digest)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(out.SizeBytes, 10))
	w.WriteHeader(http.StatusOK)
	// Stream straight through; the whole object is never buffered in memory. A copy failure mid-stream
	// cannot change the already-sent 200 — the Content-Digest is what lets the client detect a short read.
	_, _ = io.Copy(w, out.Reader)
}

// listForResponse lists the artifacts a response's run produced, within the verified scope. A hit is 200
// with the store's list JSON; an unknown or foreign response id is 404 (no existence disclosure). A known
// response whose run produced no artifacts renders 200 with an empty list.
func (h *artifactHandler) listForResponse(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.artifacts.ListRunArtifacts(r.Context(), scope, r.PathValue("response_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if out.NotFound {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such response in this project")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Body)
}
