package api

import (
	"context"
	"io"
	"net/http"
	"strconv"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// ArtifactAPI is the store seam for the artifact retrieval surface (E13 Task 5, DAT-006/MCI-004): the
// never-opened READ half of the E09 write-path. The internal/artifacts Reader implements it over the
// durable, tenant-scoped artifacts rows and the control-plane-only object store (spec §24 — the S3
// credential never leaves that boundary). Tiers that never serve artifacts pass nil so the routes stay
// unmounted. Every method is scoped by the verified identity, never a request field — a wrong-tenant or
// unknown id is an indistinguishable miss (NotFound), so the surface leaks zero cross-tenant existence
// (§22.6 non-disclosure).
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

type artifactHandler struct {
	artifacts ArtifactAPI
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
