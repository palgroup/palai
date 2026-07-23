//go:build uat

// The E15 T6 SH-2 RC journey — the host-agnostic EXIT proof. It is an ORCHESTRATOR (not a reimplementation):
// it drives the already-green T1-T5 live harnesses in ONE end-to-end sequence, ONE stack at a time (the 8GB
// Docker Desktop constraint), and ends in a REAL provider run. Each phase REUSES the slice that owns it:
//
//	Phase 1 (T2, OPS-005/007 + SAN-011): scripts/test/upgrade-drill.sh — two REAL builds (N = pinned fork-point,
//	  N+1 = current tree); a fake-provider run ACTIVE on N SURVIVES `palai upgrade` on its PINNED engine and
//	  completes; the engine alias rolls for NEW runs; a REAL provider smoke on N+1 (a chatcmpl-…); `palai upgrade
//	  rollback` runs the N binary on the expanded schema; the old-stamp runner is rejected (OPS-008). This is the
//	  drain-before-recreate (T2 MF-3) surviving-run integration — the load-bearing SH-2 proof.
//	Phase 2 (T5, DR-001 + DR-002/004..006): tests/uat/dr TestDRDrills — five measured/detection drills on two
//	  isolated stacks, RPO/RTO recomputed from raw timestamps (the measurement anti-fabrication anchor).
//	Phase 3 (T4, OPS-004): deploy/airgap — the signed bundle builds, re-verifies, and is fail-closed on tamper;
//	  best-effort the OFFLINE `verify.sh --network-none` topological proof if a tool image is present.
//	Phase 4 (T3, OPS-003): tests/uat/kubernetes — helm lint + render + policy asserts (no ClusterRole, restricted
//	  posture); the `kind` install smoke (make uat-kind) is the cluster-adjacent live leg, referenced not run.
//
// HOST-AGNOSTIC / HONEST CEILING (plan §6): the local seam is two-local-build upgrade, a same-host two-stack DR,
// an internal-network air-gap, and a kind cluster. A published-release-to-release upgrade, a real managed-K8s
// with an enforcing CNI, a real air-gapped facility, and a separate-physical-host/second-site DR are the §6
// operator legs — the harness is parametric (env passthrough) so an operator points each phase at real infra
// unchanged. It does NOT write the committed self-host-0.2.0 bundle (authored data verified by the Docker-free
// core); it proves the real thing independently. Credentials load ONLY from .env.local (make uat-sh2
// PROVIDER=provider-one) — never argv, never a log, never set -x.
package upgrade

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// chatcmplRe extracts the real provider request id the upgrade drill's N+1 smoke commits to model_requests.
var chatcmplRe = regexp.MustCompile(`chatcmpl-[A-Za-z0-9_-]+`)

// TestSH2Journey is the E15 EXIT gate live journey. It ends in a REAL provider-one completion (phase 1's N+1
// smoke). It is Docker- + credential-bound and only runs under PALAI_UAT_PROVIDER=provider-one, so it never
// rides make verify.
func TestSH2Journey(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the SH-2 journey brings up real stacks")
	}
	if os.Getenv("PALAI_UAT_PROVIDER") != "provider-one" {
		t.Skip("the SH-2 journey ends in a REAL provider run: set PALAI_UAT_PROVIDER=provider-one + OPENAI_API_KEY (make uat-sh2 PROVIDER=provider-one)")
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Fatal("OPENAI_API_KEY is unset; the operator entry loads it from .env.local")
	}
	root := repoRoot(t)

	// --- Phase 1: the surviving-run upgrade + both rollbacks + a REAL provider smoke (T2, one stack) ---
	// scripts/test/upgrade-drill.sh does its own two-build bring-up, active run, upgrade, rollback, real smoke,
	// and 0-leak teardown. We run it, then assert the load-bearing invariants from its output.
	out := phase(t, root, 55*time.Minute, "bash", "scripts/test/upgrade-drill.sh")
	if !strings.Contains(out, "active run survived the upgrade") {
		t.Fatalf("phase 1: the active run did not survive the N->N+1 upgrade — see the drill output")
	}
	if !strings.Contains(out, "rollback: N control-plane serving on expanded schema") {
		t.Fatalf("phase 1: app rollback to the N binary on the expanded schema was not confirmed")
	}
	chatcmpl := chatcmplRe.FindString(out)
	if chatcmpl == "" {
		t.Fatalf("phase 1: no real provider request id (chatcmpl-…) — the journey must end in a REAL provider run")
	}
	t.Logf("PHASE 1 PASS (T2): surviving run completed across the upgrade; app + engine-alias rollback drained; REAL provider run %s", chatcmpl)

	// --- Phase 2: the measured DR drills (T5, two stacks, sequential after phase 1 tore down) ---
	phase(t, root, 45*time.Minute, "go", "test", "-tags", "uat", "-count=1", "-timeout", "40m", "-run", "TestDRDrills", "./tests/uat/dr")
	t.Logf("PHASE 2 PASS (T5): DR-001 primary-loss + DR-002/004..006 drills green; RPO/RTO recomputed from raw timestamps")

	// --- Phase 3: the signed air-gap bundle build + verify + tamper (T4) ---
	phase(t, root, 20*time.Minute, "go", "test", "-count=1", "-run", "TestBundleBuildsAndVerifies|TestVerifyFailsOnTamperedComponent|TestVerifyRejectsWrongKey", "./deploy/airgap")
	t.Logf("PHASE 3 PASS (T4): air-gap bundle built, signature+digest re-verified, tamper fail-closed. Offline --network none is deploy/airgap/verify.sh --network-none (operator/tool-image gated)")

	// --- Phase 4: the restricted helm chart render/policy asserts (T3) ---
	phase(t, root, 10*time.Minute, "go", "test", "-count=1", "./tests/uat/kubernetes")
	t.Logf("PHASE 4 PASS (T3): helm lint + render + policy asserts green (no ClusterRole, restricted posture). Kind install smoke is make uat-kind (Docker+kind gated); NetworkPolicy ENFORCEMENT is the §6 operator leg")

	t.Logf("SH-2 JOURNEY PASS: surviving-run upgrade + drained rollbacks (real run %s), measured DR, air-gap verify, helm render — LOCAL seam only; §6 operator legs named, not claimed", chatcmpl)
}

// phase runs one orchestrated sub-harness under a deadline, streaming its output to the test log and failing the
// journey on non-zero exit. It returns the combined output for assertion. The child inherits the environment
// (OPENAI_API_KEY from .env.local via make uat-sh2) — the secret is never placed on argv here.
func phase(t *testing.T, root string, timeout time.Duration, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = root
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	// Stream the child output (the sub-harnesses already redact their own secrets; they never echo the key).
	t.Logf("$ %s %s\n%s", name, strings.Join(args, " "), out)
	if err != nil {
		t.Fatalf("phase %q failed: %v", name+" "+strings.Join(args, " "), err)
	}
	return string(out)
}
