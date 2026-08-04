//go:build component

package extensions

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// The REDEMPTION half of the Slack last mile, against a real Postgres.
//
// `palai up` registered a connection whose signing_secret_ref / bot_token_ref / app_token_ref were
// correct handles and stored no VALUE anywhere the control-plane could redeem — so every one of
// them resolved to nothing. That failure is invisible at every layer: an unresolvable signing
// secret is a refusal with no config oracle, an unresolvable bot token is a message that quietly
// stays stale, an unresolvable app token is a socket that is simply never dialled. A registration
// looked complete and could receive nothing.
//
// So the assertion here is not "a secret store works". It is that each of the three handles, as
// written on a REAL slack_connections row, reaches its REAL consumer with the value that was
// stored — through the store's own write-path (CreateSecretRef, envelope-encrypted at rest) and the
// resolver chain main.dbSecret puts in front of every Slack ref:
//
//	signing_secret_ref → SlackAdmitter.VerifySignature → a v0 MAC computed longhand from the value
//	bot_token_ref      → SlackAdmitter.repairDecisionMessage → the chat.update Authorization header
//	app_token_ref      → SlackSocket.dial → the apps.connections.open Authorization header
//
// The two Authorization headers are read off the wire by a fake Slack, not off our own writer.

// slackRedemptionSecrets are the three values under test. They are obvious fakes and are asserted
// to appear ONLY where they belong.
const (
	redeemSigning  = "8f14e45fceea167a5a36dedd4bea2543"
	redeemBotToken = "xoxb-0000-1111-notarealbottokenatall"
	redeemAppToken = "xapp-1-A0000-0000-notarealapptokenatall"
)

// dbSecretResolver is main.dbSecret's body: the DB-backed store in front, a miss (or a store error)
// falling through — here to nothing, since a component test has no env bridge. Written out rather
// than imported because the composition root is package main; what it proves is that the STORE
// answers a (org, ref) pair with the value stored under it, which is the half the last mile added.
func dbSecretResolver(t *testing.T, secrets *identity.SecretStore) SecretResolver {
	t.Helper()
	return func(ref string) ([]byte, error) {
		v, ok, err := secrets.Resolve(context.Background(), org, ref)
		switch {
		case err != nil:
			return nil, err
		case !ok:
			return nil, fmt.Errorf("no secret is stored for ref %q under org %q", ref, org)
		}
		return v, nil
	}
}

// seedSlackWorkspace stores the three values and registers a connection naming them, exactly as
// `palai up` does: the handle written on the row and the name the value is stored under are the
// same string, which is the pairing the whole task exists to establish.
func seedSlackWorkspace(t *testing.T, s *Store, org, project string) (*identity.SecretStore, api.SlackConnectionRef) {
	t.Helper()
	ctx := context.Background()

	// A real master key, the shape identity.ParseMasterKey (and `palai up`'s ensureMasterKey) mints.
	key, err := identity.ParseMasterKey("a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90")
	if err != nil {
		t.Fatalf("parse master key: %v", err)
	}
	secrets := identity.NewSecretStore(s.pool, key)

	team := testID("T")
	scope := middleware.Scope{Project: project}
	refs := map[string]string{
		"slack-signing-" + team: redeemSigning,
		"slack-bot-" + team:     redeemBotToken,
		"slack-app-" + team:     redeemAppToken,
	}
	for name, value := range refs {
		body, _ := json.Marshal(map[string]string{"name": name, "value": value})
		out, err := secrets.CreateSecretRef(ctx, scope, body)
		if err != nil {
			t.Fatalf("store secret ref %q: %v", name, err)
		}
		if out.MissingField != "" || out.BadField {
			t.Fatalf("store secret ref %q was refused: missing=%q bad=%t", name, out.MissingField, out.BadField)
		}
	}

	reg := fmt.Sprintf(`{"team_id":%q,"bot_user_id":"Ubot",
		"signing_secret_ref":"slack-signing-%s","bot_token_ref":"slack-bot-%s","app_token_ref":"slack-app-%s",
		"default_policy":{"agent_revision_id":"agr_1","principal_id":"prn_1"}}`, team, team, team, team)
	if _, err := s.CreateSlackConnection(ctx, org, project, []byte(reg)); err != nil {
		t.Fatalf("register the workspace: %v", err)
	}

	// The ref set the transports actually see comes from ResolveConnection — the production lookup an
	// unauthenticated callback and the connect loop both go through — never from the literal above.
	a := NewSlackAdmitter(s, nil, nil, api.AdmissionLimits{})
	conn, found, err := a.ResolveConnection(ctx, team, "")
	if err != nil || !found {
		t.Fatalf("resolve the registered workspace: found=%t err=%v", found, err)
	}
	return secrets, conn
}

