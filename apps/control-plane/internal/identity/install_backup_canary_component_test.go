//go:build component

// This proves the E14 T4 install-restore SECRET CANARY at the component tier, against a real
// PostgreSQL with two different master keys — the path the base-profile two-stack backup proof
// cannot exercise (the base profile has no secret store). A secret sealed under master key A must:
//   - decrypt under A with the SAME AES-256-GCM construction `palai restore verify` uses (openSealed
//     in cmd/cli/internal/stack — the CLI cannot import this internal package, so the shared seal
//     FORMAT is pinned here against a real stored ciphertext), and
//   - fail closed under a different master key B — both via that stdlib open AND via the control-
//     plane's own Resolve (ErrSecretDecrypt), which is the exact production symptom of a restore that
//     did not carry the source master key.
package identity_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/storage"
)

func TestInstallRestoreSecretCanaryTwoMasterKeys(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())
	keyA := masterKey(t)
	keyB := masterKey(t) // a DIFFERENT per-host master key — the restore-to-a-fresh-stack case

	org, _, _ := provisionOrg(t, idstore, "sec-canary")
	scope := middleware.Scope{Organization: org}

	// Seal a secret under master key A (the source stack's key).
	storeA := identity.NewSecretStore(cs.Pool(), keyA)
	if _, err := storeA.CreateSecretRef(ctx, scope, []byte(`{"name":"provider-one","value":"sk-live"}`)); err != nil {
		t.Fatalf("CreateSecretRef: %v", err)
	}

	// Read the raw ciphertext exactly as the canary does (SELECT encode(ciphertext,'hex')).
	var ctHex string
	if err := cs.Pool().QueryRow(storage.WithTenant(ctx, org, ""),
		"SELECT encode(ciphertext, 'hex') FROM secret_refs LIMIT 1").Scan(&ctHex); err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	sealed, err := hex.DecodeString(ctHex)
	if err != nil {
		t.Fatalf("decode ciphertext: %v", err)
	}

	// The canary's stdlib GCM open (byte-identical to cmd/cli/internal/stack.openSealed): key A opens
	// the real stored blob, proving format interop; key B fails closed.
	if got, err := canaryOpen(keyA, sealed); err != nil || string(got) != "sk-live" {
		t.Fatalf("canary must decrypt under the source key: got %q err %v", got, err)
	}
	if _, err := canaryOpen(keyB, sealed); err == nil {
		t.Fatalf("canary MUST fail closed under a mismatched master key")
	}

	// And the control-plane's own resolver fails closed with ErrSecretDecrypt under the wrong key —
	// the silent-death symptom the canary exists to catch before the first provider call.
	if _, ok, err := identity.NewSecretStore(cs.Pool(), keyB).Resolve(ctx, org, "provider-one"); ok || !errors.Is(err, identity.ErrSecretDecrypt) {
		t.Fatalf("Resolve under a mismatched master key must be ErrSecretDecrypt, got ok=%v err=%v", ok, err)
	}
}

// canaryOpen mirrors cmd/cli/internal/stack.openSealed EXACTLY (nonce||ciphertext, AES-256-GCM). The
// CLI canary cannot import this internal package, so the shared seal format is pinned here against a
// real identity-sealed ciphertext.
func canaryOpen(key, sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	n := gcm.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("ciphertext shorter than the nonce")
	}
	return gcm.Open(nil, sealed[:n], sealed[n:], nil)
}
