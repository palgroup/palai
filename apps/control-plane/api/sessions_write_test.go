package api

import (
	"context"
	"encoding/json"
	"fmt"
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

	// The standing-authorization half (E30 T1). armTools/armPublications are what the ROUTE decided to
	// send, which is the only thing that distinguishes "the route armed what I asked for" from "the
	// route armed something else and answered 200" — the two are identical from the status code.
	armCalls        int
	armTools        bool
	armPublications bool
	// currentTools/currentPublications are what GetSession reports the session ALREADY has, so a test
	// can prove a PATCH naming one half leaves the other exactly as it was.
	currentTools        bool
	currentPublications bool
}

func (f *fakeSessionManager) CreateSession(_ context.Context, _ middleware.Scope, name string) (SessionResult, error) {
	f.createdName = name
	return SessionResult{Body: []byte(`{"id":"ses_1","object":"session"}`), Found: true}, nil
}

func (f *fakeSessionManager) GetSession(_ context.Context, _ middleware.Scope, _ string) (SessionResult, error) {
	body := fmt.Sprintf(`{"id":"ses_1","object":"session","auto_approve_tools":%t,"auto_approve_publications":%t}`,
		f.currentTools, f.currentPublications)
	return SessionResult{Body: []byte(body), Found: true}, nil
}

func (f *fakeSessionManager) SetSessionAutoApprove(_ context.Context, _ middleware.Scope, id string, tools, publications bool) (SessionResult, error) {
	f.armCalls++
	f.armTools, f.armPublications = tools, publications
	body := fmt.Sprintf(`{"id":%q,"object":"session","auto_approve_tools":%t,"auto_approve_publications":%t}`, id, tools, publications)
	return SessionResult{Body: []byte(body), Found: true}, nil
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

// ---------------------------------------------------------------------------------------------------
// THE STANDING AUTHORIZATION OVER HTTP (E30 T1).
// ---------------------------------------------------------------------------------------------------
//
// These drive the ROUTE, not the store, because the failure they exist to catch is invisible from a
// status code: a PATCH that answers 200 and arms something OTHER than what was asked reads exactly like
// one that worked. The fake records what the route decided to send.

// TestSessionPatchArmsTheToolHalfWithoutTouchingPublications is the split, at the HTTP edge. Arming the
// tool half must send publications=false because that is what the session already had — NOT because the
// route helpfully sent both.
func TestSessionPatchArmsTheToolHalfWithoutTouchingPublications(t *testing.T) {
	fake := &fakeSessionManager{}
	srv := sessionWriteServer(t, fake)

	resp := patchSession(t, srv, "ses_1", `{"auto_approve_tools":true}`)
	if resp != http.StatusOK {
		t.Fatalf("PATCH auto_approve_tools = %d, want 200", resp)
	}
	if fake.armCalls != 1 {
		t.Fatalf("SetSessionAutoApprove called %d time(s), want 1", fake.armCalls)
	}
	if !fake.armTools {
		t.Fatal("the route did not arm the tool half the request asked for")
	}
	if fake.armPublications {
		t.Fatal("ARMING THE TOOL HALF ARMED PUBLICATIONS AT THE HTTP EDGE: the operator asked to stop " +
			"confirming xcodebuild and has been given unattended writes to their repository")
	}
	if fake.renameCalls != 0 {
		t.Fatalf("an auto-approve PATCH also called RenameSession %d time(s) — it must not touch the label", fake.renameCalls)
	}
}

// TestSessionPatchNamingOneHalfLeavesTheOtherExactlyAsItWas is the hazard the pointer fields exist for.
// The session is ALREADY armed for publications; a PATCH that arms tools must not disarm it.
func TestSessionPatchNamingOneHalfLeavesTheOtherExactlyAsItWas(t *testing.T) {
	fake := &fakeSessionManager{currentPublications: true}
	srv := sessionWriteServer(t, fake)

	if got := patchSession(t, srv, "ses_1", `{"auto_approve_tools":true}`); got != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200", got)
	}
	if !fake.armTools {
		t.Fatal("the tool half was not armed")
	}
	if !fake.armPublications {
		t.Fatal("A PATCH THAT NAMED ONLY THE TOOL HALF DISARMED PUBLICATIONS: one half became a lever on " +
			"the other, which is exactly what two columns exist to prevent")
	}
}

