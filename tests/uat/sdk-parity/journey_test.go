//go:build uat

// The E16 T8 SDK-parity + provider-completeness EXIT journey — the host-agnostic capstone proof. Like the E15
// T6 SH-2 journey it is an ORCHESTRATOR (not a reimplementation): it drives the already-green live harness in ONE
// end-to-end sequence and ends in REAL provider runs. The load-bearing journey lives in the control-plane `live`
// package (apps/control-plane/internal/execution/live/sdk_parity_journey_test.go, TestSDKParityJourney) where the
// engine dialer, provisioning, and orchestrator helpers already exist; this wrapper runs it through the
// scripts/test/live-provider harness, which stands up a throwaway Postgres + the reference engine and sources
// BOTH credentials (OPENAI_API_KEY + ANTROPHIC_API_KEY) from the git-ignored .env.local into the test process —
// never argv, never a log, never set -x.
//
// What the journey proves LIVE (see the live file header for the full ceilings): the FOUR clients (TS/Python/Go
// SDKs + the palai CLI) decode the SAME real provider-one run byte-identically (the crown, a mechanical
// canonical diff — a uat.ThreeLanguageEqualityProof built from the four real outputs must pass Complete());
// retained retrieval survives a server restart; the openai-compatible STAND-IN gateway is killed and the direct
// provider-one route still serves a real run (a uat.GatewayOffProof); and a provider-two (Anthropic) route serves
// a real completion (a msg_ id). HONEST CEILING (plan §6): the stand-in gateway is a local proxy — a real
// LiteLLM/private-server gateway drill is the §6 operator leg; published npm/PyPI/Go-proxy releases are E18.
package sdkparity

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestSDKParityJourney is the E16 EXIT gate live journey. It ends in REAL provider-one + provider-two
// completions, so it is Docker- + credential-bound and only runs under PALAI_UAT_PROVIDER=provider-one — it never
// rides make verify.
func TestSDKParityJourney(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the SDK-parity journey brings up a throwaway Postgres")
	}
	if os.Getenv("PALAI_UAT_PROVIDER") != "provider-one" {
		t.Skip("the SDK-parity journey ends in REAL provider runs: set PALAI_UAT_PROVIDER=provider-one + OPENAI_API_KEY + ANTROPHIC_API_KEY (make uat-sdk-parity PROVIDER=provider-one)")
	}
	if os.Getenv("OPENAI_API_KEY") == "" || os.Getenv("ANTROPHIC_API_KEY") == "" {
		t.Fatal("the SDK-parity journey needs BOTH OPENAI_API_KEY and ANTROPHIC_API_KEY; the operator entry loads them from .env.local")
	}
	for _, bin := range []string{"go", "node", "uv"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s not on PATH: the four-client parity leg needs go + node + uv on the workstation", bin)
		}
	}
	root := repoRoot(t)

	// Drive the live journey through the shared harness (it stands up the throwaway Postgres + engine and runs
	// TestSDKParityJourney under -tags live). CASE=sdk-parity-journey is registered in scripts/test/live-provider.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "scripts/test/live-provider")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PROVIDER=provider-one", "CASE=sdk-parity-journey")
	out, err := cmd.CombinedOutput()
	t.Logf("$ PROVIDER=provider-one CASE=sdk-parity-journey scripts/test/live-provider\n%s", out)
	if err != nil {
		t.Fatalf("SDK-parity live journey failed: %v", err)
	}
	// Assert the load-bearing PASS markers, not just a zero exit — the crown (four-client parity), the gateway-off
	// direct path, and the second real provider must each be present.
	text := string(out)
	for _, marker := range []string{
		"PARITY PASS (crown)",
		"GATEWAY-OFF PASS",
		"PROVIDER-TWO PASS",
		"live_provider_one_sdk_parity_journey=PASS",
	} {
		if !strings.Contains(text, marker) {
			t.Fatalf("SDK-parity journey did not report %q — see the harness output", marker)
		}
	}
	t.Logf("SDK-PARITY JOURNEY PASS: four-client mechanical equality + gateway-off direct path + real provider-two, driven through the shared live harness")
}
