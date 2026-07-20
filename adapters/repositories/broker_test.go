package repositories

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBrokerMintMaterializeRevoke proves the local broker's core credential lifecycle: a minted
// credential is opaque (the returned struct carries no token), the secret materializes only into a
// 0600 helper file, and Revoke both removes that file and drops the secret so no later use can
// redeem the handle (spec §30.2 revoke-after-operation; the internal contract behind
// TestReadCredentialRevokedAfterPreparation).
func TestBrokerMintMaterializeRevoke(t *testing.T) {
	const token = "palai-REPMARK-broker-unit-secret-zz99"
	b := NewLocalBrokerWithToken(token)
	dir := t.TempDir()

	cred, err := b.Mint(context.Background(), ScopeRead, Audience{Organization: "org_x", Run: "run_y", ToolCall: "tcall_z"})
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	if cred.Handle == "" || cred.Scope != ScopeRead {
		t.Fatalf("Mint() = %+v, want a handle scoped read", cred)
	}
	// The returned credential must not carry the raw token on ANY field (absence by construction).
	if strings.Contains(cred.Handle+cred.Username, token) {
		t.Fatal("Mint() returned the raw token in an opaque field")
	}

	helperCfg, err := b.writeHelper(cred.Handle, "https://github.com/org/repo", dir)
	if err != nil {
		t.Fatalf("writeHelper() error = %v", err)
	}
	helperPath := strings.TrimPrefix(helperCfg, "store --file=")
	body, err := os.ReadFile(helperPath)
	if err != nil {
		t.Fatalf("read helper file: %v", err)
	}
	if info, _ := os.Stat(helperPath); info != nil && info.Mode().Perm() != 0o600 {
		t.Fatalf("helper file mode = %v, want 0600", info.Mode().Perm())
	}
	if !strings.Contains(string(body), token) {
		t.Fatal("the helper file is the ONE place the token may live; it is absent")
	}

	if err := b.Revoke(context.Background(), cred.Handle); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	if _, err := os.Stat(helperPath); !os.IsNotExist(err) {
		t.Fatalf("after Revoke, helper file still exists (err=%v)", err)
	}
	if _, err := b.writeHelper(cred.Handle, "https://github.com/org/repo", dir); err == nil {
		t.Fatal("after Revoke, writeHelper must fail closed — the secret is dropped")
	}
}

// TestBrokerExpiredCredentialFailsClosed proves the minutes-scale expiry backstop (spec §28.11): a
// credential past its TTL cannot be materialized even if its handle is known.
func TestBrokerExpiredCredentialFailsClosed(t *testing.T) {
	b := NewLocalBrokerWithToken("palai-REPMARK-expired-yy88")
	past := time.Now().Add(-2 * tokenTTL)
	b.now = func() time.Time { return past }
	cred, err := b.Mint(context.Background(), ScopeRead, Audience{Run: "run_y"})
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	b.now = time.Now // clock advances past the expiry
	if _, err := b.writeHelper(cred.Handle, "https://github.com/org/repo", filepath.Join(t.TempDir())); err == nil {
		t.Fatal("expired credential materialized; want fail-closed")
	}
}

// TestValidateSubmoduleURLRejectsRCEVectors proves the untrusted-submodule transport allowlist
// (spec §30.4): ext:: (arbitrary command) and file:// (local escape) are rejected, https/ssh and
// relative URLs pass.
func TestValidateSubmoduleURLRejectsRCEVectors(t *testing.T) {
	p := Policy{}
	reject := []string{
		`ext::sh -c "touch /tmp/pwned"`,
		"file:///etc/passwd",
		"EXT::sh -c whoami",
	}
	for _, u := range reject {
		if err := p.validateSubmoduleURL(u); err == nil {
			t.Errorf("validateSubmoduleURL(%q) = nil, want rejected", u)
		}
	}
	allow := []string{
		"https://github.com/org/dep.git",
		"git@github.com:org/dep.git",
		"../sibling",
		"./nested",
	}
	for _, u := range allow {
		if err := p.validateSubmoduleURL(u); err != nil {
			t.Errorf("validateSubmoduleURL(%q) = %v, want allowed", u, err)
		}
	}
}