// TestSessionPatchOfTheNameAloneDoesNotDisarmTheSession is the SAME hazard from the other side, and it
// is the one a plain `bool` in the generated contract would have shipped: `{"name":"x"}` decodes both
// flags as false, so a rename would silently disarm a session armed a minute earlier.
func TestSessionPatchOfTheNameAloneDoesNotDisarmTheSession(t *testing.T) {
	fake := &fakeSessionManager{currentTools: true, currentPublications: true}
	srv := sessionWriteServer(t, fake)

	if got := patchSession(t, srv, "ses_1", `{"name":"iOS build session"}`); got != http.StatusOK {
		t.Fatalf("PATCH name = %d, want 200", got)
	}
	if fake.armCalls != 0 {
		t.Fatalf("a rename called SetSessionAutoApprove %d time(s) — a PATCH that carries no auto-approve "+
			"field must not touch the standing authorization at all", fake.armCalls)
	}
	if fake.renamedName != "iOS build session" {
		t.Fatalf("renamed to %q, want the label the request carried", fake.renamedName)
	}
}

// TestSessionPatchDisarmsExplicitly proves false is a REAL value rather than an absence — the toggle has
// to work in both directions or the screen is lying in one of them.
func TestSessionPatchDisarmsExplicitly(t *testing.T) {
	fake := &fakeSessionManager{currentTools: true}
	srv := sessionWriteServer(t, fake)

	if got := patchSession(t, srv, "ses_1", `{"auto_approve_tools":false}`); got != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200", got)
	}
	if fake.armCalls != 1 || fake.armTools {
		t.Fatalf("disarm sent tools=%v after %d call(s), want false after 1", fake.armTools, fake.armCalls)
	}
}

// TestSessionPatchWithNeitherFieldIsStillABadRequest keeps the pre-existing contract exactly: a PATCH
// that changes nothing is a request that lost its field, and silently succeeding at nothing is how a
// caller comes to believe a change landed that did not.
func TestSessionPatchWithNeitherFieldIsStillABadRequest(t *testing.T) {
	fake := &fakeSessionManager{}
	srv := sessionWriteServer(t, fake)

	if got := patchSession(t, srv, "ses_1", `{}`); got != http.StatusBadRequest {
		t.Fatalf("PATCH {} = %d, want 400", got)
	}
	if fake.armCalls != 0 || fake.renameCalls != 0 {
		t.Fatalf("an empty PATCH still wrote something (arm=%d rename=%d)", fake.armCalls, fake.renameCalls)
	}
}

// TestSessionPatchRefusesToNameItsOwnApprover is the no-escalation claim at the edge. The principal is
// stamped server-side from the verified key; a body that tries to supply one must be a 400 rather than a
// silently ignored field, because the auto-decision is MADE as that principal.
func TestSessionPatchRefusesToNameItsOwnApprover(t *testing.T) {
	fake := &fakeSessionManager{}
	srv := sessionWriteServer(t, fake)

	if got := patchSession(t, srv, "ses_1", `{"auto_approve_tools":true,"auto_approve_set_by":"key:someone-else"}`); got != http.StatusBadRequest {
		t.Fatalf("PATCH naming its own approver = %d, want 400", got)
	}
	if fake.armCalls != 0 {
		t.Fatal("a request that tried to name its own approver still armed the session")
	}
}

func patchSession(t *testing.T, srv *httptest.Server, id, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/v1/sessions/"+id, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build PATCH: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("PATCH /v1/sessions/%s: %v", id, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
