package execution

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/packages/coordinator"
)

// WHICH IDENTITY THE WRITE IS MADE UNDER, proven on the wire rather than by which field was read.
//
// The publisher already refuses to fall back to the deployment App when a binding's credential does not
// resolve (approval_test.go). What that cannot show is the successful case: that a publish which SUCCEEDS
// succeeded as the tenant, because with a local file remote every credential works equally well and the
// assertion would be vacuous. So these push to a real git-http-backend behind Basic auth that answers 401
// to anything but one exact token — a landed ref is then proof of WHOSE credential authenticated it.
//
// UNTAGGED, deliberately. It needs git and a loopback HTTP server and nothing else — no Docker, no
// database — so it rides `go test ./...` and `make verify` rather than a component tier whose `-run`
// allow-list is a second place to remember it.

// pushTargetRemote is a real, EMPTY bare repository served over HTTP by git-http-backend behind Basic
// auth. It is the push-side twin of the component tier's authedGitRemote (which serves a seeded repo to
// clone FROM): this one exists to be written TO, and receive-pack is enabled so a push can land.
type pushTargetRemote struct {
	url string
	dir string
	// seen records every Authorization header the remote was offered, including the ones it refused, so a
	// test can say what was PRESENTED and not only what was accepted.
	seen *[]string
}

func newPushTargetRemote(t *testing.T, token string) pushTargetRemote {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found on PATH: %v", err)
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "target.git")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := gitAt(t, repoDir)
	run("init", "-q", "--bare", "-b", "main")
	// git-http-backend serves receive-pack only when the repository opts in; without this it answers 403
	// "Service not enabled: 'receive-pack'" to an AUTHENTICATED push, which would make a failed push look
	// like a refused credential and every assertion below true for the wrong reason.
	run("config", "http.receivepack", "true")

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	seen := &[]string{}
	backend := &cgi.Handler{Path: gitBin, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			*seen = append(*seen, got)
		}
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="palai"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return pushTargetRemote{url: srv.URL + "/target.git", dir: repoDir, seen: seen}
}

// TestAnAppLessPublishAuthenticatesAsTheBindingsOwnCredential is the owner's transcript at the PUMP, and
// on a remote that can tell the two identities apart.
//
// No GitHub App anywhere in this publisher (Broker is nil, which is a supported deployment after the
// construction was un-gated). The binding names its own credential. An approved push. The remote refuses
// every credential but the tenant's, and the branch LANDS — so the write was made as the tenant, on a
// deployment that has no deployment-global credential at all.
func TestAnAppLessPublishAuthenticatesAsTheBindingsOwnCredential(t *testing.T) {
	requireGitExec(t)
	ctx := context.Background()

	const tenantToken = "tenant-provisioned-token-a1b2"
	remote := newPushTargetRemote(t, tenantToken)
	root := t.TempDir()
	head := seedWorkspaceRepo(t, root)

	pub := coordinator.Publication{
		ID: "pub_appless", RunID: "run_1", Operation: "push_branch",
		Remote: remote.url, Branch: "agent/ses/appless", HeadSHA: head, State: "approved",
	}
	pump := newFakePump(pub)
	publisher := &RepositoryPublisher{
		// NO DEPLOYMENT-GLOBAL BROKER. This is the state that used to make the whole publisher nil.
		Broker: nil,
		ConnectionSecrets: func(_ coordinator.Tenant, ref string) ([]byte, error) {
			if ref != "rcon_tenant_pat" {
				t.Errorf("resolver asked for ref=%q, want the binding's own", ref)
			}
			return []byte(tenantToken), nil
		},
	}

	tenant := coordinator.Tenant{Project: "prj"}
	if err := publishApproved(ctx, pump, publisher, tenant, "run_1", "ses_1", "resp_1", root, 7,
		publicationCredential{ConnectionRef: "rcon_tenant_pat", Identity: "local/demo-target"}); err != nil {
		t.Fatalf("publishApproved() error = %v", err)
	}
	if _, published := pump.published["pub_appless"]; !published {
		t.Fatalf("nothing was published on an App-less deployment: warned=%v — this is the approval that "+
			"landed and did nothing", pump.warned)
	}
	if got := remoteBranch(t, remote.dir, "agent/ses/appless"); got != head {
		t.Fatalf("the remote's ref is %q, want the approved head %q", got, head)
	}
}

