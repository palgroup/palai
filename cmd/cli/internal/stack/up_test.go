package stack

import (
	"io"
	"os"
	"strings"
	"testing"
)

// up_test.go covers `palai up`'s refusals and its crown assertion with no Docker: every decision
// that can silently downgrade a bring-up to the fake adapter is a pure function here, and the
// docker-bound half (the same proveLive run against a REAL fake stack) is up_e2e_test.go.

// env builds the lookup Bootstrap uses, without touching the process environment.
func env(pairs ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(k string) string { return m[k] }
}

// TestFakeCompletionIsRefused is THE guarantee: a run that completed on the deterministic adapter
// must not be accepted as evidence of a live stack. RED before proveLive existed — a bring-up that
// asserted only `status == completed` accepted exactly this response and reported success.
func TestFakeCompletionIsRefused(t *testing.T) {
	err := proveLive(roundTrip{ResponseID: "resp_1", Status: "completed", Model: "fake"})
	if err == nil {
		t.Fatal("proveLive accepted a completed run on model \"fake\" — the fake stack would report success")
	}
	if !strings.Contains(err.Error(), "THE STACK IS FAKE") || !strings.Contains(err.Error(), "PALAI_MODEL_PROVIDER") {
		t.Fatalf("the refusal must name the fault AND the fix, got: %v", err)
	}
}

