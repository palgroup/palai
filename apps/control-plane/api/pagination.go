package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// The shared read/LIST pagination surface (E13 Task 4). Every list route reuses ONE opaque,
// tenant-bound cursor and ONE contracts.Page envelope, so the whole surface pages identically and
// a foreign cursor is rejected the same way everywhere (TEN-001 cursor-fuzz). No filter DSL — the
// filters are the two the plan names (status, created_at range); richer query is E17 knowledge.

const (
	defaultPageLimit = 20
	maxPageLimit     = 100
	// macLen truncates the HMAC-SHA256 tag the cursor carries. 16 bytes (128 bits) is ample to make
	// forging a valid cross-tenant cursor infeasible without shrinking the token needlessly.
	macLen = 16
)

// errBadCursor marks a pagination cursor that is malformed, truncated, tampered, or minted for a
// different tenant or resource kind. It is ALWAYS surfaced as a 400 invalid_cursor — a foreign
// cursor is an EXPLICIT reject, never a silently-empty page (the TEN-001 cursor-fuzz contract).
var errBadCursor = errors.New("api: invalid pagination cursor")

// statusFilterKinds are the list kinds that carry a lifecycle-state column ?status= can filter on. Every
// other list rejects ?status= (a silently-dropped filter would let a client act on unfiltered rows). The
// agent-revisions kind is profile-suffixed at the call site, so it is correctly absent here too.
var statusFilterKinds = map[string]bool{"responses": true, "sessions": true}

// listCursor is a keyset position: the (created_at, id) of the last row a page returned. A list
// orders by (created_at DESC, id DESC), so the next page is every row strictly before this point.
type listCursor struct {
	CreatedAt time.Time
	ID        string
}

// ListQuery is a resolved, tenant-SAFE list request a store read consumes. It never carries a
// tenant: under RLS (E13 T1) the request's published scope confines every row the store sees, so
// the query only needs the keyset position, the page size, and the two basic filters. The store
// fetches Limit+1 rows so the handler detects a further page without a second round trip.
type ListQuery struct {
	After      *listCursor
	Limit      int
	Status     string
	CreatedGTE *time.Time
	CreatedLTE *time.Time
}

// ListRow is one row a store list returns: its keyset coordinates plus the already-marshaled
// resource body the page envelope embeds verbatim. Keeping the body pre-marshaled lets each store
// reuse its resource's existing projection shape rather than re-deriving it in the handler.
type ListRow struct {
	ID        string
	CreatedAt time.Time
	Body      json.RawMessage
}

// cursorKeyVal is the process-wide HMAC key that binds a cursor to its minting process.
// ponytail: a process-random key means a cursor does not survive a restart or cross a replica —
// correct for the single-replica compose this phase targets (the same ceiling T7's in-process
// rate limiter documents). Upgrade path: derive it from a configured server secret when
// multi-replica pagination continuity is required.
var (
	cursorKeyOnce sync.Once
	cursorKeyVal  []byte
)

func cursorKey() []byte {
	cursorKeyOnce.Do(func() {
		cursorKeyVal = make([]byte, 32)
		if _, err := rand.Read(cursorKeyVal); err != nil {
			panic("api: cursor key entropy: " + err.Error())
		}
	})
	return cursorKeyVal
}

// encodeCursor mints an opaque, tenant-bound cursor for position c. The payload carries ONLY the
// keyset position — never the tenant — so the token discloses no tenant identity; the tenant, and
// the resource kind (so a cursor cannot be replayed on another list), are bound in by the HMAC,
// which decodeCursor recomputes from the REQUEST's scope. A cursor minted for tenant A therefore
// fails the MAC check when presented by tenant B, which is the explicit foreign-cursor reject.
func encodeCursor(key []byte, kind string, scope middleware.Scope, c listCursor) string {
	payload := cursorPayload(c)
	mac := cursorMAC(key, kind, scope, payload)
	return base64.RawURLEncoding.EncodeToString(append(mac, payload...))
}

// decodeCursor validates token against the request's scope and kind and returns its keyset
// position. Any mismatch — a foreign tenant, a foreign resource kind, a flipped byte, a truncated
// or non-base64 token — is errBadCursor: the cursor fails CLOSED, never yielding a wrong page.
func decodeCursor(key []byte, kind string, scope middleware.Scope, token string) (listCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < macLen {
		return listCursor{}, errBadCursor
	}
	mac, payload := raw[:macLen], raw[macLen:]
	if subtle.ConstantTimeCompare(mac, cursorMAC(key, kind, scope, payload)) != 1 {
		return listCursor{}, errBadCursor
	}
	return parseCursorPayload(payload)
}

