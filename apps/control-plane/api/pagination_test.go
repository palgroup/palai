package api

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// assertPageLen decodes a list response as a contracts.Page and asserts its data length. Shared by the
// per-resource list-route handler tests.
func assertPageLen(t *testing.T, resp *http.Response, want int) contracts.Page {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read page body: %v", err)
	}
	var p contracts.Page
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode page: %v (body=%s)", err, body)
	}
	if len(p.Data) != want {
		t.Fatalf("page data len = %d, want %d (body=%s)", len(p.Data), want, body)
	}
	return p
}

// TestCursorRoundTrips is the happy path: a cursor minted for a scope decodes back to the
// same keyset position under that same scope and resource kind.
func TestCursorRoundTrips(t *testing.T) {
	key := testCursorKey()
	scope := middleware.Scope{Organization: "org_a", Project: "prj_a"}
	pos := listCursor{CreatedAt: time.Unix(0, 1_700_000_000_123_456_789).UTC(), ID: "resp_abc123"}

	tok := encodeCursor(key, "responses", scope, pos)
	got, err := decodeCursor(key, "responses", scope, tok)
	if err != nil {
		t.Fatalf("decodeCursor round-trip error = %v", err)
	}
	if !got.CreatedAt.Equal(pos.CreatedAt) || got.ID != pos.ID {
		t.Fatalf("round-trip = %+v, want %+v", got, pos)
	}
}

// TestCursorRejectsForeignTenant is the TEN-001 cursor-fuzz core: a cursor minted for tenant A
// is REJECTED — an explicit error, not a silently-different page — when presented by tenant B.
func TestCursorRejectsForeignTenant(t *testing.T) {
	key := testCursorKey()
	orgA := middleware.Scope{Organization: "org_a", Project: "prj_a"}
	orgB := middleware.Scope{Organization: "org_b", Project: "prj_a"}
	tok := encodeCursor(key, "responses", orgA, listCursor{CreatedAt: time.Now().UTC(), ID: "resp_x"})

	if _, err := decodeCursor(key, "responses", orgB, tok); err == nil {
		t.Fatal("a cursor minted for org_a decoded under org_b; the foreign cursor was not rejected")
	}
	// A different project within the same org is also a foreign cursor.
	orgAProjC := middleware.Scope{Organization: "org_a", Project: "prj_c"}
	if _, err := decodeCursor(key, "responses", orgAProjC, tok); err == nil {
		t.Fatal("a cursor minted for prj_a decoded under prj_c; the foreign cursor was not rejected")
	}
}

// TestCursorRejectsForeignKind proves a cursor cannot be replayed onto another list resource:
// a /v1/responses cursor is rejected when presented to the /v1/sessions list.
func TestCursorRejectsForeignKind(t *testing.T) {
	key := testCursorKey()
	scope := middleware.Scope{Organization: "org_a", Project: "prj_a"}
	tok := encodeCursor(key, "responses", scope, listCursor{CreatedAt: time.Now().UTC(), ID: "resp_x"})
	if _, err := decodeCursor(key, "sessions", scope, tok); err == nil {
		t.Fatal("a responses cursor decoded on the sessions list; the cross-kind cursor was not rejected")
	}
}

// TestCursorRejectsTamperAndGarbage covers the fuzz surface: a flipped byte, a truncated token,
// and non-base64 garbage all fail closed with errBadCursor rather than a 500 or a wrong page.
func TestCursorRejectsTamperAndGarbage(t *testing.T) {
	key := testCursorKey()
	scope := middleware.Scope{Organization: "org_a", Project: "prj_a"}
	tok := encodeCursor(key, "responses", scope, listCursor{CreatedAt: time.Now().UTC(), ID: "resp_x"})

	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		t.Fatalf("decode token bytes: %v", err)
	}
	raw[0] ^= 0xFF // flip a MAC byte
	tampered := base64.RawURLEncoding.EncodeToString(raw)

	for name, bad := range map[string]string{
		"tampered":  tampered,
		"truncated": tok[:4],
		"garbage":   "!!!not-base64!!!",
		"empty":     "",
	} {
		if _, err := decodeCursor(key, "responses", scope, bad); err == nil {
			t.Fatalf("%s cursor decoded without error; it must fail closed", name)
		}
	}
}

// TestCursorDoesNotLeakTenant proves the opaque token discloses no tenant identity: the decoded
// payload contains neither the organization nor the project id (they live only in the HMAC).
func TestCursorDoesNotLeakTenant(t *testing.T) {
	key := testCursorKey()
	scope := middleware.Scope{Organization: "org_secret_tenant", Project: "prj_secret_tenant"}
	tok := encodeCursor(key, "responses", scope, listCursor{CreatedAt: time.Now().UTC(), ID: "resp_x"})

	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		t.Fatalf("decode token bytes: %v", err)
	}
	if strings.Contains(string(raw), "org_secret_tenant") || strings.Contains(string(raw), "prj_secret_tenant") {
		t.Fatalf("cursor token leaks the tenant identity: %q", raw)
	}
}

// testCursorKey is a fixed key so the unit test is deterministic; production uses a
// process-random key (cursorKey).
func testCursorKey() []byte {
	return []byte("test-cursor-key-0123456789abcdef")
}
