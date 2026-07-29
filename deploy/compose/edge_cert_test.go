// The edge TLS pair and the runner gateway's server certificate are two DIFFERENT identities with
// incompatible requirements, and until this test existed the production overlay forced them onto one
// file (${PALAI_HOME}/ca/server.crt):
//
//   - the edge (Caddy) must present a certificate for the operator's DOMAIN;
//   - the runner gateway's certificate is pinned by packages/runner to EXACTLY one DNS SAN equal to
//     PALAI_CONTROLLER_DNS ("control-plane") — enrollment.go, session.go and renewal.go each refuse
//     anything else, including a certificate that carries "control-plane" AND the domain.
//
// So every certificate that satisfied the edge broke the runner, and it broke it LATE: the running
// control-plane holds its cert in memory, so the stack looks healthy until the next restart, and the
// symptom is then "runs stay queued forever" with the word "certificate" in no message anywhere.
// Measured 2026-07-29 against a live production bring-up (docs/operations/cloud-smoke-report.md,
// Bulgu 3).
//
// This test is the static half of the fix: the edge mount must be operator-overridable and must
// DEFAULT to the byte-identical path it used before, so an install that sets neither variable is
// unchanged. The live half (a palai.example.com edge certificate served while the runner keeps
// enrolling across a control-plane restart) is the operator proof in the same document.
package compose

import (
	"os"
	"strings"
	"testing"
)

// The exact mount sources the overlay must declare. Written out in full rather than assembled from
// parts: the assertion is byte-equality, so neither the override variable nor the default can drift
// without this test saying which one did.
const (
	edgeCertSource = "${PALAI_EDGE_CERT:-${PALAI_HOME}/ca/server.crt}:/etc/palai/edge/edge.crt:ro"
	edgeKeySource  = "${PALAI_EDGE_KEY:-${PALAI_HOME}/ca/server.key}:/etc/palai/edge/edge.key:ro"
)

// TestEdgeCertIsSeparableFromTheRunnerPinnedIdentity proves the operator can give the edge its own
// certificate WITHOUT touching the file the runner pins — and that doing nothing changes nothing.
func TestEdgeCertIsSeparableFromTheRunnerPinnedIdentity(t *testing.T) {
	edge, ok := loadComposeDoc(t, "production.yml").Services["edge"]
	if !ok {
		t.Fatal("production.yml declares no edge service")
	}
	want := []string{edgeCertSource, edgeKeySource}
	if len(edge.Volumes) != len(want) {
		t.Fatalf("edge declares %d volume(s), want %d: %q", len(edge.Volumes), len(want), edge.Volumes)
	}
	for i, w := range want {
		if edge.Volumes[i] != w {
			t.Errorf("edge volume %d is\n  %q\nwant\n  %q\n(the edge must be overridable AND default to the byte-identical pre-fix path)", i, edge.Volumes[i], w)
		}
	}
}

// TestRunnerGatewayCertStaysUnparameterised proves the fix cannot be misused to point the RUNNER
// gateway at the operator's domain certificate: its cert path is a literal in-container path under
// the read-only ${PALAI_HOME}/ca mount, and neither edge variable appears in the base file at all.
// If a future edit wires PALAI_EDGE_CERT into the control-plane, the delayed breakage is back.
func TestRunnerGatewayCertStaysUnparameterised(t *testing.T) {
	cp, ok := loadComposeDoc(t, "compose.yaml").Services["control-plane"]
	if !ok {
		t.Fatal("compose.yaml declares no control-plane service")
	}
	env := cp.Environment
	for key, want := range map[string]string{
		"PALAI_RUNNER_SERVER_CERT": "/palai/ca/server.crt",
		"PALAI_RUNNER_SERVER_KEY":  "/palai/ca/server.key",
	} {
		if got := env[key]; got != want {
			t.Errorf("control-plane %s = %q, want the literal %q", key, got, want)
		}
	}
	raw, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"PALAI_EDGE_CERT", "PALAI_EDGE_KEY"} {
		if strings.Contains(string(raw), v) {
			t.Errorf("compose.yaml mentions %s — the runner gateway's identity must not be operator-steerable, it is pinned to a single SAN", v)
		}
	}
}