// TestAPublishUnderABindingCredentialNeverAuthenticatesAsTheApp is the SECURITY leg, and it has two
// halves because either alone is vacuous.
//
// The publisher holds BOTH: a deployment-global broker and the binding's resolver. The remote accepts only
// the tenant's token.
//
//	ConnectionRef set   -> the push LANDS, so the deployment credential is not what authenticated it.
//	ConnectionRef empty -> the push is REFUSED 401, so the deployment credential really is the one a
//	                       ref-less binding uses, and it really cannot write here.
//
// Without the second half the first is satisfied by a remote that accepts anything.
//
// THE STAND-IN, stated rather than glossed: the deployment-global broker here is a token broker holding a
// DIFFERENT token, not a GitHub App broker — no App credential exists in any tier of this repository
// (repository_binding_component_test.go records the same ceiling). What is being proven is WHICH
// credential reaches the wire, and any broker whose credential differs from the tenant's demonstrates it.
func TestAPublishUnderABindingCredentialNeverAuthenticatesAsTheApp(t *testing.T) {
	requireGitExec(t)
	ctx := context.Background()

	const tenantToken = "tenant-provisioned-token-c3d4"
	const deploymentToken = "deployment-app-token-e5f6"
	remote := newPushTargetRemote(t, tenantToken)
	root := t.TempDir()
	head := seedWorkspaceRepo(t, root)
	tenant := coordinator.Tenant{Project: "prj"}

	publisher := &RepositoryPublisher{
		Broker:            repositories.NewTokenBroker(deploymentToken),
		ConnectionSecrets: func(coordinator.Tenant, string) ([]byte, error) { return []byte(tenantToken), nil },
	}

	// HALF ONE: the binding names its own credential, and the write lands.
	own := coordinator.Publication{
		ID: "pub_own", RunID: "run_1", Operation: "push_branch",
		Remote: remote.url, Branch: "agent/ses/own", HeadSHA: head, State: "approved",
	}
	ownPump := newFakePump(own)
	if err := publishApproved(ctx, ownPump, publisher, tenant, "run_1", "ses_1", "resp_1", root, 7,
		publicationCredential{ConnectionRef: "rcon_tenant_pat", Identity: "local/demo-target"}); err != nil {
		t.Fatalf("publishApproved(own credential) error = %v", err)
	}
	if _, published := ownPump.published["pub_own"]; !published {
		t.Fatalf("a binding with its own credential published nothing: warned=%v", ownPump.warned)
	}
	if got := remoteBranch(t, remote.dir, "agent/ses/own"); got != head {
		t.Fatalf("the remote's ref is %q, want the approved head %q", got, head)
	}
	// The deployment credential was never even OFFERED. The refused attempts are recorded too, so this
	// distinguishes "was rejected" from "was never presented" — the latter is the property.
	//
	// THE POSITIVE CONTROL COMES FIRST because a sweep over an empty list can never fail: if git had
	// authenticated some other way, or the recorder had missed every request, the loop below would report
	// a clean result while measuring nothing.
	presented := append([]string(nil), *remote.seen...)
	sawTenant := false
	for _, header := range presented {
		decoded := decodeBasic(t, header)
		if strings.Contains(decoded, tenantToken) {
			sawTenant = true
		}
		if strings.Contains(decoded, deploymentToken) {
			t.Fatal("the deployment-global credential was presented to the remote while the binding named " +
				"its own: a publish under the wrong identity SUCCEEDS, it does not fail")
		}
	}
	if !sawTenant {
		t.Fatalf("the tenant's credential was never presented to the remote (%d headers seen), so the sweep "+
			"for the deployment credential measured nothing", len(presented))
	}

	// HALF TWO, on the same publisher and the same remote: a binding naming NO credential falls to the
	// deployment one, which this remote refuses. Without this the assertion above proves nothing about
	// which credential a ref-less binding would have used.
	refLess := coordinator.Publication{
		ID: "pub_refless", RunID: "run_1", Operation: "push_branch",
		Remote: remote.url, Branch: "agent/ses/refless", HeadSHA: head, State: "approved",
	}
	refLessPump := newFakePump(refLess)
	if err := publishApproved(ctx, refLessPump, publisher, tenant, "run_1", "ses_1", "resp_1", root, 7,
		publicationCredential{}); err != nil {
		t.Fatalf("publishApproved(no credential) error = %v, want a warning not a fatal error", err)
	}
	if _, published := refLessPump.published["pub_refless"]; published {
		t.Fatal("a ref-less push was published to a remote that refuses the deployment credential: the two " +
			"paths are not distinct, so half one proved nothing")
	}
	if _, err := repoRef(remote.dir, "agent/ses/refless"); err == nil {
		t.Fatal("the ref-less branch reached the remote anyway")
	}
}

