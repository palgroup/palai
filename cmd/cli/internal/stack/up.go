package stack

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// up.go is `palai up`: ONE command that ends in a stack PROVEN to be talking to a real model
// provider — or a named failure with its fix. It composes what already exists (Init, AddProvider,
// Up, doctor's runChecks, the public API) and adds exactly one thing none of them do: it REFUSES
// to report success on a stack it could not demonstrate a live round-trip against.
//
// The trap it closes is real and was hit by hand an hour before this file existed: a stack brought
// up with a valid credential but a mis-spelled selector runs the DETERMINISTIC FAKE adapter, the
// run reports `status: completed`, and the only tell is `"model": "fake"` in the response body.
// Everything below exists because a silent downgrade to fake is indistinguishable from success at
// every layer except the one this command asserts.

// liveSelector is the ONLY PALAI_MODEL_PROVIDER value main.modelBrokerFromEnv treats as live
// (apps/control-plane/cmd/palai-control-plane/main.go). EVERY other value — "openai", "anthropic",
// a typo, or unset — falls through to the fake adapter with no warning, so `palai up` refuses
// anything else instead of inheriting that fallback.
const liveSelector = "provider-one"

// fakeModel is the model id main.go scripts the fake adapter with. It is the needle the live proof
// looks for: a terminal projection carrying it means the run never left the process.
const fakeModel = "fake"

// credentialEnv is the .env.local variable the provider-one (OpenAI) adapter's credential comes
// from. NOTE what it is NOT: the control-plane never reads it — the value is written to the 0600
// $PALAI_HOME/secrets/provider-one file secret, which deploy/compose/control-plane-entrypoint.sh
// bridges into the container as PALAI_SECRET_PROVIDER_ONE at start. Setting OPENAI_API_KEY in the
// environment of a running stack does nothing at all, which is the second half of the same trap.
const credentialEnv = "OPENAI_API_KEY"

