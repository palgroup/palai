package stack

import (
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

// TestSlackStepSkipsCleanlyWithoutValues: an absent Slack app is normal. The skip must name what is
// missing and where it comes from, not just say "skipped".
func TestSlackStepSkipsCleanlyWithoutValues(t *testing.T) {
	body, skip := slackRegistration(env())
	if body != nil {
		t.Fatal("a registration body was built with no SLACK_TEAM_ID")
	}
	for _, want := range []string{"SLACK_TEAM_ID", "api.slack.com/apps", "§0.1"} {
		if !strings.Contains(skip, want) {
			t.Fatalf("the skip reason must mention %q, got: %q", want, skip)
		}
	}
}

// TestATeamIdAndASigningSecretAreEnoughToRegister replaces the old
// TestSlackSkipNamesTheMissingRunTarget, and the replacement IS the E21 T2 behaviour change.
//
// That test pinned a skip: a team id alone could not be registered because the API refuses a binding
// with no run target, so `palai up` named SLACK_AGENT_REVISION_ID / SLACK_PRINCIPAL_ID and did
// nothing. The skip was accurate and the outcome was still wrong — those two are Palai-side ids that
// no Slack app page hands you, `palai up` exited GREEN having wired nothing, and an operator saw a
// successful install that could not answer a message. The bring-up resolves them itself now
// (resolveRunTarget), so the only Slack values an operator must supply are Slack's own.
func TestATeamIdAndASigningSecretAreEnoughToRegister(t *testing.T) {
	body, skip := slackRegistration(env("SLACK_TEAM_ID", "T0001", "SLACK_SIGNING_SECRET", "s3cr3t"))
	if skip != "" {
		t.Fatalf("registration skipped for want of ids the bring-up provisions itself: %q", skip)
	}
	if body["team_id"] != "T0001" {
		t.Fatalf("team_id = %v", body["team_id"])
	}
}

// TestSlackRegistrationSendsHandlesNeverCredentials: POST /v1/slack-connections refuses an inline
// credential, and more to the point the CLI must never put one on the wire. The signing secret's
// VALUE must appear nowhere in the body.
func TestSlackRegistrationSendsHandlesNeverCredentials(t *testing.T) {
	const secret = "8f14e45fceea167a5a36dedd4bea2543"
	body, skip := slackRegistration(env(
		"SLACK_TEAM_ID", "T0001", "SLACK_SIGNING_SECRET", secret,
		"SLACK_AGENT_REVISION_ID", "agr_1", "SLACK_PRINCIPAL_ID", "prn_1", "SLACK_TEST_CHANNEL", "C0001"))
	if skip != "" {
		t.Fatalf("a complete Slack environment was skipped: %s", skip)
	}
	if body["signing_secret_ref"] != "slack-signing-T0001" {
		t.Fatalf("signing_secret_ref = %v, want a handle derived from the team id", body["signing_secret_ref"])
	}
	for k, v := range body {
		if s, ok := v.(string); ok && strings.Contains(s, secret) {
			t.Fatalf("field %q carries the signing secret VALUE", k)
		}
	}
	// default_policy is no longer built here: wireSlack fills it from resolveRunTarget, against the
	// running stack. TestExplicitRunTargetAlwaysWins is where the two explicit ids are proven to survive.
	if _, present := body["default_policy"]; present {
		t.Fatalf("slackRegistration built a default_policy from the environment: %v", body["default_policy"])
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

// TestSlackTestChannelNeverBecomesAProductionScope is Finding 2, and it is a NAMING guarantee with a
// security consequence rather than a style rule. SLACK_TEST_CHANNEL belongs to the live test harness
// (tests/live/slack) — it is "the channel the bot was invited to so a test can post there". `palai up`
// used to copy it into allowed_channels, so an operator who set it to make the live tests run silently
// confined their PRODUCTION bot to their test channel, and an operator who did not set it got a bot with
// no scope while the field looked like one.
func TestSlackTestChannelNeverBecomesAProductionScope(t *testing.T) {
	// SLACK_SIGNING_SECRET is in the base because a COMPLETE environment now includes it: `palai up`
	// registers signing_secret_ref only when it can store a value under that handle, so without the
	// value the whole registration is a skip and there is no body to assert scoping on. It is fixture
	// completeness only — nothing below reads it, and both scopes stay independent of it.
	base := []string{"SLACK_TEAM_ID", "T0001", "SLACK_AGENT_REVISION_ID", "agr_1", "SLACK_PRINCIPAL_ID", "prn_1",
		"SLACK_SIGNING_SECRET", "shh"}

	body, skip := slackRegistration(env(append(base, "SLACK_TEST_CHANNEL", "C_TEST")...))
	if skip != "" {
		t.Fatalf("a complete Slack environment was skipped: %s", skip)
	}
	if got, ok := body["allowed_channels"]; ok {
		t.Fatalf("SLACK_TEST_CHANNEL became allowed_channels=%v — a variable named 'test channel' must never silently become a production security scope", got)
	}

	// Unset ⇒ NO channel restriction. That is the production default and it must be the ABSENCE of the
	// field, not an empty list that a later reader could mistake for "nothing is allowed".
	bare, _ := slackRegistration(env(base...))
	if _, ok := bare["allowed_channels"]; ok {
		t.Fatalf("with no allow-list configured the body still carried allowed_channels=%v; an unconfigured connection must be registered with no channel restriction at all", bare["allowed_channels"])
	}

	// The honestly-named variable is the one that scopes, and it takes a comma-separated LIST — one channel
	// was never the right shape for an allow-list.
	scoped, _ := slackRegistration(env(append(base, "SLACK_ALLOWED_CHANNELS", " C1, C2 ,,C3 ")...))
	got, ok := scoped["allowed_channels"].([]string)
	if !ok || len(got) != 3 || got[0] != "C1" || got[1] != "C2" || got[2] != "C3" {
		t.Fatalf("SLACK_ALLOWED_CHANNELS parsed to %#v, want [C1 C2 C3] — comma-separated, trimmed, empties dropped", scoped["allowed_channels"])
	}
}

// TestSlackApproverSetIsRegisteredAndItsAbsenceIsSaidOutLoud is Finding 3. The deny-by-default in
// ApproverAuthorized is CORRECT and stays; what was broken is that `palai up` registered no approver at
// all and said nothing, so a real operator's first Approve click was refused with no way to learn why.
func TestSlackApproverSetIsRegisteredAndItsAbsenceIsSaidOutLoud(t *testing.T) {
	// SLACK_SIGNING_SECRET is in the base because a COMPLETE environment now includes it: `palai up`
	// registers signing_secret_ref only when it can store a value under that handle, so without the
	// value the whole registration is a skip and there is no body to assert scoping on. It is fixture
	// completeness only — nothing below reads it, and both scopes stay independent of it.
	base := []string{"SLACK_TEAM_ID", "T0001", "SLACK_AGENT_REVISION_ID", "agr_1", "SLACK_PRINCIPAL_ID", "prn_1",
		"SLACK_SIGNING_SECRET", "shh"}

	with, _ := slackRegistration(env(append(base, "SLACK_APPROVER_IDS", "U1, U2")...))
	users, ok := with["allowed_users"].([]string)
	if !ok || len(users) != 2 || users[0] != "U1" || users[1] != "U2" {
		t.Fatalf("SLACK_APPROVER_IDS parsed to %#v, want [U1 U2]", with["allowed_users"])
	}
	if warn := slackApproverWarning(with); warn != "" {
		t.Fatalf("a connection WITH approvers warned anyway: %q", warn)
	}

	// The silent case: registered, and every approve/deny click will be refused.
	bare, _ := slackRegistration(env(base...))
	if _, ok := bare["allowed_users"]; ok {
		t.Fatalf("with no SLACK_APPROVER_IDS the body carried allowed_users=%v; it must send nothing rather than guess", bare["allowed_users"])
	}
	warn := slackApproverWarning(bare)
	if warn == "" {
		t.Fatal("a connection registered with NO approver produced no warning — the operator learns of it as a silently refused Approve click, which is exactly the failure this warning exists to prevent")
	}
	// It must name the consequence AND the fix; "no approvers configured" alone tells an operator nothing
	// about the click that is about to be swallowed.
	for _, want := range []string{"NO approver", "refused", "SLACK_APPROVER_IDS"} {
		if !strings.Contains(warn, want) {
			t.Fatalf("the warning must mention %q, got: %q", want, warn)
		}
	}
}
