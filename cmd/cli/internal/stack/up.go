package stack

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
//
// native selects the E22 T5 posture: the control plane runs as a PROCESS ON THIS MACHINE and only
// Postgres, the object store and the runner stay in Docker (native.go). Everything after the
// bring-up is identical — the same doctor checks, the same live round-trip, the same report — which
// is the point: a Mac deployment is a deployment, not a second product.
func Bootstrap(envFile string, native bool) error {
	fmt.Fprintln(os.Stderr, "palai up — bring the stack up and PROVE it is live")

	// [1/5] The dotenv file. Only key NAMES are ever printed, and only the PALAI_* knobs below are
	// exported to the child `docker compose` environment — a credential read here reaches exactly
	// one destination: the 0600 file secret AddProvider writes.
	fileEnv, found, err := loadEnvFile(envFile)
	if err != nil {
		return err
	}
	if found {
		fmt.Fprintf(os.Stderr, "[1/5] env       %s: %d values (%s)\n", envFile, len(fileEnv), strings.Join(sortedKeys(fileEnv), ", "))
	} else {
		fmt.Fprintf(os.Stderr, "[1/5] env       %s absent — running on the process environment alone\n", envFile)
	}
	get := lookup(fileEnv)

	// [2/5] The provider decision comes BEFORE `init`, so a refusal leaves no .palai behind and
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
	fmt.Fprintf(os.Stderr, "[2/5] provider  selector %s — %s\n", liveSelector, choice.source)
	if err := Init(); err != nil {
		return err
	}
	if choice.credential != "" {
		if err := addProvider(liveSelector, strings.NewReader(choice.credential)); err != nil {
			return err
		}
	}

	// [3/5] The compose interpolation contract. PALAI_MODEL_PROVIDER is what selects the live
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
	if err := applyConcurrencyEnv(get); err != nil {
		return err
	}
	// THE MACHINE'S OWN DESIRED CONFIGURATION, read off the control plane that is already running and
	// exported BEFORE compose is driven, so the values are in the environment compose interpolates from
	// (see desired.go for the sequencing and for why a failure here is silent). This is the reader that
	// makes the panel's write mean something: without it the desired document is a table nobody consults.
	//
	// It is applied AFTER the three blocks above and overwrites them where they overlap, which matters for
	// exactly one setting: applyConcurrencyEnv defaults PALAI_DISPATCH_WORKERS to 1 so the live round-trip
	// below can be proven, and a machine whose operator saved a different number should get theirs. The one
	// value this command still owns outright is PALAI_MODEL_PROVIDER — `palai up` REFUSES to run on the
	// fake adapter, so it is re-asserted after the document is applied rather than before it.
	desired := readDesiredFromRunningStack()
	applied, err := applyDesiredEnv(desired)
	if err != nil {
		return err
	}
	if len(applied) > 0 {
		fmt.Fprintf(os.Stderr, "        desired    revision %d from this machine: %s\n", desired.revision, strings.Join(applied, ", "))
		if err := os.Setenv("PALAI_MODEL_PROVIDER", liveSelector); err != nil {
			return err
		}
	}

	// The publication destination's CREDENTIAL must be exported before compose creates the container: a
	// running control-plane never re-reads its environment. An approved push publishes through a GitHub App
	// the control-plane resolves at boot, so a stack brought up without one has an approval chain that ends
	// in silence (see applyGitHubAppEnv). Failures here are reported and carried — a publication path must
	// not fail a bring-up whose model round-trip is about to be proven.
	for _, w := range applyGitHubAppEnv(p, get) {
		fmt.Fprintf(os.Stderr, "        WARNING %s\n", w)
	}
	posture := "container control plane (docker compose)"
	if native {
		fmt.Fprintln(os.Stderr, "[3/5] stack     NATIVE: postgres + object-store + runner in docker, control plane on this machine")
		if posture, err = UpNative(get); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(os.Stderr, "[3/5] stack     docker compose up (this builds on a first run)")
		if err := Up(); err != nil {
			return err
		}
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

	// [4/5] Health, from doctor's OWN checks — no second set of thresholds. A still-red check is
	// reported and CARRIED, not swallowed and not fatal: the live proof below is the real verdict,
	// and a red check that actually blocks the stack will surface there with its detail on screen.
	report := waitHealthy(cfg, p, 90*time.Second)
	red := redChecks(report)
	if len(red) == 0 {
		fmt.Fprintf(os.Stderr, "[4/5] health    doctor: %d/%d green\n", len(report.Checks), len(report.Checks))
	} else {
		fmt.Fprintf(os.Stderr, "[4/5] health    doctor: %d/%d green — NOT green: %s\n",
			len(report.Checks)-len(red), len(report.Checks), strings.Join(red, "; "))
	}

	// THE DESIRED CONFIGURATION, VERIFIED against the machine's own report of what it is running.
	//
	// This is where the claim "the panel writes it and the bring-up applies it" becomes a transcript rather
	// than a design note. It reads GET /v1/deployment back, repairs a first-install drift with one
	// control-plane recreate, and REFUSES the command if the machine is still not running what was saved —
	// the same discipline the live round trip below applies to the model provider. A bring-up that reported
	// success on a stack running something other than the operator's document would be the "declared, and
	// nothing happens" defect wearing this command's own report.
	// THE REPAIR MUST RESTART THE CONTROL PLANE THIS DEPLOYMENT ACTUALLY RUNS. recreateControlPlane
	// recreates the COMPOSE SERVICE, and on a native bring-up that service is not the control plane —
	// it is in a profile and deliberately not started, while the real one is a process on this machine.
	//
	// Measured 2026-08-02: with a drifted desired document, `palai up --native` brought the native
	// control plane up cleanly (pid printed, doctor 14/15) and then failed at [4/6] with
	//
	//   Error response from daemon: Ports are not available: exposing port TCP 127.0.0.1:60351
	//
	// because the repair asked compose to start a container that wanted the port the native process had
	// just taken. The bring-up it had already completed was reported as a failure, and on the run before
	// it — where the container was still up from an earlier `local up` — the collision went the other
	// way: the NATIVE process died on bind, the container kept serving, and the command printed PROVEN
	// LIVE for a round trip against the very container it was replacing.
	//
	// A native deployment therefore repairs by restarting its own process. Refusing instead would be
	// worse: a drifted document is exactly when an operator needs the bring-up to converge.
	repair := func() error { return restartControlPlane(cfg, p, get, native) }
	desiredLine, err := verifyDesiredApplied(api, repair)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "        config    %s\n", desiredLine)

	// [5/5] The point of the whole command.
	fmt.Fprintln(os.Stderr, "[5/5] proof     one real single-step run...")
	rt, err := roundTripProof(api)
	if err != nil {
		if len(red) > 0 {
			return fmt.Errorf("%w\n        doctor checks still red, one of them may be the cause: %s", err, strings.Join(red, "; "))
		}
		return err
	}

	caps, err := api.capabilities()
	if err != nil {
		return fmt.Errorf("read /v1/capabilities for the status table: %w", err)
	}

	// WHAT THE STACK IS READY TO DO, said out loud. Every line below is unconditional — each one names a
	// state whose only other symptom is silence, and none of them belongs to any one integration.
	var warns []string
	// WHICH REPOSITORY A SESSION CAN CODE AGAINST (E22 T3). The binding is created from the two values
	// §0.2 asks the operator for; a session, an agent or a bot then references it by id.
	repo := resolveRepository(api, get)
	if repo.resolved != "" {
		fmt.Fprintf(os.Stderr, "        %s\n", repo.resolved)
	}
	warns = appendWarn(warns, repo.warn)
	if native {
		// A control plane on a Mac with no declared shell posture has a nil shell runner, so the
		// whole reason for running natively — the host's own toolchain — is silently absent.
		warns = appendWarn(warns, nativeShellWarning(get))
	}
	// WHO MAY APPROVE (E23 T2): the HTTP approve surface exists on every stack.
	warns = appendWarn(warns, approverListWarning(api))
	// HOW MANY RUNS AT ONCE (§3.6 D11): a second machine that nothing can reach is the shape of a fleet
	// that was bought and is not being used.
	warns = appendWarn(warns, dispatchWorkerFleetWarning(api, get))
	// WHICH OF THE TWO MODEL-CREDENTIAL AUTHORITIES ACTUALLY SERVED THIS RUN. Placed AFTER the round trip
	// on purpose: by here the proof has happened, so the operator is being told which credential produced
	// the `PROVEN LIVE` line they are about to read.
	warns = appendWarn(warns, modelAuthorityWarning(api, get))
	// WHETHER ANY AGENT ON THIS STACK CAN CALL A TOOL AT ALL. A revision's tool list is a CEILING
	// intersected with the project's default_tools baseline, so a project whose config_policy is NULL —
	// which is every project until somebody sets one — resolves EVERY agent to the empty set. The runs
	// complete. They answer in one step. They call nothing, and no layer says why.
	warns = appendWarn(warns, api.emptyToolBaselineWarning())
	// WHAT THE FLEET IS DOING (E24 T6), on the report rather than in a warning: a machine held in a strict
	// pool's waiting room is a machine an operator will otherwise read as broken.
	printReport(cfg, posture, rt, caps, observedFacts(rt), fleetLine(api.fleet()), red, warns...)
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
	//
	// AND IT IS NOT CARRIED ANYWHERE NEW. The console's provider screen (app/registry) takes an Anthropic
	// credential under a name the OPERATOR chooses and binds it to the `provider-two` family, so the
	// mis-spelling cannot propagate: there is no environment variable on that path to spell wrongly. Both
	// spellings keep being read here, and DELIBERATELY — an operator whose .env.local carries the
	// mis-spelled name has a working file today, and silently ceasing to read it would be exactly the
	// quiet-behaviour-change this branch exists to argue against. The name is reported, never corrected in
	// place: rewriting somebody's dotenv is not this command's business.
	anthropic := firstNonEmpty(get("ANTHROPIC_API_KEY"), get("ANTROPHIC_API_KEY"))
	if get("ANTROPHIC_API_KEY") != "" && get("ANTHROPIC_API_KEY") == "" {
		c.warnings = append(c.warnings, "ANTROPHIC_API_KEY is spelled the way this repo spells it, but it is a mis-spelling of ANTHROPIC_API_KEY. "+
			"Both are accepted here and NOTHING ELSE in the tree reads either, so renaming it changes nothing and leaving it costs nothing — "+
			"the durable home for an Anthropic key is a connection on the console's /registry screen, where you name it yourself")
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
		// THE SECOND FIX IS NEW AND IT IS THE BETTER ONE. This message used to offer exactly one remedy —
		// "set OPENAI_API_KEY in .env.local" — because a per-project model route could only be published by
		// the provisioning API or `palai model route`, and this command cannot ask an operator to run a CLI
		// verb mid-bring-up. The console publishes that route now (/registry, "Point runs at a connection"),
		// so an Anthropic key has a real home and the sentence names it. The ORDER is deliberate: the file
		// remedy is second because it is the bootstrap seed, not the durable place for a credential.
		return c, fmt.Errorf("an Anthropic credential is set but the compose deployment default cannot use it: the entrypoint bridges only "+
			"provider_one_key and modelBrokerFromEnv's env route is %s (OpenAI). The Anthropic adapter (provider-two) is reachable only through a "+
			"published per-project model route — fix: bring the stack up with an OpenAI key, then seal the Anthropic one on the console's "+
			"/registry screen and publish a route with \"Point runs at a connection\" (the route overrides the deployment default, with no "+
			"restart); or, to stay on the file for now, set %s in .env.local for this bring-up", liveSelector, credentialEnv)
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

// --- the deployment's own knobs ---------------------------------------------------------------

// defaultModelRouteAlias is the ONLY route alias a run resolves through — coordinator.DefaultModelRouteAlias,
// which internal/store/model_routes.go enforces by refusing every other name. It is spelled here rather than
// imported because this package is the CLI and does not depend on the control plane's internals; the store's
// own 400 is what stops a stale spelling from writing an alias dispatch ignores.
const defaultModelRouteAlias = "default"

// containerMasterKeyPath is where compose mounts the master-key file secret. A PATH, never a value,
// so it rides the compose environment like every other knob.
const containerMasterKeyPath = "/run/secrets/master_key"

// containerGitHubAppKeyPath is where compose mounts the GitHub App private key file secret. compose.yaml
// writes it LITERALLY, the way it writes the master key's path: the value an operator puts in .env.local is
// a HOST path, and interpolating a host path into a container is how you get a control-plane looking for a
// key at a name that means something else on the other side of the mount.
const containerGitHubAppKeyPath = "/run/secrets/github_app_key"

// gitHubAppKeySlot is the file-secret source compose bind-mounts the key from.
const gitHubAppKeySlot = "github-app-key"

// applyGitHubAppEnv wires the GitHub App the approval pump publishes THROUGH (§0.2), and removes the
// silence around it.
//
// THE SILENCE IT REMOVED, and what is left of it after the publisher was un-gated. This function was
// written when main.repositoryPublisherFromEnv returned nil for a missing App variable and a nil publisher
// made pumpApprovedPublications a no-op — so an operator could grant the publish tools, watch the agent
// propose a push, press Approve, see the message repaired to "Approved: push agent/… -> …", and nothing
// would ever happen: an approved row sitting in the database forever, with a human believing they
// authorized something.
//
// The publisher is no longer gated on the App (main.repositoryPublisher), so that silence is gone: a
// binding carrying its own connection_ref publishes with no App at all, and a binding carrying none is
// REFUSED by the publication tool before a human is asked. THIS WARNING STILL FIRES AND STILL EARNS ITS
// LINE, for the branch it was always really about: the repository `palai up` binds from PALAI_GIT_CLONE_URL
// has NO connection_ref, so on a stack with no App it is exactly the binding that cannot publish. The
// operator now learns it at bring-up instead of at the refusal.
//
// WHAT RIDES WHERE. The App id and the installation id are identifiers, so they ride the compose
// environment like every other knob. The PRIVATE KEY does not: its bytes are copied into the 0600
// $PALAI_HOME/secrets slot compose mounts as a file secret, and what the control-plane reads from the
// environment is a PATH. The key value therefore appears in no `docker inspect`, no `compose config`, no
// argv and no log — the same posture the provider credential and the master key already have, and the
// reason main.go can say it "only mints short-lived scoped tokens against it".
//
// It runs BEFORE `docker compose up` because a running control-plane never re-reads its environment: a
// key staged after the fact stays unresolved until the next bring-up.
func applyGitHubAppEnv(p paths, get func(string) string) []string {
	appID := strings.TrimSpace(get("PALAI_GITHUB_APP_ID"))
	installID := strings.TrimSpace(get("PALAI_GITHUB_APP_INSTALLATION_ID"))
	keyFile := strings.TrimSpace(get("PALAI_GITHUB_APP_PRIVATE_KEY_FILE"))

	if appID == "" && installID == "" && keyFile == "" {
		// Nothing configured. Warn ONLY when this stack is about to grant the publish tools — an operator who
		// bound no repository did not ask to publish anything, and warning them on every bring-up is the
		// crying-wolf `palai up` already refuses elsewhere.
		if strings.TrimSpace(get("PALAI_GIT_CLONE_URL")) == "" || strings.TrimSpace(get("PALAI_GIT_BASE_BRANCH")) == "" {
			return nil
		}
		return []string{"a repository is bound but NO GitHub App is configured, so the agent can propose a push " +
			"and a human can approve it and NOTHING WILL HAPPEN: the approved publication waits forever with no " +
			"error anywhere. Fix: set PALAI_GITHUB_APP_ID, PALAI_GITHUB_APP_INSTALLATION_ID and " +
			"PALAI_GITHUB_APP_PRIVATE_KEY_FILE in .env.local (§0.2) and re-run `palai up`"}
	}
	if missing := missingNames(map[string]string{
		"PALAI_GITHUB_APP_ID": appID, "PALAI_GITHUB_APP_INSTALLATION_ID": installID,
		"PALAI_GITHUB_APP_PRIVATE_KEY_FILE": keyFile,
	}); len(missing) > 0 {
		return []string{"the GitHub App is HALF configured (" + strings.Join(missing, " and ") + " unset), so no " +
			"publisher is wired at all and an approved push would wait forever. All three are required together"}
	}

	// The key's BYTES, copied into the slot compose mounts. Read failures are warnings rather than fatal: a
	// bring-up whose model round-trip is about to be proven must not die on a publication path.
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return []string{fmt.Sprintf("PALAI_GITHUB_APP_PRIVATE_KEY_FILE=%s could not be read (%v), so no publisher "+
			"is wired and an approved push would wait forever", keyFile, err)}
	}
	if len(bytes.TrimSpace(keyPEM)) == 0 {
		return []string{"PALAI_GITHUB_APP_PRIVATE_KEY_FILE=" + keyFile + " is empty, so no publisher is wired and " +
			"an approved push would wait forever"}
	}
	if err := os.WriteFile(p.secretPath(gitHubAppKeySlot), keyPEM, 0o600); err != nil {
		return []string{fmt.Sprintf("could not stage the GitHub App key into %s (%v), so no publisher is wired",
			p.secretPath(gitHubAppKeySlot), err)}
	}

	var warnings []string
	exports := map[string]string{
		"PALAI_GITHUB_APP_ID":               appID,
		"PALAI_GITHUB_APP_INSTALLATION_ID":  installID,
		"PALAI_GITHUB_APP_PRIVATE_KEY_FILE": containerGitHubAppKeyPath,
	}
	// owner/repo is what the PULL REQUEST half needs (main.go builds the PR client from it); without it a
	// push still publishes and every pull request answers "no pull-request client wired". §0.2 asks the
	// operator for PALAI_GIT_REPO, which is also what the repository binding uses, so the two cannot drift.
	if slug := repositorySlug(get); slug != "" {
		exports["PALAI_GITHUB_REPO"] = slug
	} else {
		warnings = append(warnings, "the GitHub App is configured but owner/repo is not, so a push will publish and "+
			"every pull request will answer 'no pull-request client wired'. Fix: set PALAI_GIT_REPO=owner/repo")
	}
	for name, value := range exports {
		if err := os.Setenv(name, value); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not export %s: %v", name, err))
		}
	}
	return warnings
}

