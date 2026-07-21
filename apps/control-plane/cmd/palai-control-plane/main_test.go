package main

import (
	"os"
	"path/filepath"
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
