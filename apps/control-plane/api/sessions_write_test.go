package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// fakeSessionManager records what the handlers hand the store, so a test can tell "the route accepted
// my name" from "the route accepted my request and dropped my name" — which look identical from the
// status code alone, and which is the failure this file exists to catch.
type fakeSessionManager struct {
	createdName  string
	renamedID    string
	renamedName  string
	renameCalls  int
	renameMisses bool
}

func (f *fakeSessionManager) CreateSession(_ context.Context, _ middleware.Scope, name string) (SessionResult, error) {
	f.createdName = name
	return SessionResult{Body: []byte(`{"id":"ses_1","object":"session"}`), Found: true}, nil
}

func (f *fakeSessionManager) GetSession(_ context.Context, _ middleware.Scope, _ string) (SessionResult, error) {
	return SessionResult{Body: []byte(`{"id":"ses_1","object":"session"}`), Found: true}, nil
}

func (f *fakeSessionManager) ListSessions(_ context.Context, _ middleware.Scope, _ ListQuery) ([]ListRow, error) {
	return nil, nil
}

func (f *fakeSessionManager) RenameSession(_ context.Context, _ middleware.Scope, id, name string) (SessionResult, error) {
	f.renameCalls++
	f.renamedID, f.renamedName = id, name
	if f.renameMisses {
		return SessionResult{}, nil
	}
	return SessionResult{Body: []byte(`{"id":"` + id + `","object":"session","name":"` + name + `"}`), Found: true}, nil
}

func (f *fakeSessionManager) AcceptCommand(_ context.Context, _ middleware.Scope, _ string, _ contracts.CommandCreateRequest) (CommandResult, error) {
	return CommandResult{}, nil
}

func sessionWriteServer(t *testing.T, fake *fakeSessionManager) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, fake, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	return srv
}

// TestSessionCreateCarriesTheLabelAndStaysBodyless pins both halves of the create change. The label
// must REACH the store — a route that 201s while dropping the name is the exact shape this repository
// has shipped before — and the bodyless POST every existing caller sends must still work unchanged,
// which is the whole reason the body is optional rather than required.
func TestSessionCreateCarriesTheLabelAndStaysBodyless(t *testing.T) {
	fake := &fakeSessionManager{}
	srv := sessionWriteServer(t, fake)

	if resp := do(t, "POST", srv.URL+"/v1/sessions", `{"name":"Gece Doğrulama"}`, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with name status = %d, want 201", resp.StatusCode)
	}
	if fake.createdName != "Gece Doğrulama" {
		t.Fatalf("store received name %q, want %q — the route accepted the request and dropped the field",
			fake.createdName, "Gece Doğrulama")
	}

	fake.createdName = "sentinel"
	if resp := do(t, "POST", srv.URL+"/v1/sessions", ``, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("bodyless create status = %d, want 201 — every pre-E29 caller sends no body", resp.StatusCode)
	}
	if fake.createdName != "" {
		t.Fatalf("bodyless create passed name %q, want empty", fake.createdName)
	}
}

// TestSessionRenameReachesTheStoreAndRefusesAnAbsentName is the PATCH contract. The three cases that
// are not the happy path are the ones worth pinning: an absent name is a 400 and must NOT reach the
// store (a rename that renames nothing must not report success), an EMPTY name is a real value that
// does reach it (clearing the operator label is a legitimate thing to want), and an unknown or foreign
// id is a 404 carrying no hint that the session exists elsewhere.
func TestSessionRenameReachesTheStoreAndRefusesAnAbsentName(t *testing.T) {
	fake := &fakeSessionManager{}
	srv := sessionWriteServer(t, fake)

	resp := do(t, "PATCH", srv.URL+"/v1/sessions/ses_abc", `{"name":"Gece Doğrulama"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename status = %d, want 200", resp.StatusCode)
	}
	if fake.renamedID != "ses_abc" || fake.renamedName != "Gece Doğrulama" {
		t.Fatalf("store received (%q,%q), want (ses_abc, Gece Doğrulama)", fake.renamedID, fake.renamedName)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var got contracts.Session
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode rename body: %v", err)
	}
	if got.Name != "Gece Doğrulama" {
		t.Fatalf("rename returned name %q, want the label just set — the caller must see the row it wrote", got.Name)
	}

	before := fake.renameCalls
	if resp := do(t, "PATCH", srv.URL+"/v1/sessions/ses_abc", `{}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("rename with no name status = %d, want 400", resp.StatusCode)
	}
	if fake.renameCalls != before {
		t.Fatal("a rename with no name reached the store; it must be refused before any write")
	}

	if resp := do(t, "PATCH", srv.URL+"/v1/sessions/ses_abc", `{"name":""}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("rename to empty status = %d, want 200 — clearing the operator label is a real request", resp.StatusCode)
	}
	if fake.renamedName != "" {
		t.Fatalf("clearing passed name %q, want empty", fake.renamedName)
	}

	fake.renameMisses = true
	if resp := do(t, "PATCH", srv.URL+"/v1/sessions/ses_foreign", `{"name":"x"}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("rename of a foreign session status = %d, want 404", resp.StatusCode)
	}
}

// TestSessionWriteRefusesAnUnknownFieldAndAnOverlongName covers the two ways a body lies. An unknown
// field is REFUSED rather than ignored: this tree has twice shipped a write path that 2xx'd a field it
// silently discarded (config_policy.pool, approvers), and the caller believed the change landed. The
// length cap is enforced in runes, so a 200-character Turkish label is accepted rather than rejected
// for its byte count.
func TestSessionWriteRefusesAnUnknownFieldAndAnOverlongName(t *testing.T) {
	fake := &fakeSessionManager{}
	srv := sessionWriteServer(t, fake)

	if resp := do(t, "POST", srv.URL+"/v1/sessions", `{"nmae":"typo"}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with an unknown field status = %d, want 400", resp.StatusCode)
	}
	if resp := do(t, "PATCH", srv.URL+"/v1/sessions/ses_abc", `{"name":"x","extra":1}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("rename with an unknown field status = %d, want 400", resp.StatusCode)
	}

	// 200 runes of a multi-byte letter: 400 bytes, and it must be ACCEPTED. A byte-counting cap rejects
	// this, and the schema's maxLength counts characters.
	atCap := strings.Repeat("ğ", maxSessionNameRunes)
	if resp := do(t, "PATCH", srv.URL+"/v1/sessions/ses_abc", `{"name":"`+atCap+`"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("rename to a %d-rune (%d-byte) label status = %d, want 200 — the cap counts runes",
			len([]rune(atCap)), len(atCap), resp.StatusCode)
	}
	overCap := strings.Repeat("a", maxSessionNameRunes+1)
	if resp := do(t, "PATCH", srv.URL+"/v1/sessions/ses_abc", `{"name":"`+overCap+`"}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("rename to a %d-rune label status = %d, want 400", len(overCap), resp.StatusCode)
	}
}
