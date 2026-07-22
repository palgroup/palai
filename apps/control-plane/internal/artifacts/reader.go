package artifacts

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/storage"
)

// Reader is the artifact retrieval read-path (spec §22.6, E13 Task 5): the never-opened READ half of the
// E09 write-path. It exposes the durable, tenant-scoped artifacts rows over the public API and streams
// their bytes straight from the control-plane-only object store (spec §24 — the S3 credential never leaves
// this boundary). It implements api.ArtifactAPI. Every method scopes itself by the verified identity, so a
// wrong-tenant or unknown id reads no row and renders the non-disclosing 404.
type Reader struct {
	store *Store
	pool  *pgxpool.Pool
}

// NewReader binds the object store and the durable pool the retrieval read-path reads over — the same two
// halves the write-path Writer ties together.
func NewReader(store *Store, pool *pgxpool.Pool) *Reader {
	return &Reader{store: store, pool: pool}
}

// GetArtifact reads one artifact's metadata within the tenant scope. An unknown or foreign id returns no
// row, which renders as the non-disclosing 404.
func (rd *Reader) GetArtifact(ctx context.Context, scope middleware.Scope, id string) (api.ArtifactResult, error) {
	ctx = storage.ScopeToTenant(ctx, scope.Organization, scope.Project)
	m, found, err := rd.metadata(ctx, scope, id)
	if err != nil {
		return api.ArtifactResult{}, err
	}
	if !found {
		return api.ArtifactResult{NotFound: true}, nil
	}
	body, err := json.Marshal(m.projection())
	if err != nil {
		return api.ArtifactResult{}, fmt.Errorf("marshal artifact metadata: %w", err)
	}
	return api.ArtifactResult{Body: body}, nil
}

// OpenArtifactContent resolves an artifact within the tenant scope and opens its bytes for a streaming
// download. The DB read is the authoritative tenant gate: an unknown or foreign id reads no row and is the
// non-disclosing 404, never reaching the object store. A row whose object the store no longer holds (a
// retention delete racing the read) is surfaced as the same miss, not a half-open stream. The returned
// Reader streams from S3 — the whole object is never buffered in control-plane memory.
func (rd *Reader) OpenArtifactContent(ctx context.Context, scope middleware.Scope, id string) (api.ArtifactContent, error) {
	ctx = storage.ScopeToTenant(ctx, scope.Organization, scope.Project)
	m, found, err := rd.metadata(ctx, scope, id)
	if err != nil {
		return api.ArtifactContent{}, err
	}
	if !found {
		return api.ArtifactContent{NotFound: true}, nil
	}
	body, size, ok, err := rd.store.Open(ctx, m.objectKey)
	if err != nil {
		return api.ArtifactContent{}, err
	}
	if !ok {
		return api.ArtifactContent{NotFound: true}, nil
	}
	// A non-conformant or proxied S3 may omit ContentLength (size <= 0). Serve the RLS-admitted row's
	// size_bytes instead, so Content-Length matches the logical bytes — a zero length makes net/http drop
	// the body, returning a 200 + Content-Digest with no content.
	if size <= 0 {
		size = m.sizeBytes
	}
	return api.ArtifactContent{
		Reader:    body,
		SizeBytes: size,
		MediaType: m.mediaType,
		Digest:    digestHeader(m.checksum),
	}, nil
}

