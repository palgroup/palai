//go:build component

// MCI-007 (E13 Task 9): the repository_bindings.connection_ref resolver seam, proven against a REAL
// Postgres (the durable binding + the envelope-encrypted secret-ref store, migration 000031), REAL git,
// and a REAL authenticated HTTP git remote. The remote answers 401 to anything but the exact token the
// tenant provisioned, so a successful clone is proof that the BINDING-SCOPED credential — not the
// deployment-global broker — authenticated it: the fallback broker handed to PrepareRepository here mints
// a token that remote rejects.
//
// HONEST CEILING: no real GitHub App credential is available to this repository's test tiers (.env.local
// carries a model-provider key only). The resolver seam, the ref-less global fallback, and cross-tenant
// isolation are proven against real git + a real DB + real HTTP basic-auth; the hosted-GitHub leg — an
// installation token minted by a real App against github.com — is NOT proven here. Nothing in this file
// stands in for that; the GitHub App broker keeps its own (unchanged) coverage.

package execution

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// bindingSecretHarness is a real spine plus the real secret-ref store, keyed by a fresh master key.
type bindingSecretHarness struct {
	spine   *coordinator.Store
	secrets *identity.SecretStore
}

func newBindingSecretHarness(t *testing.T) *bindingSecretHarness {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found on PATH: %v", err)
	}
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("mint master key: %v", err)
	}
	master, err := identity.ParseMasterKey(hex.EncodeToString(key[:]))
	if err != nil {
		t.Fatalf("ParseMasterKey: %v", err)
	}
	return &bindingSecretHarness{spine: cs, secrets: identity.NewSecretStore(cs.Pool(), master)}
}

// resolver is the production-shaped bridge from (org, ref) to the credential bytes: the DB-backed store
// first, a MISS is an error (a binding that names its own credential never falls back to the global one)
// — exactly what main.go's repositoryConnectionSecret does.
func (h *bindingSecretHarness) resolver(calls *int) SecretResolver {
	return func(org, ref string) ([]byte, error) {
		*calls++
		v, ok, err := h.secrets.Resolve(context.Background(), org, ref)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("no secret ref provisioned under org %q for repository connection %q", org, ref)
		}
		return v, nil
	}
}