// TestProveLiveRefusesEveryFakeableCompletion pins the three completions that a weaker assertion
// would wave through. Each is a distinct way to be fake while looking finished.
func TestProveLiveRefusesEveryFakeableCompletion(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   roundTrip
		want string
	}{
		{"empty model sails past a bare model!=fake check", roundTrip{ResponseID: "r", Status: "completed"}, "NO model"},
		{"a completed run is not by itself a live one", roundTrip{ResponseID: "r", Status: "failed", Model: "gpt-4o-mini"}, "not completed"},
		{"zero usage is the fake script's other signature", roundTrip{ResponseID: "r", Status: "completed", Model: "gpt-4o-mini"}, "ZERO tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := proveLive(tc.rt)
			if err == nil {
				t.Fatalf("proveLive accepted %+v", tc.rt)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal detail = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestProveLiveAcceptsARealRoundTrip is the GREEN side: the assertion must not be so strict that a
// genuine provider response fails it (a proof nothing can satisfy proves nothing).
func TestProveLiveAcceptsARealRoundTrip(t *testing.T) {
	if err := proveLive(roundTrip{
		ResponseID: "resp_1", Status: "completed", Model: "gpt-4o-mini-2024-07-18",
		InputTokens: 47, OutputTokens: 4, TotalTokens: 51,
	}); err != nil {
		t.Fatalf("proveLive rejected a real round-trip: %v", err)
	}
}

// TestUnrecognisedSelectorIsRefused: any value other than provider-one selects the fake adapter in
// the control-plane WITHOUT WARNING, so the CLI must refuse it here rather than inherit it.
func TestUnrecognisedSelectorIsRefused(t *testing.T) {
	for _, sel := range []string{"openai", "anthropic", "provider_one", "Provider-One", "provider-two"} {
		_, err := resolveProvider(env("PALAI_MODEL_PROVIDER", sel, credentialEnv, "sk-test"), false)
		if err == nil {
			t.Fatalf("selector %q was accepted — the stack would have come up fake", sel)
		}
		if !strings.Contains(err.Error(), "provider-one") {
			t.Fatalf("refusal for %q must name the recognised value, got: %v", sel, err)
		}
	}
}

// TestExplicitFakeSelectorIsRefusedWithItsOwnReason: "fake" IS a working selector for `local up`,
// so its refusal points at that command instead of implying a typo.
func TestExplicitFakeSelectorIsRefusedWithItsOwnReason(t *testing.T) {
	_, err := resolveProvider(env("PALAI_MODEL_PROVIDER", "fake", credentialEnv, "sk-test"), false)
	if err == nil || !strings.Contains(err.Error(), "palai local up") {
		t.Fatalf("an explicit fake selector must point at `palai local up`, got: %v", err)
	}
}

// TestMissingCredentialIsAnError: no key anywhere means the stack would come up fake. The message
// must name the file, the variable and the alternative.
func TestMissingCredentialIsAnError(t *testing.T) {
	_, err := resolveProvider(env(), false)
	if err == nil {
		t.Fatal("a bring-up with no credential was accepted")
	}
	for _, want := range []string{".env.local", credentialEnv, "palai provider add provider-one"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error must name %q, got: %v", want, err)
		}
	}
}

// TestAnthropicOnlyIsRefusedWithTheRealReason: an Anthropic key is a credential, but not one the
// compose deployment default can use — the entrypoint bridges provider_one_key alone. Saying
// "no credential" there would send the operator hunting for a key they already have.
func TestAnthropicOnlyIsRefusedWithTheRealReason(t *testing.T) {
	c, err := resolveProvider(env("ANTROPHIC_API_KEY", "sk-ant-test"), false)
	if err == nil {
		t.Fatal("an Anthropic-only environment was accepted for a provider-one bring-up")
	}
	if !strings.Contains(err.Error(), "provider-two") || !strings.Contains(err.Error(), credentialEnv) {
		t.Fatalf("the error must explain WHY the key is unusable and what to set, got: %v", err)
	}
	if len(c.warnings) != 1 || !strings.Contains(c.warnings[0], "mis-spelling") {
		t.Fatalf("the ANTROPHIC mis-spelling must be warned about, got: %v", c.warnings)
	}
}

// TestStoredSecretNeedsNoEnvCredential: an operator who already ran `provider add` must not be
// asked for the value again, and the source line must say the credential was not re-read.
func TestStoredSecretNeedsNoEnvCredential(t *testing.T) {
	c, err := resolveProvider(env(), true)
	if err != nil {
		t.Fatalf("a stack with a stored credential was refused: %v", err)
	}
	if c.credential != "" {
		t.Fatal("the stored-secret path must not carry a credential value")
	}
	if !strings.Contains(c.source, "provider add") {
		t.Fatalf("source = %q, want it to name where the stored credential came from", c.source)
	}
}

// TestCredentialNeverAppearsInOperatorProse guards the hygiene rule at the one place a value is
// held in memory: the human-facing `source` string must describe the credential, never contain it.
func TestCredentialNeverAppearsInOperatorProse(t *testing.T) {
	const secret = "sk-proj-DEADBEEFdeadbeef"
	c, err := resolveProvider(env(credentialEnv, secret), false)
	if err != nil {
		t.Fatalf("a valid credential was refused: %v", err)
	}
	if c.credential != secret {
		t.Fatalf("the credential must be carried verbatim to the file-secret writer")
	}
	if strings.Contains(c.source, secret) {
		t.Fatal("the operator-facing source line leaked the credential")
	}
}

// TestCapabilityRowsComeFromTheServedBody: the table's rows are whatever the running stack served —
// a capability this binary has never heard of still gets a row, and a dormant one carries the tier
// the deployment itself reported as its reason.
func TestCapabilityRowsComeFromTheServedBody(t *testing.T) {
	served := map[string]string{
		"responses":  "preview",
		"workspaces": "unavailable",
		"slack":      "preview",
		"invented":   "stable",
	}
	rows := capabilityRows(served, map[string]string{"responses": "real round-trip proven: gpt-4o-mini, 47 in / 4 out"})
	if len(rows) != len(served) {
		t.Fatalf("got %d rows for %d served capabilities", len(rows), len(served))
	}
	byName := map[string]capRow{}
	for _, r := range rows {
		byName[r.name] = r
	}
	if got := byName["invented"]; got.state != "dormant" {
		t.Fatalf("an unknown capability must still get a row: %+v", got)
	}
	if got := byName["responses"]; got.state != "live" || !strings.Contains(got.reason, "47 in") {
		t.Fatalf("responses row = %+v, want live with the proof as its reason", got)
	}
	if got := byName["workspaces"]; got.state != "dormant" || !strings.Contains(got.reason, "unavailable") {
		t.Fatalf("workspaces row = %+v, want dormant with the served tier as its reason", got)
	}
	if got := byName["slack"]; got.state != "dormant" || !strings.Contains(got.reason, "exercised nothing") {
		t.Fatalf("slack row = %+v, want dormant naming that this run exercised nothing on it", got)
	}
}

// TestAProvenRoundTripDoesNotSuppressTheWarnings pins the seam where the report meets the conditions
// Bootstrap gathers after it. They answer DIFFERENT questions — the round trip answers "does this stack
// talk to a real model?", the warnings answer "will anything an operator does next actually happen?" —
// and a report is exactly where one quietly swallows the other.
//
// The PROVEN case is the one under test on purpose. A bring-up that printed PROVEN LIVE is where an
// operator STOPS reading, and it is also precisely when the refused Approve clicks begin.
func TestAProvenRoundTripDoesNotSuppressTheWarnings(t *testing.T) {
	rt := roundTrip{ResponseID: "resp_1", Status: "completed", Model: "claude-x", InputTokens: 1, OutputTokens: 2}
	warn := "no project approver list is configured; the HTTP approve surface is gated only by tenant-scoped key possession"
	out := captureStdout(t, func() {
		printReport(Config{BaseURL: "http://127.0.0.1:8080"}, "container control plane (docker compose)", rt,
			map[string]string{"responses": "preview"}, observedFacts(rt), "1 pool(s), 1 active runner(s), 0 pending approval",
			nil, warn)
	})
	if !strings.Contains(out, "PROVEN LIVE") || !strings.Contains(out, "live") {
		t.Fatalf("the report lost the proof — an observed round trip must read as live:\n%s", out)
	}
	if !strings.Contains(out, warn) {
		t.Fatalf("the report lost the warning: a proven round trip suppressed it, so every refused approve click "+
			"stays unexplained:\n%s", out)
	}
}

// captureStdout collects what printReport writes, which is os.Stdout directly.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	w.Close()
	return <-done
}

// TestParseEnvReadsTheShapeThisRepoUses covers the file `.env.local` actually is, plus the `export`
// and quoted forms an operator is likely to paste in.
func TestParseEnvReadsTheShapeThisRepoUses(t *testing.T) {
	got, err := parseEnv(strings.NewReader(strings.Join([]string{
		"# a comment",
		"",
		"OPENAI_API_KEY=sk-plain",
		`export ANTROPHIC_API_KEY="sk-quoted"`,
		"SLACK_TEAM_ID = T0001 ",
		"NOT_AN_ASSIGNMENT",
	}, "\n")))
	if err != nil {
		t.Fatalf("parseEnv: %v", err)
	}
	want := map[string]string{"OPENAI_API_KEY": "sk-plain", "ANTROPHIC_API_KEY": "sk-quoted", "SLACK_TEAM_ID": "T0001"}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", sortedKeys(got), sortedKeys(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestRedChecksCarryTheirDetail: naming a failing check without its detail gives the operator a
// label and no fix, which is the failure mode this command exists to stop repeating.
func TestRedChecksCarryTheirDetail(t *testing.T) {
	red := redChecks(Report{Checks: map[string]Check{
		"api":  ok("GET /v1/capabilities 200"),
		"disk": fail("data dir 4.8% free — under the 10% floor (PalaiDiskLow)"),
	}})
	if len(red) != 1 || !strings.Contains(red[0], "disk:") || !strings.Contains(red[0], "PalaiDiskLow") {
		t.Fatalf("redChecks = %v, want the failing check with its detail", red)
	}
}