// cursorPayload serializes the position as 8 big-endian nanosecond bytes followed by the id.
func cursorPayload(c listCursor) []byte {
	buf := make([]byte, 8, 8+len(c.ID))
	binary.BigEndian.PutUint64(buf, uint64(c.CreatedAt.UnixNano()))
	return append(buf, c.ID...)
}

func parseCursorPayload(payload []byte) (listCursor, error) {
	if len(payload) < 8 {
		return listCursor{}, errBadCursor
	}
	nanos := int64(binary.BigEndian.Uint64(payload[:8]))
	return listCursor{CreatedAt: time.Unix(0, nanos).UTC(), ID: string(payload[8:])}, nil
}

// cursorMAC binds the position to the tenant and resource kind. The kind, organization, and
// project are length-prefixed so ("a","b") and ("ab","") cannot collide into one MAC input.
func cursorMAC(key []byte, kind string, scope middleware.Scope, payload []byte) []byte {
	h := hmac.New(sha256.New, key)
	writeField(h, kind)
	writeField(h, scope.Organization)
	writeField(h, scope.Project)
	h.Write(payload)
	return h.Sum(nil)[:macLen]
}

func writeField(h hash.Hash, s string) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	h.Write(n[:])
	h.Write([]byte(s))
}

// beginList parses the shared pagination + filter query for a list route on resource kind,
// validating any cursor against the verified scope. A malformed/foreign cursor or filter is a 400
// (written here); ok=false means the caller returns. The +1 over-fetch is the store's job, not
// this parse's — Limit is the page size the caller asked for.
func beginList(w http.ResponseWriter, r *http.Request, kind string, scope middleware.Scope) (ListQuery, bool) {
	q := ListQuery{Limit: defaultPageLimit}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageLimit {
			middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "limit must be an integer between 1 and 100")
			return ListQuery{}, false
		}
		q.Limit = n
	}
	if v := r.URL.Query().Get("after"); v != "" {
		c, err := decodeCursor(cursorKey(), kind, scope, v)
		if err != nil {
			middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_cursor", "the pagination cursor is not valid for this tenant")
			return ListQuery{}, false
		}
		q.After = &c
	}
	// Forward-only pagination (contracts.Page advertises previous_cursor/before, but this surface never
	// populates them): a ?before= is rejected rather than silently ignored, so a client cannot believe it
	// paged backward. Backward pagination is YAGNI here.
	if r.URL.Query().Get("before") != "" {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "backward pagination is not supported")
		return ListQuery{}, false
	}
	// The status filter only exists on lists with a lifecycle-state column (responses, sessions). On any
	// other kind a ?status= is rejected, never silently dropped — a client that believes it filtered must
	// not act on unfiltered rows (review SHOULD 1).
	q.Status = r.URL.Query().Get("status")
	if q.Status != "" && !statusFilterKinds[kind] {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "status filtering is not supported for this resource")
		return ListQuery{}, false
	}
	var err error
	if q.CreatedGTE, err = parseTimeParam(r, "created_after"); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "created_after must be an RFC3339 timestamp")
		return ListQuery{}, false
	}
	if q.CreatedLTE, err = parseTimeParam(r, "created_before"); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "created_before must be an RFC3339 timestamp")
		return ListQuery{}, false
	}
	return q, true
}

// parseTimeParam reads an optional RFC3339 query parameter. Absent is (nil, nil); malformed is an
// error the caller maps to a 400.
func parseTimeParam(r *http.Request, name string) (*time.Time, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// renderPage trims the store's Limit+1 rows to the page, mints the next cursor for kind bound to
// scope, and writes the contracts.Page envelope. has_more is true exactly when the store returned
// more than Limit rows; next_cursor is the last returned row's tenant-bound position.
func renderPage(w http.ResponseWriter, r *http.Request, kind string, scope middleware.Scope, rows []ListRow, limit int) {
	var page contracts.Page
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		cursor := encodeCursor(cursorKey(), kind, scope, listCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		page.HasMore = true
		page.NextCursor = &cursor
	}
	page.Data = make([]any, len(rows))
	for i, row := range rows {
		page.Data[i] = row.Body
	}
	writeJSON(w, http.StatusOK, page)
}