// TestARefLessPublicationIsWarnedRatherThanSkipped is what the operator gets at the
// pump when a publication has no credential path at all.
//
// This is the case that used to be INVISIBLE: no App meant no publisher, no publisher meant the pump
// returned before reading anything, and the row sat at `approved` forever with a human believing they
// authorized a push. Now the publisher exists, refuses, and the refusal lands where every other publish
// failure lands — a warning on the publication row (REP-010) that the run and the screen can read.
func TestARefLessPublicationIsWarnedRatherThanSkipped(t *testing.T) {
	requireGitExec(t)
	ctx := context.Background()
	root := t.TempDir()
	head := seedWorkspaceRepo(t, root)
	bare := seedBareRemote(t)

	pub := coordinator.Publication{
		ID: "pub_nocred", RunID: "run_1", Operation: "push_branch",
		Remote: bare, Branch: "agent/ses/nocred", HeadSHA: head, State: "approved",
	}
	pump := newFakePump(pub)
	// An App-less deployment: the connection resolver is wired (it always is), the App broker is not.
	publisher := &RepositoryPublisher{
		ConnectionSecrets: func(coordinator.Tenant, string) ([]byte, error) {
			t.Error("the connection resolver was consulted for a binding that names no connection_ref")
			return nil, nil
		},
	}

	if err := publishApproved(ctx, pump, publisher, coordinator.Tenant{Project: "prj"},
		"run_1", "ses_1", "resp_1", root, 1, publicationCredential{}); err != nil {
		t.Fatalf("publishApproved() error = %v, want a warning not a fatal error", err)
	}
	if _, published := pump.published["pub_nocred"]; published {
		t.Fatal("a publication with no credential path was marked published")
	}
	if _, err := repoRef(bare, "agent/ses/nocred"); err == nil {
		t.Fatal("a branch reached the remote with no credential to publish under")
	}
	warning, warned := pump.warned["pub_nocred"]
	if !warned {
		t.Fatal("nothing was published and NOTHING WAS WARNED: this is the silent skip the whole change " +
			"exists to remove — an approved row, a human who pressed Approve, and no surface saying why")
	}
	// It must name the REMEDY, and the remedy is now one thing rather than two. This assertion used to
	// require "PALAI_GITHUB_APP_ID" as well, because a ref-less binding had a second way to publish — the
	// deployment-global GitHub App, removed 2026-08-05. Naming a variable nothing reads would send an
	// operator to configure something that cannot help them.
	for _, want := range []string{"connection_ref", "/v1/secret-refs"} {
		if !strings.Contains(warning, want) {
			t.Fatalf("the warning does not name %q, so it does not say what to do: %q", want, warning)
		}
	}
}

// decodeBasic returns the decoded `user:secret` of a Basic Authorization header, or the header itself when
// it is not decodable — the caller is scanning for a secret, so an undecodable header must still be
// scanned rather than silently dropped.
func decodeBasic(t *testing.T, header string) string {
	t.Helper()
	raw, ok := strings.CutPrefix(header, "Basic ")
	if !ok {
		return header
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return header
	}
	return string(decoded)
}