// ListRunArtifacts lists the artifacts a response's run produced, tenant-scoped. It first resolves the
// response to its run within the tenant (reusing the responses read-path's RunIDForResponse): an unknown
// or foreign response id reads no row and is the non-disclosing 404. A known response whose run produced no
// artifacts is an empty list, not a miss.
func (rd *Reader) ListRunArtifacts(ctx context.Context, scope middleware.Scope, responseID string) (api.ArtifactResult, error) {
	ctx = storage.ScopeToTenant(ctx, scope.Organization, scope.Project)
	var runID string
	err := rd.pool.QueryRow(ctx, storage.Query("RunIDForResponse"), responseID, scope.Organization, scope.Project).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ArtifactResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ArtifactResult{}, fmt.Errorf("resolve response run: %w", err)
	}
	rows, err := rd.pool.Query(ctx, storage.Query("ListArtifactsByRun"), runID, scope.Organization, scope.Project)
	if err != nil {
		return api.ArtifactResult{}, fmt.Errorf("list run artifacts: %w", err)
	}
	defer rows.Close()
	data := make([]any, 0)
	for rows.Next() {
		var m metadataRow
		if err := rows.Scan(&m.id, &m.runID, &m.sizeBytes, &m.checksum, &m.mediaType, &m.logicalType, &m.scanStatus, &m.createdAt); err != nil {
			return api.ArtifactResult{}, fmt.Errorf("scan run artifact: %w", err)
		}
		data = append(data, m.projection())
	}
	if err := rows.Err(); err != nil {
		return api.ArtifactResult{}, fmt.Errorf("iterate run artifacts: %w", err)
	}
	body, err := json.Marshal(map[string]any{"object": "list", "data": data})
	if err != nil {
		return api.ArtifactResult{}, fmt.Errorf("marshal artifact list: %w", err)
	}
	return api.ArtifactResult{Body: body}, nil
}

// metadataRow is one artifact's retrieval metadata scanned from the tenant-scoped read.
type metadataRow struct {
	id          string
	runID       string
	objectKey   string
	sizeBytes   int64
	checksum    string
	mediaType   string
	logicalType string
	scanStatus  string
	createdAt   time.Time
}

// metadata reads one artifact's row within the tenant scope. found is false for an unknown or foreign id
// (the tenant-scoped query returns no row), so a caller renders the same miss whether the artifact is
// absent or owned by another tenant — no cross-tenant existence leaks.
func (rd *Reader) metadata(ctx context.Context, scope middleware.Scope, id string) (metadataRow, bool, error) {
	m := metadataRow{id: id}
	err := rd.pool.QueryRow(ctx, storage.Query("ArtifactByID"), id, scope.Organization, scope.Project).
		Scan(&m.runID, &m.objectKey, &m.sizeBytes, &m.checksum, &m.mediaType, &m.logicalType, &m.scanStatus, &m.createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return metadataRow{}, false, nil
	}
	if err != nil {
		return metadataRow{}, false, fmt.Errorf("read artifact row: %w", err)
	}
	return m, true, nil
}

// projection is the artifact's public metadata JSON shape (§22.6): identity, the run it belongs to, its
// integrity fields, and its classification. object_key is deliberately NOT surfaced — the S3 layout is
// control-plane internal (spec §24), and a client downloads via /content, never by key.
func (m metadataRow) projection() map[string]any {
	return map[string]any{
		"id":                  m.id,
		"object":              "artifact",
		"run_id":              m.runID,
		"size_bytes":          m.sizeBytes,
		"checksum":            m.checksum,
		"media_type":          m.mediaType,
		"logical_type":        m.logicalType,
		"malware_scan_status": m.scanStatus,
		"created_at":          m.createdAt.UTC().Format(time.RFC3339Nano),
	}
}

// digestHeader renders the stored "sha256:<hex>" checksum as an RFC 9530 Content-Digest value
// (sha-256=:<base64>:), so a downloader can verify byte-integrity against the streamed bytes. A checksum
// that is not the expected "sha256:<hex>" shape yields "" and the header is then omitted rather than sent
// wrong (the write-path always records this shape).
func digestHeader(checksum string) string {
	hexsum, ok := strings.CutPrefix(checksum, "sha256:")
	if !ok {
		return ""
	}
	raw, err := hex.DecodeString(hexsum)
	if err != nil || len(raw) != 32 {
		// A malformed or legacy row (fewer than 32 bytes) would emit a garbage sha-256 digest; skip the
		// header instead of asserting integrity over the wrong length.
		return ""
	}
	return "sha-256=:" + base64.StdEncoding.EncodeToString(raw) + ":"
}
