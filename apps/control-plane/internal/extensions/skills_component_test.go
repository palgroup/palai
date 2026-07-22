//go:build component

package extensions

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// skillResolverFunc is the injectable DNS seam: a test maps a PUBLIC hostname to a public IP so the
// egress vet (allowPrivate=false) passes, while the dial redirects the bytes to a local test server.
type skillResolverFunc func(ctx context.Context, host string) ([]net.IPAddr, error)

func (f skillResolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

func publicSkillResolver() skillResolverFunc {
	return func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil // TEST-NET-3, a public (non-special) address
	}
}

// skillDial ignores the vetted address and connects to the real local listener — the research_test
// idiom that lets a public-looking hostname serve from an httptest server.
func skillDial(target string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
}

func trustSkillServer(srv *httptest.Server) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &tls.Config{RootCAs: pool}
}

func serveSkill(t *testing.T, archive []byte) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("skill fetch carried a credential header — install must send none")
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive)
	}))
}

func TestSkillInstallByURLQuarantinesComputesDigestParsesMetadata(t *testing.T) {
	store, org, project := openStore(t)
	ctx := context.Background()

	archive := buildTGZ(t, tgzEntry{name: "SKILL.md", body: []byte(
		"---\nname: commit-convention\ndescription: write conventional commit messages\nrequired_tools:\n  - push\n---\nAlways use conventional commits.\n")})
	srv := serveSkill(t, archive)
	defer srv.Close()

	skill, err := store.CreateSkill(ctx, org, project, "commit-convention")
	if err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	rev, err := store.InstallSkillRevisionFromURL(ctx, org, project, skill.ID, "https://example.com/commit.tgz",
		WithSkillResolver(publicSkillResolver()), WithSkillDialContext(skillDial(srv.Listener.Addr().String())), WithSkillTLSConfig(trustSkillServer(srv)))
	if err != nil {
		t.Fatalf("InstallSkillRevisionFromURL: %v", err)
	}
	if !strings.HasPrefix(rev.Digest, "sha256:") {
		t.Fatalf("digest = %q, want a sha256 content address over the sanitized archive", rev.Digest)
	}
	if rev.State != "approved" {
		t.Fatalf("state = %q, want approved for a clean scan", rev.State)
	}
	if rev.Metadata.Name != "commit-convention" || rev.Metadata.Description == "" {
		t.Fatalf("metadata = %+v, want parsed name+description from SKILL.md frontmatter", rev.Metadata)
	}
	if len(rev.Metadata.RequiredTools) != 1 || rev.Metadata.RequiredTools[0] != "push" {
		t.Fatalf("required_tools = %v, want [push] (a REQUEST, not a grant)", rev.Metadata.RequiredTools)
	}
	// The digest addresses the sanitized archive: the stored bytes are loadable and re-quarantine equal.
	stored, err := store.LoadSkillArchive(ctx, org, project, rev.Digest)
	if err != nil || len(stored) == 0 {
		t.Fatalf("LoadSkillArchive(%s) = %d bytes, %v; want the sanitized tar", rev.Digest, len(stored), err)
	}
}

func TestScanFailureBlocksEnable(t *testing.T) {
	store, org, project := openStore(t)
	ctx := context.Background()

	// Clean skill → approved → enable succeeds.
	clean := buildTGZ(t, tgzEntry{name: "SKILL.md", body: []byte("---\nname: clean\ndescription: fine\n---\nprose\n")})
	cs, _ := store.CreateSkill(ctx, org, project, "clean")
	cleanRev, err := store.InstallSkillRevision(ctx, org, project, cs.ID, clean, "")
	if err != nil {
		t.Fatalf("install clean: %v", err)
	}
	if cleanRev.State != "approved" {
		t.Fatalf("clean state = %q, want approved", cleanRev.State)
	}
	if exists, err := store.EnableSkillRevision(ctx, org, project, cleanRev.ID); err != nil || !exists {
		t.Fatalf("EnableSkillRevision(clean) = exists=%v err=%v, want enabled", exists, err)
	}

	// Skill with a committed secret in SKILL.md → quarantined, findings, enable REFUSED.
	dirty := buildTGZ(t, tgzEntry{name: "SKILL.md", body: []byte("---\nname: dirty\ndescription: leaks\n---\ntoken: sk-ABCDEF0123456789\n")})
	ds, _ := store.CreateSkill(ctx, org, project, "dirty")
	dirtyRev, err := store.InstallSkillRevision(ctx, org, project, ds.ID, dirty, "")
	if err != nil {
		t.Fatalf("install dirty: %v", err)
	}
	if dirtyRev.State != "quarantined" || len(dirtyRev.Findings) == 0 {
		t.Fatalf("dirty rev = state %q findings %v, want quarantined with a finding", dirtyRev.State, dirtyRev.Findings)
	}
	switch _, err := store.EnableSkillRevision(ctx, org, project, dirtyRev.ID); {
	case errors.Is(err, ErrScanFindingsBlockEnable):
		// correct: scan FAIL blocks enable
	default:
		t.Fatalf("EnableSkillRevision(dirty) err = %v, want ErrScanFindingsBlockEnable", err)
	}
	// Confirm it never left quarantined.
	after, _, _ := store.GetSkillRevision(ctx, org, project, dirtyRev.ID)
	if after.State != "quarantined" {
		t.Fatalf("dirty rev after enable attempt = %q, want still quarantined", after.State)
	}
}

func TestModelCannotInstallSkill(t *testing.T) {
	store, org, project := openStore(t)
	ctx := context.Background()

	// Install + enable a skill named after a tool verb — a prompt-injection would love this.
	archive := buildTGZ(t, tgzEntry{name: "SKILL.md", body: []byte("---\nname: push\ndescription: pretends to be a tool\n---\nuse the push tool\n")})
	sk, _ := store.CreateSkill(ctx, org, project, "push")
	rev, err := store.InstallSkillRevision(ctx, org, project, sk.ID, archive, "")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := store.EnableSkillRevision(ctx, org, project, rev.ID); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// Enabling a skill creates ZERO tool rows — a skill is NOT a tool (TOL-011). The model's only surface
	// is the tool broker; a skill never becomes a broker-dispatchable tool, and install is a scope-gated
	// admin Store method with no ExecEnv, so no model-reachable path installs one.
	var toolCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM tools WHERE organization_id=$1 AND project_id=$2`, org, project).Scan(&toolCount); err != nil {
		t.Fatalf("count tools: %v", err)
	}
	if toolCount != 0 {
		t.Fatalf("enabling a skill created %d tool rows, want 0 — a skill must never become a tool", toolCount)
	}
}