// TestSlackHandlesRedeemToTheStoredValues is the guarantee. Each sub-test drives ONE handle's real
// consumer and reads the credential back off the wire (or off a MAC), so a handle that resolved to
// nothing — the state on main — fails it with the consumer's own symptom.
func TestSlackHandlesRedeemToTheStoredValues(t *testing.T) {
	s, org, project := openStore(t)
	secrets, conn := seedSlackWorkspace(t, s, org, project)
	resolve := dbSecretResolver(t, secrets)

	for _, want := range []struct{ what, ref string }{
		{"signing_secret_ref", conn.SigningSecretRef},
		{"bot_token_ref", conn.BotTokenRef},
		{"app_token_ref", conn.AppTokenRef},
	} {
		if want.ref == "" {
			t.Fatalf("the registered connection carries no %s", want.what)
		}
	}

	t.Run("signing_secret_ref verifies a v0 signature computed from the stored value", func(t *testing.T) {
		a := NewSlackAdmitter(s, nil, resolve, api.AdmissionLimits{})
		body := []byte(`{"type":"event_callback","event_id":"Ev1"}`)
		ts := strconv.FormatInt(time.Now().Unix(), 10)

		// LONGHAND: the MAC is built here from the value that was STORED, never by asking the adapter
		// to sign for us — so a resolver that returned some other value cannot produce this signature.
		mac := hmac.New(sha256.New, []byte(redeemSigning))
		mac.Write([]byte("v0:" + ts + ":" + string(body)))
		sig := "v0=" + hex.EncodeToString(mac.Sum(nil))

		if err := a.VerifySignature(context.Background(), conn, ts, sig, body); err != nil {
			t.Fatalf("a signature computed from the STORED signing secret did not verify: %v — the handle %q resolved to something else, or to nothing", err, conn.SigningSecretRef)
		}
		// And the converse, so the pass above is not vacuous: a MAC under a different secret fails.
		other := hmac.New(sha256.New, []byte("a-different-signing-secret"))
		other.Write([]byte("v0:" + ts + ":" + string(body)))
		if err := a.VerifySignature(context.Background(), conn, ts, "v0="+hex.EncodeToString(other.Sum(nil)), body); !errors.Is(err, slack.ErrBadSignature) {
			t.Fatalf("a foreign signature verified (err = %v); the check would accept anything", err)
		}
	})

	t.Run("bot_token_ref reaches chat.update as the bearer", func(t *testing.T) {
		fake := newFakeSlackWebAPI(t)
		a := NewSlackAdmitter(s, nil, resolve, api.AdmissionLimits{})
		a.doer, a.apiBase = fake.srv.Client(), fake.srv.URL

		intent := slack.ApprovalIntent{
			TeamID: conn.TeamID, UserID: "U1", Decision: "approve", ActionID: "approve",
			ChannelID: "C1", ThreadTS: "1.1", MessageTS: "1.2",
		}
		scoped := storage.ScopeToTenant(context.Background(), conn.Project)
		if !a.repairDecisionMessage(scoped, conn, intent, "", coordinator.Publication{Display: "a decision"}) {
			t.Fatalf("the visible message was not repaired: the bot token behind %q resolved to nothing, so the decision stands but Slack is never told", conn.BotTokenRef)
		}
		if got := fake.authorization("/chat.update"); got != "Bearer "+redeemBotToken {
			t.Fatalf("chat.update presented %q, want the STORED bot token", got)
		}
	})

	t.Run("app_token_ref reaches apps.connections.open as the bearer", func(t *testing.T) {
		peer := newFakeSocketPeer(t)
		a := NewSlackAdmitter(s, nil, resolve, api.AdmissionLimits{})
		a.doer, a.apiBase = peer.srv.Client(), peer.srv.URL

		sock := a.SocketMode(conn.TeamID)
		if sock == nil {
			t.Fatal("SocketMode returned nil for a registered workspace")
		}
		runSocket(t, sock)
		peer.accept(t, "the Socket Mode connection for a registered workspace")

		peer.mu.Lock()
		auth := append([]string(nil), peer.auth...)
		peer.mu.Unlock()
		if len(auth) == 0 {
			t.Fatal("apps.connections.open was never called: the connect loop had no app-level token to present")
		}
		if auth[0] != "Bearer "+redeemAppToken {
			t.Fatalf("apps.connections.open presented %q, want the STORED app-level token", auth[0])
		}
	})
}

