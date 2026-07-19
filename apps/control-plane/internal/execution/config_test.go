package execution

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestConfigSnapshotContentAddressedWithProvenance proves the resolver is content-addressed
// (same input → same hash) and records the layer that set each effective value (spec §14;
// SES-008 snapshot half). It also proves redaction: the credential ref stays a reference, so
// no secret value ever enters the snapshot (LP secret-hygiene pattern).
func TestConfigSnapshotContentAddressedWithProvenance(t *testing.T) {
	deployment := ResolveInput{
		DeploymentModel:  "model-alpha",
		DeploymentSecret: "openai_api_key", // a ref NAME, never the value
		ProjectTools:     []string{"palai.conformance.math.add"},
	}

	// No session override: model comes from the deployment, tools from the project baseline.
	base := Resolve(deployment)
	if base.Model != "model-alpha" {
		t.Fatalf("effective model = %q, want the deployment default model-alpha", base.Model)
	}
	if base.Provenance["model"] != layerDeployment {
		t.Fatalf("model provenance = %q, want %q", base.Provenance["model"], layerDeployment)
	}
	if base.Provenance["tools"] != layerProject {
		t.Fatalf("tools provenance = %q, want %q", base.Provenance["tools"], layerProject)
	}

	// Content addressing: the identical input resolves to the identical hash.
	if again := Resolve(deployment); again.Hash != base.Hash {
		t.Fatalf("same input produced different hashes: %q vs %q", base.Hash, again.Hash)
	}
	if !strings.HasPrefix(base.Hash, "sha256:") {
		t.Fatalf("hash = %q, want a sha256: content address", base.Hash)
	}

	// A session model override flips only the model's provenance to session and re-addresses.
	switched := deployment
	switched.SessionModel = "model-beta"
	snap := Resolve(switched)
	if snap.Model != "model-beta" || snap.Provenance["model"] != layerSession {
		t.Fatalf("session override: model = %q prov = %q, want model-beta from session", snap.Model, snap.Provenance["model"])
	}
	if snap.Provenance["tools"] != layerProject {
		t.Fatalf("tools provenance after a model-only override = %q, want it to stay project", snap.Provenance["tools"])
	}
	if snap.Hash == base.Hash {
		t.Fatal("a model switch must change the content address, but the hash was unchanged")
	}

	// Redaction: the ref name is carried, but the snapshot JSON holds no credential value.
	blob, _ := json.Marshal(snap)
	if snap.SecretRef != "openai_api_key" {
		t.Fatalf("secret ref = %q, want the reference name preserved", snap.SecretRef)
	}
	if strings.Contains(string(blob), "sk-") || strings.Contains(string(blob), "secret-value") {
		t.Fatalf("snapshot leaked a credential value: %s", blob)
	}
}