// seedTenant creates an organization/project/session/run the preparation receipt can key to, and returns
// the tenant plus the run id.
func (h *bindingSecretHarness) seedTenant(t *testing.T) (coordinator.Tenant, string) {
	t.Helper()
	tenant := coordinator.Tenant{Organization: redeliveryID("org"), Project: redeliveryID("prj")}
	sessionID, runID := redeliveryID("ses"), redeliveryID("run")
	pool := h.spine.Pool()
	execSQL(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	execSQL(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	execSQL(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, sessionID, tenant.Organization, tenant.Project)
	execSQL(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, state) VALUES ($1, $2, $3, $4, 'running')`, runID, tenant.Organization, tenant.Project, sessionID)
	return tenant, runID
}

// bind writes a repository binding for the tenant and returns its id.
func (h *bindingSecretHarness) bind(t *testing.T, tenant coordinator.Tenant, cloneURL, connectionRef string) string {
	t.Helper()
	id := redeliveryID("repo")
	if err := h.spine.CreateRepositoryBinding(context.Background(), tenant, coordinator.RepositoryBindingInput{
		BindingID: id, Provider: "local", RepositoryIdentity: "palai/mci-007",
		CloneURL: cloneURL, DefaultBranch: "main", ConnectionRef: connectionRef,
		AllowedOperations: []string{"read"},
	}); err != nil {
		t.Fatalf("CreateRepositoryBinding() error = %v", err)
	}
	return id
}

// putSecret provisions a secret-ref through the real write-path (the engine behind POST /v1/secret-refs).
func (h *bindingSecretHarness) putSecret(t *testing.T, org, name, value string) {
	t.Helper()
	out, err := h.secrets.CreateSecretRef(context.Background(), middleware.Scope{Organization: org},
		[]byte(`{"name":"`+name+`","value":"`+value+`"}`))
	if err != nil {
		t.Fatalf("CreateSecretRef() error = %v", err)
	}
	if out.BadField || out.MissingField != "" || out.NotFound {
		t.Fatalf("CreateSecretRef() rejected the request: %+v", out)
	}
}

// TestBindingConnectionRefClonesUnderTenantCredential is MCI-007. Case 1: a binding whose connection_ref
// names a secret the tenant provisioned clones from a remote that ACCEPTS only that credential, while the
// deployment-global broker passed alongside would be rejected — so the clone proves the binding-scoped
// path end to end. Case 2: a ref-less binding never consults the resolver and still clones the old way.
// Case 3: a second tenant's binding naming the SAME ref name resolves its OWN secret and is refused by the
// first tenant's remote — a ref never crosses an organization.
func TestBindingConnectionRefClonesUnderTenantCredential(t *testing.T) {
	h := newBindingSecretHarness(t)
	ctx := context.Background()

	const tenantToken = "palai-REPMARK-tenant-git-token"
	remote := newAuthedGitRemote(t, tenantToken)

	tenant, runID := h.seedTenant(t)
	h.putSecret(t, tenant.Organization, "github-conn", tenantToken)
	bindingID := h.bind(t, tenant, remote.url, "github-conn")

	calls := 0
	target, secretsDir := filepath.Join(t.TempDir(), "repo"), t.TempDir()
	prep, err := PrepareRepository(ctx, h.spine, repositories.NewLocalBroker(), tenant, PrepareRepositoryInput{
		BindingID:         bindingID,
		RunID:             runID,
		RequestedRef:      "main",
		WorkBranch:        "agent/ses_mci007/run_mci007",
		TargetDir:         target,
		SecretsDir:        secretsDir,
		ConnectionSecrets: h.resolver(&calls),
	})
	if err != nil {
		t.Fatalf("PrepareRepository(binding-scoped credential) error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("secret resolver calls = %d, want exactly 1", calls)
	}
	if prep.Receipt.BaseCommit != remote.head {
		t.Fatalf("receipt base commit = %q, want the remote head %q", prep.Receipt.BaseCommit, remote.head)
	}
	// The clone really happened over the authenticated remote, and the receipt is durable.
	if body, err := os.ReadFile(filepath.Join(target, "README.md")); err != nil || !strings.Contains(string(body), "mci-007") {
		t.Fatalf("cloned tree = %q err=%v, want the remote's committed file", body, err)
	}
	stored, found, err := h.spine.GetPreparationReceipt(
		storage.WithTenant(ctx, tenant.Organization, tenant.Project), bindingID, runID)
	if err != nil || !found {
		t.Fatalf("GetPreparationReceipt() found=%v err=%v, want the recorded receipt", found, err)
	}
	if stored.BaseCommit != remote.head {
		t.Fatalf("stored receipt base commit = %q, want %q", stored.BaseCommit, remote.head)
	}
	// The tenant's credential is absent from every surface the run leaves behind: the workspace tree the
	// engine sees, and the /secrets staging area (the helper file is removed with the credential, §30.2).
	assertNoCredentialResidue(t, tenantToken, target, secretsDir)

	// Case 2: a ref-less binding takes the deployment-global broker, unchanged — it clones a remote that
	// needs no credential and the resolver is never consulted (nothing about today's bindings changed).
	plain := newGitRemote(t)
	plainBinding := h.bind(t, tenant, plain.url, "")
	callsBefore := calls
	if _, err := PrepareRepository(ctx, h.spine, repositories.NewLocalBroker(), tenant, PrepareRepositoryInput{
		BindingID:         plainBinding,
		RunID:             "",
		RequestedRef:      plain.head,
		TargetDir:         filepath.Join(t.TempDir(), "repo"),
		SecretsDir:        t.TempDir(),
		ConnectionSecrets: h.resolver(&calls),
	}); err != nil {
		t.Fatalf("PrepareRepository(ref-less binding) error = %v, want the unchanged global path", err)
	}
	if calls != callsBefore {
		t.Fatalf("ref-less binding consulted the secret resolver %d time(s)", calls-callsBefore)
	}

	// Case 3: a SECOND tenant with a secret of the SAME name resolves its own value (RLS, migration
	// 000031), which the first tenant's remote refuses — a binding can never redeem another org's secret.
	other, _ := h.seedTenant(t)
	h.putSecret(t, other.Organization, "github-conn", "palai-REPMARK-other-tenant-token")
	otherBinding := h.bind(t, other, remote.url, "github-conn")
	if _, err := PrepareRepository(ctx, h.spine, repositories.NewLocalBroker(), other, PrepareRepositoryInput{
		BindingID:         otherBinding,
		RunID:             "",
		RequestedRef:      "main",
		TargetDir:         filepath.Join(t.TempDir(), "repo"),
		SecretsDir:        t.TempDir(),
		ConnectionSecrets: h.resolver(&calls),
	}); err == nil {
		t.Fatal("a foreign tenant cloned the remote, want its own (rejected) credential — cross-tenant leak")
	} else if strings.Contains(err.Error(), tenantToken) {
		t.Fatalf("error disclosed the first tenant's credential: %v", err)
	}
}

// TestBindingConnectionRefFailsClosedWhenUnresolvable proves the fail-closed half against the real store:
// a binding naming a ref its organization never provisioned does NOT quietly clone under the deployment
// broker — the preparation fails, and the message names the REF, never a value.
func TestBindingConnectionRefFailsClosedWhenUnresolvable(t *testing.T) {
	h := newBindingSecretHarness(t)
	ctx := context.Background()

	remote := newGitRemote(t) // unauthenticated: only the fail-closed rule can stop this clone
	tenant, runID := h.seedTenant(t)
	bindingID := h.bind(t, tenant, remote.url, "never-provisioned")

	calls := 0
	target := filepath.Join(t.TempDir(), "repo")
	_, err := PrepareRepository(ctx, h.spine, repositories.NewLocalBroker(), tenant, PrepareRepositoryInput{
		BindingID:         bindingID,
		RunID:             runID,
		RequestedRef:      remote.head,
		TargetDir:         target,
		SecretsDir:        t.TempDir(),
		ConnectionSecrets: h.resolver(&calls),
	})
	if err == nil {
		t.Fatal("PrepareRepository with an unprovisioned connection_ref cloned anyway, want fail-closed")
	}
	if !strings.Contains(err.Error(), "never-provisioned") {
		t.Fatalf("error = %q, want it to name the unresolvable ref", err)
	}
	if _, statErr := os.Stat(filepath.Join(target, "README.md")); statErr == nil {
		t.Fatal("the tree was materialized despite the unresolvable credential")
	}
	if _, found, err := h.spine.GetPreparationReceipt(
		storage.WithTenant(ctx, tenant.Organization, tenant.Project), bindingID, runID); err != nil || found {
		t.Fatalf("a receipt was recorded for a failed preparation (found=%v err=%v)", found, err)
	}
}

// authedGitRemote is a real git remote served over HTTP by git-http-backend behind Basic auth: every
// request without the exact expected credential gets a 401, so only a clone carrying that credential can
// succeed. It is the closest honest stand-in for a private hosted repository this tier can run.
type authedGitRemote struct{ url, head string }

func newAuthedGitRemote(t *testing.T, token string) authedGitRemote {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found on PATH: %v", err)
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo.git")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("mci-007 binding-scoped clone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "initial commit")

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	backend := &cgi.Handler{Path: gitBin, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="palai"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return authedGitRemote{url: srv.URL + "/repo.git", head: run("rev-parse", "HEAD")}
}

// assertNoCredentialResidue walks the given trees and fails if the credential appears in any file name or
// content — the REP-003 absence discipline applied to the binding-scoped credential.
func assertNoCredentialResidue(t *testing.T, secret string, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if strings.Contains(path, secret) {
				t.Fatalf("credential appears in a path: %s", path)
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil // an unreadable transient file discloses nothing
			}
			if strings.Contains(string(body), secret) {
				t.Fatalf("credential appears in %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}