// TestSlackSocketRefusesToDialOnAnUnredeemableHandle is the state main shipped: the row names a
// handle, nothing is stored under it, and the loop must NOT open a socket. It is the RED-side
// companion — without it "the token reached the wire" could be satisfied by a loop that dials with
// an empty bearer.
func TestSlackSocketRefusesToDialOnAnUnredeemableHandle(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	team := testID("T")
	reg := fmt.Sprintf(`{"team_id":%q,"bot_user_id":"Ubot","signing_secret_ref":"slack-signing-%s","app_token_ref":"slack-app-%s",
		"default_policy":{"agent_revision_id":"agr_1","principal_id":"prn_1"}}`, team, team, team)
	if _, err := s.CreateSlackConnection(ctx, org, project, []byte(reg)); err != nil {
		t.Fatalf("register the workspace: %v", err)
	}

	key, _ := identity.ParseMasterKey("a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90")
	// A store with NOTHING in it — the registered-but-unstored state.
	resolve := dbSecretResolver(t, identity.NewSecretStore(s.pool, key))

	peer := newFakeSocketPeer(t)
	a := NewSlackAdmitter(s, nil, resolve, api.AdmissionLimits{})
	a.doer, a.apiBase = peer.srv.Client(), peer.srv.URL

	sock := a.SocketMode(team)
	if sock == nil {
		t.Fatal("SocketMode returned nil for a registered workspace")
	}
	_, result := runSocket(t, sock)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("the connect loop finished CLEANLY on an unredeemable app token: a permanent stop hides the fault from the supervisor")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connect loop neither dialled nor returned within 5s")
	}
	if peer.openCount() != 0 {
		t.Fatalf("apps.connections.open was called %d time(s) with no token to present", peer.openCount())
	}
}

// ---------------------------------------------------------------------------------------------------
// a fake Slack Web API (the chat.update half)
// ---------------------------------------------------------------------------------------------------

// fakeSlackWebAPI answers the Web API methods the outbound half calls, recording the Authorization
// header each one presented. It is deliberately separate from fakeSocketPeer: the bearer under test
// here is the BOT token, and mixing the two on one server would make a passing assertion ambiguous
// about which credential arrived.
type fakeSlackWebAPI struct {
	srv *httptest.Server

	mu   sync.Mutex
	auth map[string]string
}

func newFakeSlackWebAPI(t *testing.T) *fakeSlackWebAPI {
	t.Helper()
	f := &fakeSlackWebAPI{auth: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth[r.URL.Path] = r.Header.Get("Authorization")
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlackWebAPI) authorization(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auth[path]
}