// Bootstrap is `palai up`. Steps 1-3 run BEFORE any Docker work so a refusal costs nothing.
func Bootstrap(envFile string) error {
	fmt.Fprintln(os.Stderr, "palai up — bring the stack up and PROVE it is live")

	// [1/6] The dotenv file. Only key NAMES are ever printed, and only the PALAI_* knobs below are
	// exported to the child `docker compose` environment — a credential read here reaches exactly
	// one destination: the 0600 file secret AddProvider writes.
	fileEnv, found, err := loadEnvFile(envFile)
	if err != nil {
		return err
	}
	if found {
		fmt.Fprintf(os.Stderr, "[1/6] env       %s: %d values (%s)\n", envFile, len(fileEnv), strings.Join(sortedKeys(fileEnv), ", "))
	} else {
		fmt.Fprintf(os.Stderr, "[1/6] env       %s absent — running on the process environment alone\n", envFile)
	}
	get := lookup(fileEnv)

	// [2/6] The provider decision comes BEFORE `init`, so a refusal leaves no .palai behind and
	// costs nothing. secretSlotFilled is honest either way: an absent .palai has no slot, and
	// `init` would only ever create an EMPTY one.
	p, err := resolvePaths()
	if err != nil {
		return err
	}
	choice, err := resolveProvider(get, secretSlotFilled(p))
	// Warnings print on BOTH paths: a mis-spelled variable name is most worth saying out loud in
	// the message that is about to tell the operator they have no usable credential.
	for _, w := range choice.warnings {
		fmt.Fprintf(os.Stderr, "        WARNING %s\n", w)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[2/6] provider  selector %s — %s\n", liveSelector, choice.source)
	if err := Init(); err != nil {
		return err
	}
	if choice.credential != "" {
		if err := addProvider(liveSelector, strings.NewReader(choice.credential)); err != nil {
			return err
		}
	}

	// [3/6] The compose interpolation contract. PALAI_MODEL_PROVIDER is what selects the live
	// adapter; PALAI_DISPATCH_WORKERS defaults to 0 (queued-only) in compose.yaml, which would
	// leave every run parked in `queued` and the proof unprovable.
	if err := os.Setenv("PALAI_MODEL_PROVIDER", liveSelector); err != nil {
		return err
	}
	if m := get("PALAI_MODEL"); m != "" {
		if err := os.Setenv("PALAI_MODEL", m); err != nil {
			return err
		}
	}
	switch w := strings.TrimSpace(get("PALAI_DISPATCH_WORKERS")); w {
	case "":
		if err := os.Setenv("PALAI_DISPATCH_WORKERS", "1"); err != nil {
			return err
		}
	case "0":
		return errors.New("PALAI_DISPATCH_WORKERS=0 selects a queued-only stack: nothing executes, so no round-trip can be proven — unset it or set it to 1")
	default:
		if err := os.Setenv("PALAI_DISPATCH_WORKERS", w); err != nil {
			return err
		}
	}
	fmt.Fprintln(os.Stderr, "[3/6] stack     docker compose up (this builds on a first run)")
	if err := Up(); err != nil {
		return err
	}

	cfg, p, err := loadConfig()
	if err != nil {
		return err
	}
	key, err := readTrimmed(p.apiKey)
	if err != nil {
		return fmt.Errorf("read api key: %w", err)
	}
	api := &apiClient{baseURL: cfg.BaseURL, key: key, http: &http.Client{Timeout: 30 * time.Second}}

	// [4/6] Health, from doctor's OWN checks — no second set of thresholds. A still-red check is
	// reported and CARRIED, not swallowed and not fatal: the live proof below is the real verdict,
	// and a red check that actually blocks the stack will surface there with its detail on screen.
	report := waitHealthy(cfg, p, 90*time.Second)
	red := redChecks(report)
	if len(red) == 0 {
		fmt.Fprintf(os.Stderr, "[4/6] health    doctor: %d/%d green\n", len(report.Checks), len(report.Checks))
	} else {
		fmt.Fprintf(os.Stderr, "[4/6] health    doctor: %d/%d green — NOT green: %s\n",
			len(report.Checks)-len(red), len(report.Checks), strings.Join(red, "; "))
	}

	// [5/6] The point of the whole command.
	fmt.Fprintln(os.Stderr, "[5/6] proof     one real single-step run...")
	rt, err := roundTripProof(api)
	if err != nil {
		if len(red) > 0 {
			return fmt.Errorf("%w\n        doctor checks still red, one of them may be the cause: %s", err, strings.Join(red, "; "))
		}
		return err
	}

	// [6/6] Slack, if and only if the workspace values are present. Never fatal.
	slackFact, slackLine, slackWarn := registerSlack(api, get)
	fmt.Fprintf(os.Stderr, "[6/6] slack     %s\n", slackLine)

	caps, err := api.capabilities()
	if err != nil {
		return fmt.Errorf("read /v1/capabilities for the status table: %w", err)
	}
	printReport(cfg, rt, caps, observedFacts(rt, slackFact), red, slackWarn)
	return nil
}

// --- provider selection -------------------------------------------------------------------

// providerChoice is the resolved live provider decision. source is operator-facing prose; it names
// where the credential came from and NEVER carries a value.
type providerChoice struct {
	credential string
	source     string
	warnings   []string
}

// resolveProvider decides whether this bring-up can run live, and refuses rather than falling back.
// The three refusals are the three ways an operator silently ends up on the fake adapter:
//
//  1. a selector that is not `provider-one` (main.modelBrokerFromEnv reads ANY other value as fake),
//  2. no credential anywhere, and
//  3. only an Anthropic credential — which the compose deployment default cannot use at all.
//
// storedSecret reports whether $PALAI_HOME/secrets/provider-one is already non-empty, so an
// operator who ran `palai provider add` once is not asked for the value again.
func resolveProvider(get func(string) string, storedSecret bool) (providerChoice, error) {
	var c providerChoice
	switch sel := strings.TrimSpace(get("PALAI_MODEL_PROVIDER")); sel {
	case "", liveSelector:
	case fakeModel:
		return c, fmt.Errorf("PALAI_MODEL_PROVIDER=%q selects the deterministic fake adapter, and `palai up` exists to prove a LIVE round-trip — "+
			"fix: unset it (or set it to %q) for a live stack, or run `palai local up` for the fake one", sel, liveSelector)
	default:
		return c, fmt.Errorf("PALAI_MODEL_PROVIDER=%q is not a recognised selector: the control-plane reads every value other than %q as the "+
			"deterministic FAKE adapter and says nothing (main.modelBrokerFromEnv) — fix: set PALAI_MODEL_PROVIDER=%s", sel, liveSelector, liveSelector)
	}

	// The mis-spelling is this repo's own: .env.local and the whole tree spell it ANTROPHIC_API_KEY
	// (`grep -r ANTROPHIC`). Both spellings are read HERE — and only here, to make the refusal below
	// accurate — but nothing is renamed in the tree: the mis-spelling is consistent, so a rename is a
	// cross-cutting change with no bearing on whether this command can prove a live round-trip.
	anthropic := firstNonEmpty(get("ANTHROPIC_API_KEY"), get("ANTROPHIC_API_KEY"))
	if get("ANTROPHIC_API_KEY") != "" && get("ANTHROPIC_API_KEY") == "" {
		c.warnings = append(c.warnings, "ANTROPHIC_API_KEY is spelled the way this repo spells it, but it is a mis-spelling of ANTHROPIC_API_KEY (accepted here, nothing else in the tree reads either)")
	}

	if cred := strings.TrimSpace(get(credentialEnv)); cred != "" {
		c.credential = cred
		c.source = fmt.Sprintf("credential from %s, written to the 0600 file secret (never argv, env or log)", credentialEnv)
		return c, nil
	}
	if storedSecret {
		c.source = "credential already stored by a previous `palai provider add provider-one` (0600 file secret, not re-read here)"
		return c, nil
	}
	if anthropic != "" {
		return c, fmt.Errorf("an Anthropic credential is set but the compose deployment default cannot use it: the entrypoint bridges only "+
			"provider_one_key and modelBrokerFromEnv's env route is %s (OpenAI). The Anthropic adapter (provider-two) is reachable only through a "+
			"published per-project model route — fix: set %s in .env.local for this bring-up", liveSelector, credentialEnv)
	}
	return c, fmt.Errorf("no provider credential: %s is unset and $PALAI_HOME/secrets/provider-one is empty, so the stack would come up on the "+
		"deterministic FAKE adapter — fix: put %s=<key> in .env.local, or pipe it once with "+
		"`printf %%s \"$%s\" | palai provider add provider-one`", credentialEnv, credentialEnv, credentialEnv)
}

// secretSlotFilled reports whether the provider-one file secret already holds a value. `palai init`
// creates the slot EMPTY (compose bind-mounts it and the mount fails on a missing source), so
// existence proves nothing and only a non-zero size does.
func secretSlotFilled(p paths) bool {
	info, err := os.Stat(p.secretPath(liveSelector))
	return err == nil && info.Size() > 0
}

// --- the live proof -----------------------------------------------------------------------

// roundTrip is the evidence one real single-step run leaves behind.
type roundTrip struct {
	ResponseID   string
	Status       string
	Model        string
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

// roundTripProof issues one trivial run and returns it only if proveLive accepts it.
func roundTripProof(api *apiClient) (roundTrip, error) {
	id, err := api.createResponse("Reply with the single word: ok")
	if err != nil {
		return roundTrip{}, err
	}
	rt, err := api.awaitTerminal(id, 180*time.Second)
	if err != nil {
		return roundTrip{}, err
	}
	return rt, proveLive(rt)
}

// proveLive is the crown assertion: it accepts a terminal response ONLY as evidence that a real
// provider served it. All four conditions are load-bearing, because each one alone is satisfiable
// by a stack that never left the process:
//
//   - a fake run also reaches `completed`, so status alone proves nothing;
//   - an empty model would sail past a bare `model != "fake"` check (the vacuous-assertion trap);
//   - `fake` is the model id main.go scripts the fake adapter with; and
//   - the fake script declares NO usage, so a zero token count is the second tell — and no real
//     provider bills a completed call zero in AND zero out.
func proveLive(rt roundTrip) error {
	switch {
	case rt.Status != "completed":
		return fmt.Errorf("NOT PROVEN: response %s ended %q, not completed — the stack is up but it did not complete a run. "+
			"Fix: `palai local doctor` for the failing surface, then `docker compose logs control-plane`", rt.ResponseID, rt.Status)
	case rt.Model == "":
		return fmt.Errorf("NOT PROVEN: response %s completed but its terminal projection carries NO model, so nothing here says which provider "+
			"served it. Fix: this is a control-plane finalize bug, not a configuration one — report it with the response id", rt.ResponseID)
	case rt.Model == fakeModel:
		return fmt.Errorf("NOT PROVEN — THE STACK IS FAKE: response %s completed on model %q, the deterministic in-process adapter. No provider "+
			"was called and no token was spent. Fix: PALAI_MODEL_PROVIDER must be exactly %q when the control-plane starts (any other value selects "+
			"fake in silence), and the credential must be in $PALAI_HOME/secrets/provider-one BEFORE `local up` — a running container never re-reads it",
			rt.ResponseID, rt.Model, liveSelector)
	case rt.InputTokens == 0 && rt.OutputTokens == 0:
		return fmt.Errorf("NOT PROVEN: response %s completed on model %q but reported ZERO tokens in and out. A real provider call is metered; a "+
			"zero-usage completion is the fake adapter's signature under a different model id. Fix: check the control-plane logs for a model route "+
			"override on this project", rt.ResponseID, rt.Model)
	}
	return nil
}

// --- slack ---------------------------------------------------------------------------------

// registerSlack registers the workspace when the SLACK_* values are present, and otherwise says
// precisely what is missing and where it comes from. It never fails the bring-up: an absent Slack
// app is a normal state, and a registration hiccup does not un-prove the live round-trip above.
//
// It returns (fact, line, warn): fact is the observed-state string the capability table shows for
// `slack` when a workspace IS registered, line is the operator-facing step result, and warn is a
// non-fatal condition the final report must repeat because the operator would otherwise only meet
// it as silence (see slackApproverWarning).
func registerSlack(api *apiClient, get func(string) string) (fact string, line string, warn string) {
	body, skip := slackRegistration(get)
	if skip != "" {
		// Even when skipping, report what the stack already holds — a workspace registered by an
		// earlier run is live regardless of what .env.local says today.
		if n, err := api.slackConnectionCount(); err == nil && n > 0 {
			return fmt.Sprintf("%d workspace(s) registered", n), fmt.Sprintf("SKIPPED — %s (%d workspace(s) already registered on this stack)", skip, n), ""
		}
		return "", "SKIPPED — " + skip, ""
	}
	id, status, err := api.createSlackConnection(body)
	switch {
	case err != nil:
		return "", "NOT registered: " + err.Error(), ""
	case status == http.StatusConflict:
		// Nothing changed, and the STORED connection's approver list is not this body's — claiming
		// anything about it would be the same kind of guess the warning exists to replace.
		return "workspace already bound", fmt.Sprintf("team %s was already bound (409) — nothing changed", body["team_id"]), ""
	}
	ref, _ := body["signing_secret_ref"].(string)
	return fmt.Sprintf("workspace %s registered", body["team_id"]), fmt.Sprintf(
		"registered %s for team %s. NOT PROVEN: the signing-secret handle %q resolves nowhere on this stack, so no Slack "+
			"signature has been verified — the local compose profile mounts neither PALAI_SLACK_SECRET_FILE_<ORG>__<REF> nor the "+
			"secret-ref API (which needs a production master key)", id, body["team_id"], ref), slackApproverWarning(body)
}

// slackRegistration builds the POST /v1/slack-connections body from the .env.local values, or
// returns the reason the step is skipped. It NEVER sends a credential: the endpoint refuses inline
// values and accepts only *_ref handles, so SLACK_SIGNING_SECRET's value is not read here at all —
// only the handle NAME derived from the team id is.
func slackRegistration(get func(string) string) (map[string]any, string) {
	team := strings.TrimSpace(get("SLACK_TEAM_ID"))
	if team == "" {
		return nil, "no SLACK_TEAM_ID in the environment. A Slack workspace needs an app at https://api.slack.com/apps; " +
			"the values and where each is found are in docs/superpowers/plans/phase-19-integration-wiring.md §0.1"
	}
	// The API requires a run target: a binding that has not been told what to run, or as whom,
	// admits nothing. These two are not in plan §0.1 because they are Palai-side ids, not Slack's.
	revision := strings.TrimSpace(get("SLACK_AGENT_REVISION_ID"))
	principal := strings.TrimSpace(get("SLACK_PRINCIPAL_ID"))
	var missing []string
	if revision == "" {
		missing = append(missing, "SLACK_AGENT_REVISION_ID")
	}
	if principal == "" {
		missing = append(missing, "SLACK_PRINCIPAL_ID")
	}
	if len(missing) > 0 {
		return nil, fmt.Sprintf("SLACK_TEAM_ID is set but %s missing. POST /v1/slack-connections refuses a binding with no run target "+
			"(default_policy.agent_revision_id and default_policy.principal_id are required — what to run, and as whom)", strings.Join(missing, " and "))
	}
	body := map[string]any{
		"team_id": team,
		// A HANDLE, never the secret. Derived from the team id so a re-run names the same handle.
		"signing_secret_ref": "slack-signing-" + team,
		"default_policy":     map[string]any{"agent_revision_id": revision, "principal_id": principal},
	}
	if v := strings.TrimSpace(get("SLACK_BOT_USER_ID")); v != "" {
		body["bot_user_id"] = v
	}
	// The two SCOPES, and both are absent-by-default on purpose.
	//
	// SLACK_ALLOWED_CHANNELS, not SLACK_TEST_CHANNEL. The latter belongs to the live test harness
	// (tests/live/slack) and means "a channel the bot was invited to so a test can post there"; it used
	// to be written here as allowed_channels, which turned a variable an operator set to make the tests
	// run into a production security scope confining their bot to their test channel. Unset ⇒ the field
	// is not sent at all ⇒ NO channel restriction, which is the production default.
	if v := splitList(get("SLACK_ALLOWED_CHANNELS")); len(v) > 0 {
		body["allowed_channels"] = v
	}
	// allowed_users is deny-by-default server-side, so an unset value really does mean nobody can approve.
	// That is the correct posture and it is not softened here — it is SAID OUT LOUD instead, by
	// slackApproverWarning, in the final report.
	if v := splitList(get("SLACK_APPROVER_IDS")); len(v) > 0 {
		body["allowed_users"] = v
	}
	return body, ""
}

// splitList parses a comma-separated operator value: trimmed, empties dropped. nil when nothing is left,
// so a caller can distinguish "not configured" from "configured empty".
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// slackApproverWarning is the line an operator would otherwise only learn from a silently refused Approve
// click. ApproverAuthorized is deny-by-default — correctly: a connection that has not been told who may
// approve must not let any member of the workspace authorize a privileged operation. But a `palai up` that
// registers no approver and says nothing leaves a real operator mentioning the bot, clicking Approve, and
// getting NOTHING back, with no surface anywhere that explains it.
//
// Derived from the body actually sent, never from an assumption about what `palai up` usually does.
func slackApproverWarning(body map[string]any) string {
	if users, ok := body["allowed_users"].([]string); ok && len(users) > 0 {
		return ""
	}
	return "slack: registered, but NO approver is allow-listed, so every approve/deny click will be refused; " +
		"set SLACK_APPROVER_IDS=U… (comma-separated Slack user ids) and re-run"
}

// --- the final report ------------------------------------------------------------------------

// capRow is one line of the capability table.
type capRow struct{ name, state, reason string }

// observedFacts collects what THIS bring-up actually observed, keyed by capability name. Only a
// capability with a fact here is called live; everything else is dormant with the tier the
// deployment served as its reason.
func observedFacts(rt roundTrip, slackFact string) map[string]string {
	facts := map[string]string{
		"responses": fmt.Sprintf("real round-trip proven: %s, %d in / %d out", rt.Model, rt.InputTokens, rt.OutputTokens),
	}
	if slackFact != "" {
		facts["slack"] = slackFact
	}
	return facts
}

// capabilityRows derives the status table from the RUNNING stack: the row set is exactly what
// GET /v1/capabilities served, so a capability this binary has never heard of still gets a row and
// one this deployment does not mount gets none.
//
// ponytail: a dormant row's reason names the TIER THE DEPLOYMENT SERVED, not the server-side knob
// that produced it (`workspaces` is "unavailable" because PALAI_WORKSPACE_ROOT is unset — see
// api.workspacesCapability). Copying that name→knob mapping into the CLI would be the hardcoded
// list this table is supposed to avoid, and it is exactly the kind of copy that goes stale when the
// server changes. Upgrade path: have /v1/capabilities serve the reason alongside the tier.
func capabilityRows(caps map[string]string, facts map[string]string) []capRow {
	rows := make([]capRow, 0, len(caps))
	for _, name := range sortedKeys(caps) {
		tier := caps[name]
		if fact, ok := facts[name]; ok && fact != "" {
			rows = append(rows, capRow{name, "live", fmt.Sprintf("%s — %s", tier, fact)})
			continue
		}
		reason := fmt.Sprintf("%s — this bring-up exercised nothing on it", tier)
		if tier == "unavailable" || tier == "disabled" {
			reason = fmt.Sprintf("%s — the deployment itself reports it does not serve this", tier)
		}
		rows = append(rows, capRow{name, "dormant", reason})
	}
	return rows
}

// printReport writes the operator-facing result to stdout. Every line states what was PROVEN, not
// what was attempted — "stack up" is not a proof.
func printReport(cfg Config, rt roundTrip, caps map[string]string, facts map[string]string, red []string, warnings ...string) {
	out := os.Stdout
	fmt.Fprintln(out, "\nPROVEN LIVE")
	fmt.Fprintf(out, "  round-trip   %s -> %s\n", rt.ResponseID, rt.Status)
	fmt.Fprintf(out, "  model        %s   (selector %s — NOT the fake adapter)\n", rt.Model, liveSelector)
	fmt.Fprintf(out, "  usage        %d in / %d out / %d total tokens\n", rt.InputTokens, rt.OutputTokens, rt.TotalTokens)
	fmt.Fprintf(out, "  api          %s\n", cfg.BaseURL)

	fmt.Fprintln(out, "\nCAPABILITIES (from GET /v1/capabilities on this running stack)")
	for _, r := range capabilityRows(caps, facts) {
		fmt.Fprintf(out, "  %-19s %-8s %s\n", r.name, r.state, r.reason)
	}
	if len(red) > 0 {
		fmt.Fprintf(out, "\nNOT green (doctor, not fatal — the round-trip above still ran): %s\n", strings.Join(red, "; "))
	}
	// Registered-but-unusable conditions. They are not failures — the thing was created — but they are
	// exactly the states an operator discovers as unexplained silence, so they are said here in full.
	for _, w := range warnings {
		if w != "" {
			fmt.Fprintf(out, "\nWARNING %s\n", w)
		}
	}
	fmt.Fprintln(out, "\n  palai local doctor      the full health surface")
	fmt.Fprintln(out, "  palai local down        stop the stack, keeping its data")
}

// --- plumbing ------------------------------------------------------------------------------

// apiClient speaks the public API with the bootstrap key. It is the only HTTP in this file; the
// four calls below (create, retrieve, capabilities, slack) all route through it.
type apiClient struct {
	baseURL string
	key     string
	http    *http.Client
}

func (c *apiClient) do(method, path string, body any, out any) (int, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		payload = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, c.baseURL+path, payload)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// POST /v1/responses is idempotency-gated: without this header it answers 400, which reads to a
	// newcomer as "the stack is broken".
	if method == http.MethodPost {
		req.Header.Set("Idempotency-Key", "palai-up-"+randomHex(12))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: decode %d body: %w", method, path, resp.StatusCode, err)
		}
	}
	if resp.StatusCode >= 500 {
		return resp.StatusCode, fmt.Errorf("%s %s = %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return resp.StatusCode, nil
}

func (c *apiClient) createResponse(input string) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	status, err := c.do(http.MethodPost, "/v1/responses", map[string]any{"input": input}, &created)
	if err != nil {
		return "", err
	}
	if status != http.StatusAccepted {
		return "", fmt.Errorf("POST /v1/responses = %d, want 202 (a 400 here is usually the missing Idempotency-Key header)", status)
	}
	return created.ID, nil
}

// awaitTerminal polls the response until it is terminal. A run that never leaves `queued` means the
// stack is dispatch-off, which the message names rather than leaving as a bare timeout.
func (c *apiClient) awaitTerminal(id string, within time.Duration) (roundTrip, error) {
	deadline := time.Now().Add(within)
	var last struct {
		Status string `json:"status"`
		Model  string `json:"model"`
		Usage  struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	for {
		// A non-200 must not be polled over: an expired key or a purged response would otherwise
		// read as "still queued" until the deadline and blame dispatch for an auth fault.
		switch status, err := c.do(http.MethodGet, "/v1/responses/"+id, nil, &last); {
		case err != nil:
			return roundTrip{}, err
		case status != http.StatusOK:
			return roundTrip{}, fmt.Errorf("GET /v1/responses/%s = %d, want 200", id, status)
		}
		switch last.Status {
		case "completed", "failed", "canceled":
			return roundTrip{
				ResponseID: id, Status: last.Status, Model: last.Model,
				InputTokens: last.Usage.InputTokens, OutputTokens: last.Usage.OutputTokens, TotalTokens: last.Usage.TotalTokens,
			}, nil
		}
		if time.Now().After(deadline) {
			return roundTrip{}, fmt.Errorf("NOT PROVEN: response %s was still %q after %s. A run stuck in `queued` means the control-plane is "+
				"dispatch-off — fix: PALAI_DISPATCH_WORKERS must be >= 1 when the container starts", id, last.Status, within)
		}
		time.Sleep(time.Second)
	}
}

func (c *apiClient) capabilities() (map[string]string, error) {
	var body struct {
		Capabilities map[string]string `json:"capabilities"`
	}
	status, err := c.do(http.MethodGet, "/v1/capabilities", nil, &body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/capabilities = %d, want 200", status)
	}
	return body.Capabilities, nil
}

func (c *apiClient) slackConnectionCount() (int, error) {
	var body struct {
		Data []json.RawMessage `json:"data"`
	}
	status, err := c.do(http.MethodGet, "/v1/slack-connections", nil, &body)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("GET /v1/slack-connections = %d", status)
	}
	return len(body.Data), nil
}

func (c *apiClient) createSlackConnection(body map[string]any) (string, int, error) {
	var created struct {
		ID     string `json:"id"`
		Detail string `json:"detail"`
	}
	status, err := c.do(http.MethodPost, "/v1/slack-connections", body, &created)
	if err != nil {
		return "", status, err
	}
	switch status {
	case http.StatusCreated, http.StatusConflict:
		return created.ID, status, nil
	default:
		return "", status, fmt.Errorf("POST /v1/slack-connections = %d: %s", status, created.Detail)
	}
}

// waitHealthy re-runs DOCTOR'S OWN checks until they are all green or they stop improving. It
// invents no check and no threshold — runChecks is the same function `palai local doctor` calls.
//
// It gives up early when a round changes nothing, because some checks are never going to go green
// by waiting: the disk check fails on a host under 10% free, and blocking the live proof behind a
// full timeout for a condition that is not about readiness would tax every bring-up on such a host.
func waitHealthy(cfg Config, p paths, within time.Duration) Report {
	deadline := time.Now().Add(within)
	previous, stable := "", 0
	for {
		report := runChecks(cfg, p)
		red := strings.Join(redChecks(report), "|")
		if red == previous {
			stable++
		} else {
			previous, stable = red, 0
		}
		// Three identical rounds (~6s): long enough for a runner still finishing its compose-mTLS
		// enrolment to turn green, short enough that a permanently-red check costs seconds.
		if report.OK || stable >= 3 || time.Now().After(deadline) {
			return report
		}
		time.Sleep(2 * time.Second)
	}
}

// redChecks names every check that is not green, with its detail — a name alone is not a fix.
func redChecks(r Report) []string {
	var out []string
	for _, name := range sortedKeys(r.Checks) {
		if c := r.Checks[name]; c.Status != "ok" {
			out = append(out, fmt.Sprintf("%s: %s", name, c.Detail))
		}
	}
	return out
}

// loadEnvFile reads a dotenv-style file. A missing file is NOT an error — the command then runs on
// the process environment alone, which is what `set -a; . ./.env.local` already gives it.
func loadEnvFile(path string) (map[string]string, bool, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]string{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	env, err := parseEnv(f)
	return env, true, err
}

// parseEnv handles the shape `.env.local` actually has: KEY=VALUE, optional `export`, # comments,
// optional surrounding quotes.
//
// ponytail: no interpolation, no multi-line values, no escape sequences. The documented way to load
// this file is `set -a; . ./.env.local`, and every value it holds is a flat opaque credential.
func parseEnv(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) > 1 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if k != "" {
			out[k] = v
		}
	}
	return out, sc.Err()
}

// lookup resolves a key from the process environment FIRST, then the dotenv file — an explicitly
// exported value always beats a file the operator may have forgotten about.
func lookup(fileEnv map[string]string) func(string) string {
	return func(k string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return fileEnv[k]
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// sortedKeys keeps every rendered map (env key names, doctor checks, capabilities) in a stable order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
