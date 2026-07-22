package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWebhookSecretResolverIsOrgScoped pins F2: the env-key namespace is scoped by org, so a tenant's
// SigningSecretRef can only reach a secret provisioned under its OWN org — naming another org's ref
// resolves to no env var (no cross-tenant HMAC-forgery oracle). The org prefix is server-minted, so it
// is a hard tenant boundary.
func TestWebhookSecretResolverIsOrgScoped(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "b.secret")
	if err := os.WriteFile(secretFile, []byte("whsec_org_b"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	// Only org_b's "shared" ref is bridged.
	t.Setenv("PALAI_WEBHOOK_SECRET_FILE_"+secretEnvKey("org_b")+"__"+secretEnvKey("shared"), secretFile)

	// org_a naming the same ref resolves nothing — it cannot reach org_b's secret.
	if _, err := webhookSecretResolver("org_a", "shared"); err == nil {
		t.Fatal("org_a resolved a secret bridged only under org_b — env namespace is not org-scoped")
	}
	// org_b resolves its own secret.
	got, err := webhookSecretResolver("org_b", "shared")
	if err != nil {
		t.Fatalf("org_b failed to resolve its own secret: %v", err)
	}
	if string(got) != "whsec_org_b" {
		t.Fatalf("resolved secret = %q, want whsec_org_b", got)
	}
}

// TestRemoteToolSecretResolverIsOrgScoped pins the E12 T4 secret hygiene: the remote-tool HMAC secret
// (which signs the outbound invoke AND verifies the inbound callback) is bridged as a FILE PATH under an
// org-scoped, namespace-DISTINCT env key. A tenant's secret_ref can only reach a secret provisioned under
// its OWN org, and the remote-tool namespace never collides with the webhook/inbound ones — the three
// secret sets are non-interchangeable. The raw secret is never an env value, argument, or log line.
func TestRemoteToolSecretResolverIsOrgScoped(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "b.secret")
	if err := os.WriteFile(secretFile, []byte("rtsec_org_b"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	// Only org_b's "sig-ref" is bridged, under the remote-tool namespace.
	t.Setenv("PALAI_REMOTE_TOOL_SECRET_FILE_"+secretEnvKey("org_b")+"__"+secretEnvKey("sig-ref"), secretFile)

	// org_a naming the same ref resolves nothing — it cannot reach org_b's secret.
	if _, err := remoteToolSecretResolver("org_a", "sig-ref"); err == nil {
		t.Fatal("org_a resolved a remote-tool secret bridged only under org_b — env namespace is not org-scoped")
	}
	// A webhook/inbound bridge for the SAME (org, ref) does NOT satisfy the remote-tool resolver (distinct
	// namespaces) — the three secret sets are non-interchangeable.
	t.Setenv("PALAI_WEBHOOK_SECRET_FILE_"+secretEnvKey("org_b")+"__"+secretEnvKey("only-webhook"), secretFile)
	if _, err := remoteToolSecretResolver("org_b", "only-webhook"); err == nil {
		t.Fatal("the remote-tool resolver read a WEBHOOK-namespaced secret — namespaces must be non-interchangeable")
	}
	// org_b resolves its own remote-tool secret from the file (a PATH, never inline bytes).
	got, err := remoteToolSecretResolver("org_b", "sig-ref")
	if err != nil {
		t.Fatalf("org_b failed to resolve its own remote-tool secret: %v", err)
	}
	if string(got) != "rtsec_org_b" {
		t.Fatalf("resolved secret = %q, want rtsec_org_b", got)
	}
}

// TestSecretResolverRejectsAmbiguousOrgKey pins a belt-and-braces guard (E11 T4 residual): an org whose
// normalized env-key form contains the "__" org/ref delimiter would make PALAI_..._SECRET_FILE_<ORG>__<REF>
// ambiguous with a different (org, ref) split, so BOTH secret resolvers reject it rather than resolve a
// colliding key. The org is server-minted (never tenant-forgeable), so this is defence-in-depth on top of
// the org-scoping tenant boundary, not the primary control.
func TestSecretResolverRejectsAmbiguousOrgKey(t *testing.T) {
	const ambiguous = "acme__evil" // normalizes to ACME__EVIL — carries the "__" org/ref delimiter
	for name, resolver := range map[string]func(string, string) ([]byte, error){
		"webhook":     webhookSecretResolver,
		"inbound":     inboundSecretResolver,
		"remote-tool": remoteToolSecretResolver,
	} {
		if _, err := resolver(ambiguous, "shared"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("%s resolver on an ambiguous org key: err = %v, want an 'ambiguous' rejection", name, err)
		}
	}
}

// TestArtifactGCGraceFloorsTinyValue proves a too-small configured grace cannot collapse the
// GC's primary write-safety guard: a typo'd sub-floor value (e.g. "1s") is clamped up to the
// floor, while a value at or above the floor is honored unchanged. Without the floor a live
// in-flight write could be reclaimed before its row commits.
func TestArtifactGCGraceFloorsTinyValue(t *testing.T) {
	if got := artifactGCGrace(time.Second); got != minArtifactGCGrace {
		t.Fatalf("artifactGCGrace(1s) = %s, want the %s floor", got, minArtifactGCGrace)
	}
	if got := artifactGCGrace(minArtifactGCGrace); got != minArtifactGCGrace {
		t.Fatalf("artifactGCGrace(floor) = %s, want %s unchanged", got, minArtifactGCGrace)
	}
	if got := artifactGCGrace(time.Hour); got != time.Hour {
		t.Fatalf("artifactGCGrace(1h) = %s, want 1h honored", got)
	}
}