// missingNames returns the names whose value is empty, sorted so the warning reads the same every run.
func missingNames(values map[string]string) []string {
	var missing []string
	for name, v := range values {
		if v == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// repositorySlug is owner/repo for the PR client: PALAI_GIT_REPO if the operator set it (§0.2), otherwise
// derived from the clone URL the SAME way the repository binding derives its identity — one value, one
// derivation, so the App and the binding can never point at two different repositories.
func repositorySlug(get func(string) string) string {
	if slug := strings.TrimSpace(get("PALAI_GIT_REPO")); strings.Contains(slug, "/") {
		return slug
	}
	if cloneURL := strings.TrimSpace(get("PALAI_GIT_CLONE_URL")); cloneURL != "" {
		if slug := repositoryIdentityFrom(cloneURL); strings.Contains(slug, "/") {
			return slug
		}
	}
	return ""
}

// ensureMasterKey makes $PALAI_HOME/secrets/master-key hold a key identity.ParseMasterKey accepts
// (32 bytes, hex). An EXISTING valid key is never replaced: it seals every secret already in the
// store, and dbSecret fails CLOSED on a decrypt failure rather than falling back — so re-minting
// would not degrade the stack, it would break it.
//
// An existing file holding something else (the `REPLACE_WITH_…` placeholder, a truncated paste) is
// a refusal, not an overwrite: the control-plane treats an unparseable key as a startup error, so
// booting on it would take the whole stack down, and clobbering it would destroy whatever it was.
func ensureMasterKey(p paths) error {
	raw, err := os.ReadFile(p.masterKey)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", p.masterKey, err)
	}
	switch key := strings.TrimSpace(string(raw)); {
	case len(key) == 64 && isHex(key):
		return nil
	case key != "":
		return fmt.Errorf("%s does not hold a 32-byte hex key, so the control-plane would refuse to boot on it "+
			"(it is a startup error by design) — fix: replace it with `openssl rand -hex 32`, or delete it to have one minted", p.masterKey)
	}
	if err := os.MkdirAll(p.secretsDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", p.secretsDir, err)
	}
	// randomHex(32) is 32 random BYTES hex-encoded — the 64-char form ParseMasterKey requires.
	if err := os.WriteFile(p.masterKey, []byte(randomHex(32)), 0o600); err != nil {
		return fmt.Errorf("mint %s: %w", p.masterKey, err)
	}
	return nil
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// appendWarn adds a non-empty warning to the list. Empty warnings are dropped here rather than at every
// call site, because a "" in the report renders as a blank WARNING block.
func appendWarn(warns []string, w string) []string {
	if w == "" {
		return warns
	}
	return append(warns, w)
}

// recreateControlPlane replaces the control-plane container in place, leaving Postgres, the object
// store and the runner alone. The engine digest is re-resolved rather than defaulted to the tag:
// the control-plane hands PALAI_ENGINE_IMAGE to the runner's lease, which requires an immutable
// sha256, so recreating with the mutable tag would break every subsequent run.
func recreateControlPlane(cfg Config, p paths) error {
	digest, err := imageID(engineImage)
	if err != nil {
		return err
	}
	if err := runVisible(cfg.composeEnv(p.home, digest), "docker", "compose", "-p", cfg.Project, "-f", p.compose,
		"up", "-d", "--force-recreate", "--no-deps", "--wait", "control-plane"); err != nil {
		return fmt.Errorf("recreate the control-plane: %w", err)
	}
	return waitForAPI(cfg, p)
}

// restartControlPlane restarts whichever control plane this deployment runs. Its compose and native halves
// are NOT interchangeable and this is the second call site that learned it the expensive way; see the
// comment above the desired-config repair in Bootstrap.
func restartControlPlane(cfg Config, p paths, get func(string) string, native bool) error {
	if native {
		return restartNative(cfg, p, get)
	}
	return recreateControlPlane(cfg, p)
}

// --- the repository a session codes against (E22 T3) -----------------------------------------------

// bootstrapProjectID is the project this stack's bootstrap key is scoped to (identity.firstProject).
// It is the project whose approver list and tool baseline `palai up` reports on, because it is the only
// one this command's own credential can read. install_backup.go pins the same bootstrap ids.
const bootstrapProjectID = "prj_local"

// repositoryBinding is the repository this bring-up bound, or the reason it bound none. id empty means
// nothing on this stack has a repository to work in — a legitimate posture, and the one every stack has
// until an operator names one.
type repositoryBinding struct {
	id       string
	resolved string // operator-facing prose: what was created or reused, printed as it happens
	warn     string // a condition the final report must repeat, because its only other symptom is silence
}

// resolveRepository creates (or reuses) the repository binding a run codes against, from the two values
// §0.2 asks the operator for. Nothing here attaches it: the id is durable configuration, and what
// references it — a session, an agent revision, a bot row — is chosen afterwards in the console.
//
// IT WARNS RATHER THAN SKIPPING SILENTLY, and that is E21 T2's lesson arriving in the same file for the
// second time: a bring-up that quietly does nothing is indistinguishable from one that worked. An operator
// who expected the agent to write code would otherwise meet the gap only as an agent that keeps answering
// "no workspace bound for this run", with no line anywhere connecting that to a variable they never set.
//
// ponytail: no PALAI_REPOSITORY_BINDING_ID override. Every other id in this file has one because the
// operator may already hold it; a binding is created BY this command, so an operator holding one can pass
// its clone_url and get it reused by the lookup below. Add the override when someone has a binding they
// cannot describe.
func resolveRepository(api *apiClient, get func(string) string) repositoryBinding {
	cloneURL := strings.TrimSpace(get("PALAI_GIT_CLONE_URL"))
	branch := strings.TrimSpace(get("PALAI_GIT_BASE_BRANCH"))
	switch {
	case cloneURL == "" && branch == "":
		return repositoryBinding{warn: "no repository is bound on this stack: PALAI_GIT_CLONE_URL and " +
			"PALAI_GIT_BASE_BRANCH are unset, so a run can read and search but CANNOT clone, edit or commit " +
			"anything. Set both in .env.local and re-run `palai up` to bind one, or register one on /repositories."}
	case cloneURL == "":
		return repositoryBinding{warn: "PALAI_GIT_BASE_BRANCH is set but PALAI_GIT_CLONE_URL is not: a binding needs " +
			"BOTH — the clone URL says WHICH repository, the base branch says which branch a pull request will " +
			"target. No repository was bound."}
	case branch == "":
		return repositoryBinding{warn: "PALAI_GIT_CLONE_URL is set but PALAI_GIT_BASE_BRANCH is not: a binding needs " +
			"BOTH, and the base branch is not a default worth guessing — it is the branch every pull request against " +
			"this repository will target. No repository was bound."}
	}
	// The provider repository identity is the authoritative name (spec §30.1); the clone URL is not trusted as
	// identity. PALAI_GIT_REPO is what §0.2 asks for; deriving it from the URL is the fallback so an operator
	// who set only the URL still gets a binding rather than a 400.
	identity := strings.TrimSpace(get("PALAI_GIT_REPO"))
	if identity == "" {
		identity = repositoryIdentityFrom(cloneURL)
	}
	id, reused, err := api.ensureRepositoryBinding(identity, cloneURL, branch)
	if err != nil {
		return repositoryBinding{warn: "no repository was bound: " + err.Error() +
			". Nothing on this stack can clone, edit or commit until this is fixed."}
	}
	verb := "registered"
	if reused {
		verb = "reused"
	}
	return repositoryBinding{
		id: id,
		// The BASE BRANCH is named because it is the whole of "open the PR against dev": the publication
		// destination is resolved from this binding's default_branch and is never asked of the model.
		resolved: fmt.Sprintf("repository: %s %s for %s (clone %s, base branch %s — a pull request against this "+
			"binding targets %s, and no model chooses that)", verb, id, identity, cloneURL, branch, branch),
	}
}

// repositoryIdentityFrom derives owner/repo from a clone URL. A best-effort fallback for the operator who
// set only PALAI_GIT_CLONE_URL; PALAI_GIT_REPO overrides it. Returns the trimmed path, or the URL itself
// when it parses to nothing — the store requires a non-empty identity and an honest echo beats an empty one.
func repositoryIdentityFrom(cloneURL string) string {
	u, err := url.Parse(cloneURL)
	if err != nil {
		return cloneURL
	}
	if path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"); path != "" {
		return path
	}
	return cloneURL
}

// ensureRepositoryBinding returns the id of a binding for this clone URL + base branch, reusing one this
// project already has. Reuse is by (clone_url, default_branch) because those two ARE the binding an operator
// described in .env.local: `palai up` is re-run constantly, and a fresh binding each time would leave a pile
// of duplicates and — worse — move the repository underneath whatever already references the old id.
//
// It matches on default_branch too rather than clone_url alone, so an operator who retargets
// PALAI_GIT_BASE_BRANCH from `main` to `dev` gets a binding that says `dev` instead of a silently inert
// change.
func (c *apiClient) ensureRepositoryBinding(identity, cloneURL, branch string) (id string, reused bool, err error) {
	var page struct {
		Data []struct {
			ID            string `json:"id"`
			CloneURL      string `json:"clone_url"`
			DefaultBranch string `json:"default_branch"`
		} `json:"data"`
	}
	status, err := c.do(http.MethodGet, "/v1/repository-bindings", nil, &page)
	switch {
	case err != nil:
		return "", false, fmt.Errorf("list repository bindings: %w", err)
	case status == http.StatusNotFound:
		return "", false, fmt.Errorf("GET /v1/repository-bindings = 404: this control-plane mounts no repository " +
			"binding surface, so no repository can be bound on this stack")
	case status != http.StatusOK:
		return "", false, fmt.Errorf("GET /v1/repository-bindings = %d, want 200", status)
	}
	for _, b := range page.Data {
		if b.CloneURL == cloneURL && b.DefaultBranch == branch {
			return b.ID, true, nil
		}
	}
	var created struct {
		ID     string `json:"id"`
		Detail string `json:"detail"`
	}
	// allowed_operations is left empty on purpose and it is NOT a permission grant this omits: nothing in the
	// tree reads that column today (it is stored and projected, never evaluated), so writing a list here would
	// be a security-shaped string that enforces nothing. What actually bounds a run is its tool list.
	status, err = c.do(http.MethodPost, "/v1/repository-bindings", map[string]any{
		"provider":            "github",
		"repository_identity": identity,
		"clone_url":           cloneURL,
		"default_branch":      branch,
	}, &created)
	switch {
	case err != nil:
		return "", false, fmt.Errorf("register the repository binding: %w", err)
	case status != http.StatusCreated || created.ID == "":
		return "", false, fmt.Errorf("POST /v1/repository-bindings = %d, want 201 with an id: %s", status, created.Detail)
	}
	return created.ID, false, nil
}

// approverListWarning exists because a project with no approvers list refuses NOTHING —
// config_policy.approvers is unrestricted when absent, deliberately (see coordinator.ConfigPolicy.ApproverAllowed:
// a control that changes behaviour for deployments that never asked for it is an outage). So the open door is
// the default, and the only honest thing to do is say so out loud rather than let an operator discover it.
//
// It names what is actually open, not a generality: the HTTP approve surface. A channel adapter that gates
// its own clicks separately is not claimed here.
//
// Derived from what the running stack ACTUALLY serves — GET /v1/projects/{id} — never from an assumption
// about what `palai up` usually writes. A read that fails warns about the read, because "I could not tell"
// and "there is no list" are different facts and only one of them is about approvals.
func approverListWarning(api *apiClient) string {
	approvers, err := api.projectApprovers(bootstrapProjectID)
	if err != nil {
		return fmt.Sprintf("could not read %s's approver list, so nothing here can tell you whether the approve surfaces are gated: %v",
			bootstrapProjectID, err)
	}
	if len(approvers) > 0 {
		return ""
	}
	return "no project approver list is configured; the HTTP approve surface is gated only by tenant-scoped key possession — " +
		"set one with `palai admin project set-policy " + bootstrapProjectID + " --approvers key:<api_key_id>` " +
		"(see docs/operations/approvals.md). Leaving it unset is a supported posture, not a bug — it is just not a gate"
}

// dispatchWorkerFleetWarning is §3.6 D11 said out loud, and it is the correction of a MEASURED LIE
// rather than a code change.
//
// Concurrency here is min(PALAI_DISPATCH_WORKERS, the fleet's lease slots), and the FIRST of those
// defaults to 1. So an operator who enrols a second Mac adds a machine that parks and is never reached,
// sees no speed-up, and has nowhere to read why: the bound is in the control plane, not in the fleet.
// That matters more now than when it was measured, because it has since been measured that two sessions
// DO run concurrently on one Mac once both knobs are raised — the ceiling is real and it is liftable.
//
// It reads the live registry rather than counting containers, because "enrolled" is a durable fact about
// machines that may not be on this host at all, which is the entire point of a fleet. Read failures are
// reported as such: a warning that silently becomes "no warning" when a read fails is worse than none.
func dispatchWorkerFleetWarning(api *apiClient, get func(string) string) string {
	// Empty follows applyConcurrencyEnv, which exports 1 when the operator set nothing — so the default
	// stack is exactly the case this warning is for.
	workers := strings.TrimSpace(get("PALAI_DISPATCH_WORKERS"))
	if workers == "" {
		workers = "1"
	}
	if workers != "1" {
		return ""
	}
	runners, err := api.enrolledRunnerCount()
	if err != nil {
		return fmt.Sprintf("could not read the enrolled runner count, so nothing here can tell you whether your fleet is reachable: %v", err)
	}
	if runners < 2 {
		return ""
	}
	return fmt.Sprintf("%d runners are enrolled but PALAI_DISPATCH_WORKERS=1 — concurrent runs are bounded by the control plane, not the fleet; "+
		"raise PALAI_DISPATCH_WORKERS (and PALAI_RUNNER_CONCURRENCY, which `palai up` keeps in step with it) or the extra machines park and are never reached", runners)
}

// enrolledRunnerCount reads how many machines are enrolled in the caller's tenant. The page limit is
// deliberately small: the question is "more than one", not "how many exactly", and a fleet of a hundred
// would answer it on the first page.
func (c *apiClient) enrolledRunnerCount() (int, error) {
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	status, err := c.do(http.MethodGet, "/v1/runners?limit=10", nil, &body)
	switch {
	case err != nil:
		return 0, err
	case status == http.StatusNotFound:
		// A stack with no runner gateway does not mount the route. That is a supported posture (a
		// queued-only stack), and it has no fleet to warn about.
		return 0, nil
	case status != http.StatusOK:
		return 0, fmt.Errorf("GET /v1/runners = %d", status)
	}
	return len(body.Data), nil
}

// fleetSummary is the fleet as `palai up` reports it (E24 T6): how many pools, how many machines are
// serving, and how many are WAITING FOR A HUMAN. `err` is kept rather than swallowed for the reason
// dispatchWorkerFleetWarning keeps its: a summary that silently becomes "nothing to say" when a read fails
// is worse than no summary.
type fleetSummary struct {
	pools   int
	active  int
	pending int
	// partial is set when the page was full, so the counts are a floor rather than a total. The line says so
	// rather than presenting a first page as a fleet.
	partial bool
	err     error
}

// fleetPageLimit bounds the read. The question this line answers is "is anything waiting", and a hundred
// machines answer it; a fleet larger than that gets a count marked as a floor.
//
// ponytail: one page, no cursor following. A `?state=pending` filter or a server-side count is the upgrade
// when somebody runs a fleet where 100 is not enough — and neither exists on this surface today.
const fleetPageLimit = 100

// fleet reads the pools and the machines in the caller's tenant and counts them by state.
//
// A `pending` MACHINE IS THE WHOLE REASON THIS EXISTS. Strict enrolment holds a machine out of its pool's
// rendezvous until a human admits it, and a waiting room nobody is shown is a waiting room an operator will
// read as a broken runner — E21 T2's silent-SKIP lesson, on a surface where the silence lasts until somebody
// notices a Mac has been idle for a day.
func (c *apiClient) fleet() fleetSummary {
	var summary fleetSummary
	var machines struct {
		Data []struct {
			State string `json:"state"`
		} `json:"data"`
	}
	status, err := c.do(http.MethodGet, fmt.Sprintf("/v1/runners?limit=%d", fleetPageLimit), nil, &machines)
	switch {
	case err != nil:
		summary.err = err
		return summary
	case status == http.StatusNotFound:
		// A stack with no runner gateway does not mount the route: a queued-only deployment has no fleet, which
		// is a supported posture and not a failure.
		return summary
	case status != http.StatusOK:
		summary.err = fmt.Errorf("GET /v1/runners = %d", status)
		return summary
	}
	summary.partial = len(machines.Data) >= fleetPageLimit
	for _, m := range machines.Data {
		switch m.State {
		case "pending":
			summary.pending++
		case "active":
			summary.active++
		}
	}
	var pools struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if status, err := c.do(http.MethodGet, fmt.Sprintf("/v1/runner-pools?limit=%d", fleetPageLimit), nil, &pools); err != nil {
		summary.err = err
	} else if status == http.StatusOK {
		summary.pools = len(pools.Data)
	} else if status != http.StatusNotFound {
		summary.err = fmt.Errorf("GET /v1/runner-pools = %d", status)
	}
	return summary
}

// fleetLine renders the summary for the report. A machine WAITING is the one state that changes the wording:
// it is the only one an operator has to act on, and it names the command that acts.
func fleetLine(summary fleetSummary) string {
	if summary.err != nil {
		return fmt.Sprintf("could not be read, so nothing here can tell you what your machines are doing: %v", summary.err)
	}
	if summary.pools == 0 && summary.active == 0 && summary.pending == 0 {
		return "no pools and no enrolled machines (a queued-only stack has none)"
	}
	line := fmt.Sprintf("%d pool(s), %d active runner(s), %d pending approval", summary.pools, summary.active, summary.pending)
	if summary.partial {
		line += fmt.Sprintf(" (first %d machines only)", fleetPageLimit)
	}
	if summary.pending > 0 {
		line += " — a PENDING machine holds a certificate and takes NO work until a human admits it: " +
			"`palai admin runner approve <runner_id>`"
	}
	return line
}

// projectApprovers reads a project's configured approver principals off the running stack.
// publishedModelRoute reports the credential THIS PROJECT's runs actually resolve through, or found=false
// when it has none and the deployment default therefore serves.
//
// It reads the three things in the order the resolver does, because there is no single route that answers
// the question: the `default` alias (the ONLY one dispatch consults — coordinator.DefaultModelRouteAlias),
// its revision carrying `resolved_by_dispatch`, and that revision's connection for the secret REF name. A
// value never travels on any of them; `ref` is a handle.
func (c *apiClient) publishedModelRoute() (model, ref, connection string, found bool, err error) {
	var routes struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	status, err := c.do(http.MethodGet, "/v1/model-routes?limit=100", nil, &routes)
	switch {
	case err != nil:
		return "", "", "", false, err
	case status == http.StatusNotFound:
		// A tier that wires no model-route store leaves the routes unmounted. Not a failure: that deployment
		// has exactly one authority, which is the case this warning exists to distinguish.
		return "", "", "", false, nil
	case status != http.StatusOK:
		return "", "", "", false, fmt.Errorf("GET /v1/model-routes = %d", status)
	}
	routeID := ""
	for _, r := range routes.Data {
		if r.Name == defaultModelRouteAlias {
			routeID = r.ID
			break
		}
	}
	if routeID == "" {
		return "", "", "", false, nil
	}

	var revisions struct {
		Data []struct {
			Model        string `json:"model"`
			ConnectionID string `json:"connection_id"`
			Resolved     bool   `json:"resolved_by_dispatch"`
		} `json:"data"`
	}
	if status, err := c.do(http.MethodGet, "/v1/model-routes/"+url.PathEscape(routeID)+"/revisions?limit=100", nil, &revisions); err != nil {
		return "", "", "", false, err
	} else if status != http.StatusOK {
		return "", "", "", false, fmt.Errorf("GET /v1/model-routes/%s/revisions = %d", routeID, status)
	}
	// `resolved_by_dispatch` IS THE FIELD, and picking the newest published revision by hand instead would be
	// a second opinion about which one steers. The store computes it; a bring-up that recomputed it could
	// disagree with the resolver and report the wrong credential as live.
	for _, rev := range revisions.Data {
		if !rev.Resolved {
			continue
		}
		model, connection = rev.Model, rev.ConnectionID
		var conn struct {
			SecretRef string `json:"secret_ref"`
		}
		// The ref is a nicety on top of the finding, so a failure here loses the NAME and keeps the fact.
		if status, err := c.do(http.MethodGet, "/v1/model-connections/"+url.PathEscape(connection), nil, &conn); err == nil && status == http.StatusOK {
			ref = conn.SecretRef
		}
		return model, ref, connection, true, nil
	}
	return "", "", "", false, nil
}

// modelAuthorityWarning is THE SECOND AUTHORITY FOR THIS STACK'S MODEL CREDENTIAL, said out loud.
//
// THERE ARE TWO, AND UNTIL NOW THE PRECEDENCE BETWEEN THEM WAS SILENT — which is the defect this tree keeps
// finding, and the reason this function exists rather than a comment:
//
//	the FILE      OPENAI_API_KEY in .env.local -> the 0600 $PALAI_HOME/secrets/provider-one file secret ->
//	              PALAI_SECRET_PROVIDER_ONE -> main.modelBrokerFromEnv's DEPLOYMENT-DEFAULT route.
//	the PLATFORM  a secret ref + a model connection + a PUBLISHED `default` route revision, all of which the
//	              console now writes.
//
// coordinator.ProjectModelRoute resolves the platform's answer and returns found=false when the project has
// none, at which point the caller falls back to the deployment default. So the platform WINS where it has an
// answer — correctly — and a bring-up that re-seals the file's key and prints PROVEN LIVE tells the operator
// nothing about which of the two just served their run.
//
// THE FILE READ IS KEPT AND THAT IS THE DECISION. Removing it was the other option and it is wrong on the
// only day that matters: on a fresh machine there is no platform to hold a credential — the database is
// empty, the control plane is not up, and the console cannot be reached — so something must seed the first
// one or `palai up`'s whole purpose, proving a LIVE round-trip, is unreachable on day one. What was not
// acceptable is the silence, so the file stays a BOOTSTRAP SEED and stops being an unannounced authority.
//
// It warns only when BOTH exist, because only then is there something an operator can be wrong about: a
// stack with a route and no file has one authority, and a stack with a file and no route has one authority.
func modelAuthorityWarning(api *apiClient, get func(string) string) string {
	// The file's key is the only half this command controls; if the operator supplied none, there is no
	// second authority to announce even when a route is published.
	if strings.TrimSpace(get(credentialEnv)) == "" {
		return ""
	}
	model, ref, connection, found, err := api.publishedModelRoute()
	if err != nil {
		// "I could not tell" and "there is no route" are different facts, and only one of them is about the
		// credential. A warning that silently becomes no-warning on a read failure is worse than none.
		return fmt.Sprintf("could not read this project's model route, so nothing here can tell you whether %s in .env.local is "+
			"what your runs actually use: %v", credentialEnv, err)
	}
	if !found {
		return ""
	}
	where := "connection " + connection
	if ref != "" {
		where += " (credential " + ref + ")"
	}
	return fmt.Sprintf("%s in .env.local is NOT what this project's runs use. %s has a PUBLISHED model route: runs resolve "+
		"through %s on %s, which was configured in the console and overrides the deployment default this bring-up seeded from "+
		"the file. The file's key still serves projects that have no route of their own. To change what %s runs, use the "+
		"console's \"Point runs at a connection\" on /registry — editing .env.local will not move it",
		credentialEnv, bootstrapProjectID, model, where, bootstrapProjectID)
}

func (c *apiClient) projectApprovers(projectID string) ([]string, error) {
	var body struct {
		ConfigPolicy struct {
			Approvers []string `json:"approvers"`
		} `json:"config_policy"`
	}
	status, err := c.do(http.MethodGet, "/v1/projects/"+projectID, nil, &body)
	switch {
	case err != nil:
		return nil, err
	case status != http.StatusOK:
		return nil, fmt.Errorf("GET /v1/projects/%s = %d", projectID, status)
	}
	return body.ConfigPolicy.Approvers, nil
}

// --- the final report ------------------------------------------------------------------------

// capRow is one line of the capability table.
type capRow struct{ name, state, reason string }

// observedFacts collects what THIS bring-up actually observed, keyed by capability name. Only a
// capability with a fact here is called live; everything else is dormant with the tier the
// deployment served as its reason.
//
// The round trip is the one thing this command PROVES, so it is the one fact there is. A capability this
// bring-up merely configured does not earn `live` — that word is reserved for something observed.
func observedFacts(rt roundTrip) map[string]string {
	return map[string]string{
		"responses": fmt.Sprintf("real round-trip proven: %s, %d in / %d out", rt.Model, rt.InputTokens, rt.OutputTokens),
	}
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
func printReport(cfg Config, posture string, rt roundTrip, caps map[string]string, facts map[string]string, fleet string, red []string, warnings ...string) {
	out := os.Stdout
	fmt.Fprintln(out, "\nPROVEN LIVE")
	fmt.Fprintf(out, "  round-trip   %s -> %s\n", rt.ResponseID, rt.Status)
	fmt.Fprintf(out, "  model        %s   (selector %s — NOT the fake adapter)\n", rt.Model, liveSelector)
	fmt.Fprintf(out, "  usage        %d in / %d out / %d total tokens\n", rt.InputTokens, rt.OutputTokens, rt.TotalTokens)
	fmt.Fprintf(out, "  api          %s\n", cfg.BaseURL)
	// WHICH POSTURE THIS BRING-UP ACTUALLY BROUGHT UP. Both postures serve the same API on the same
	// port, so nothing else on this report distinguishes a control plane in a container from one on
	// this machine — and the difference is the whole of whether `xcodebuild` is reachable.
	fmt.Fprintf(out, "  posture      %s\n", posture)
	// THE FLEET, on the report rather than in a warning (E24 T6): how many pools, how many machines are
	// serving, and how many are waiting for a human. A pending machine that nothing prints is a Mac an
	// operator assumes is working.
	fmt.Fprintf(out, "  fleet        %s\n", fleet)

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
// calls below (create, retrieve, capabilities, and the deployment's own configuration reads) all route
// through it.
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

// applyConcurrencyEnv exports BOTH concurrency knobs, because a stack is only as parallel as the smaller of
// them and compose reads both from the environment. Extracted from Bootstrap so the pairing is testable —
// the defect it fixes was invisible precisely because nothing could assert it.
func applyConcurrencyEnv(get func(string) string) error {
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
	// THE RUNNER'S CONCURRENCY MUST BE EXPORTED TOO, and forgetting it is why a stack could be told to run
	// four runs at once and still run them one at a time. PALAI_DISPATCH_WORKERS is how many runs the control
	// plane will dispatch; PALAI_RUNNER_CONCURRENCY is how many engine leases ONE runner identity parks at
	// once (runner_concurrency_test.go: at the default of 1 "a second concurrent Dial blocks"). Compose reads
	// the second from the environment and `palai up` never put it there, so it was always 1 whatever
	// .env.local said — three of four dispatch workers queueing behind one engine, with nothing said about it.
	//
	// UNSET FOLLOWS THE DISPATCH COUNT, because the two numbers answer the same question — "how many runs at
	// once" — and letting them disagree silently is the defect above. An explicit value always wins.
	runners := strings.TrimSpace(get("PALAI_RUNNER_CONCURRENCY"))
	if runners == "" {
		runners = os.Getenv("PALAI_DISPATCH_WORKERS")
	}
	if runners != "" {
		if err := os.Setenv("PALAI_RUNNER_CONCURRENCY", runners); err != nil {
			return err
		}
	}
	return nil
}

// emptyToolBaselineWarning reports the project granting no tools, or "" when it grants some or when the
// question cannot be answered. It reads the project this deployment's key is scoped to.
//
// IT IS SILENT ON FAILURE ON PURPOSE. A warning that cannot distinguish "this project grants nothing"
// from "the read did not happen" is the shape this tree keeps paying for — it would fire on every
// deployment whose policy route moved, and an operator who learns to ignore it has lost the real one.
func (c *apiClient) emptyToolBaselineWarning() string {
	var page struct {
		Data []struct {
			ID     string `json:"id"`
			Policy *struct {
				DefaultTools []string `json:"default_tools"`
			} `json:"config_policy"`
		} `json:"data"`
	}
	status, err := c.do(http.MethodGet, "/v1/projects", nil, &page)
	if err != nil || status != http.StatusOK || len(page.Data) == 0 {
		return ""
	}
	// The deployment's own project is the one this key writes to; `palai up` provisions prj_local.
	for _, p := range page.Data {
		if p.ID != "prj_local" {
			continue
		}
		if p.Policy != nil && len(p.Policy.DefaultTools) > 0 {
			return ""
		}
		return "this project grants NO default tools, so every agent's effective tool set is EMPTY: a " +
			"revision's tool list is a ceiling INTERSECTED with the project baseline, and an empty baseline " +
			"empties it. Runs will complete, answer in one step and call nothing. Grant a baseline with " +
			"`palai admin project set-policy prj_local --default-tools=palai.workspace.file,palai.workspace.shell` " +
			"(add palai.workspace.show_media for screenshots, palai.publish.push for commits)."
	}
	return ""
}
