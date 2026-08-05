//go:build component

package execution

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// E26 T6 — THE CREDENTIAL, against a REAL Postgres, a REAL detached host process and a REAL
// AES-256-GCM secret store.
//
// The claim these tests exist for is §0.4's, and it is the one this plan called its sharpest question:
// a background task DOES carry an environment value, so the value's lifetime is no longer the Execute
// call — it is the TASK. Three consequences are measured here and each of them is a behaviour rather
// than a paragraph: the bytes a process wrote to its own log are redacted ON THE WAY OUT at BOTH places
// they reach a model, no field of any row carries a value, and a task that carries a credential cannot
// be created without a ceiling on how long it will hold one.
//
// EVERY VALUE HERE TRAVELS THE PRODUCTION PATH. It is sealed by the real identity.SecretStore, named by
// a real agent revision, read by the shipped RunEnvironmentKeys, resolved at the last moment by the
// shipped resolveEnvValues, and handed to a real `printenv` inside a real detached process. Nothing is
// injected past a seam.

// envValueSentinel is the environment VALUE under test, and it is deliberately NOT a secret SHAPE:
// nothing in secretPatterns matches it, so every mask this file asserts is proof that the VALUE-based
// redactor ran. A sentinel that happened to look like an API key would have been masked by RedactSecrets
// alone and the whole file would have proven the wrong half.
const envValueSentinel = "e26-t6-deploy-token-4f19ac82"

// shapedSecret is the OTHER half of D8's "both redactors run": a token SHAPE that no environment value
// resolves to, so masking it can only come from RedactSecrets. It is assembled rather than written as a
// literal so this repository commits nothing that reads like a credential.
func shapedSecret() string { return "ghp_" + strings.Repeat("A", 24) }

// secretFixture is the wake fixture (a real run, a real allocation root, the real host executor as both
// shell and background runner, the shipped reconciler) PLUS an environment the run's pinned revision
// names, sealed through the real secret store.
type secretFixture struct {
	*wakeFixture
	envID string
	// resolves counts how many times the run's secret resolver was ASKED for a value. It is the whole of
	// the park proof: "no credential was held while a human decided" is a claim about a call that did not
	// happen, and a counter is the only way to tell that apart from a call whose result was discarded.
	resolves int32
}

// newSecretFixture seeds the environment, the profile, the revision and the pin, then wires the SAME
// resolver main.go wires (identity.SecretStore.Resolve) behind a counter.
func newSecretFixture(t *testing.T) *secretFixture {
	t.Helper()
	f := &secretFixture{wakeFixture: newWakeFixture(t)}
	ctx := context.Background()
	sys := storage.WithSystemScope(ctx)

	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i*11 + 3)
	}
	key, err := identity.ParseMasterKey(hex.EncodeToString(raw[:]))
	if err != nil {
		t.Fatalf("ParseMasterKey: %v", err)
	}
	secrets := identity.NewSecretStore(f.spine.Pool(), key)

	f.envID = redeliveryID("env")
	if _, err := f.spine.Pool().Exec(sys,
		// THE NAME CARRIES THE FIXTURE'S OWN ID. environments_name_key is UNIQUE (project_id, name), and this
		// INSERT names no project_id — so the row lands in the empty-project bucket shared by every other
		// fixture that also writes none, across a database dozens of harnesses share. A literal 'production'
		// collides with all of them.
		`INSERT INTO environments (id, name) VALUES ($1,$2)`,
		f.envID, "production-"+f.envID); err != nil {
		t.Fatalf("seed the environment: %v", err)
	}
	// The write path is the REAL one: PutEnvironmentValue seals into secret_refs under the derived name
	// the SQL rebuilds, so a disagreement between the two spellings would resolve nothing here.
	out, err := secrets.PutEnvironmentValue(ctx, middleware.Scope{Project: f.tenant.Project},
		f.envID, []byte(`{"key":"DEPLOY_TOKEN","value":"`+envValueSentinel+`"}`))
	if err != nil || out.BadField || out.NotFound {
		t.Fatalf("PutEnvironmentValue: %+v %v", out, err)
	}

	profileID, revID := redeliveryID("aprof"), redeliveryID("arev")
	stmts := [][]any{
		{`INSERT INTO agent_profiles (id, project_id, name) VALUES ($1,$2,'deployer')`,
			profileID, f.tenant.Project},
		// tools stays NULL: a revision's tools column is a CEILING that INTERSECTS when it is non-nil
		// (config.go), so a fixture that named one would silently take the shell tool away from the run.
		{`INSERT INTO agent_revisions (id, project_id, profile_id, revision_number, environment, published_at)
		  VALUES ($1,$2,$3,1,$4,clock_timestamp())`,
			revID, f.tenant.Project, profileID, f.envID},
		{`UPDATE runs SET agent_revision_id = $2 WHERE id = $1`, f.runID, revID},
	}
	for _, stmt := range stmts {
		if _, err := f.spine.Pool().Exec(sys, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("pin the run to an environment: %v", err)
		}
	}

	f.orch.SetEnvironmentSecrets(func(forTenant coordinator.Tenant, ref string) ([]byte, error) {
		atomic.AddInt32(&f.resolves, 1)
		v, ok, err := secrets.Resolve(ctx, forTenant, ref)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("no such environment secret ref %q", ref)
		}
		return v, nil
	})
	return f
}

// secretScript is a build that PRINTS ITS OWN ENVIRONMENT — the accidental leak RedactValues' own
// comment says is the failure that happens by default, and the one a background task makes durable. It
// blocks on the same sentinel wakeFixture.finishTask drops, so the log is complete before anything reads
// it, and exits 7 so the notification carries an exit code the operating system reported.
func secretScript(pidFile, doneFile string) string {
	return "echo $$ > " + pidFile + "; while [ ! -f " + doneFile + " ]; do sleep 0.05; done; " +
		"printenv DEPLOY_TOKEN; echo " + shapedSecret() + "; echo " + wakeTailMarker + "; exit 7"
}

// spawnLeakyBuild drives ONE whole attempt through ExecuteAttempt: engine.ready, a tool.request for
// palai.workspace.shell with background: true, then a completed terminal. Everything about the
// credential is production — ExecuteAttempt resolves the key NAMES at attempt start and the VALUES one
// statement before Execute, exactly as it does for a synchronous call.
func (f *secretFixture) spawnLeakyBuild(t *testing.T) {
	t.Helper()
	frames := []contracts.EngineFrame{
		engineFrame(1, "engine.ready", map[string]any{
			"selected_protocol": engineProtocol, "engine": map[string]any{"version": "test"},
		}),
		engineFrame(2, "tool.request", map[string]any{
			"tool_call_id": redeliveryID("tc"), "name": "palai.workspace.shell",
			"arguments": map[string]any{
				"argv": []any{secretScript(f.pidFile, f.doneFile)}, "shell": true, "background": true,
			},
		}),
		engineFrame(3, "run.terminal", map[string]any{"outcome": "completed"}),
	}
	t.Cleanup(func() {
		if pgid, err := readPgid(f.pidFile); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	})
	// THE ATTEMPT REACHES THIS FIXTURE'S MACHINE (A.3 T7). The credential travels to it in cmd.Env, over
	// the same wire and the same encode production uses — which is the half of this file's claim that
	// only became measurable once the spawn crossed a machine boundary at all.
	f.orch.dialer = &scriptedDialer{ch: &scriptedChannel{frames: frames}, exec: f.exec, machine: f.machineID}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := f.orch.ExecuteAttempt(ctx, AttemptDescriptor{
		RunID: contracts.RunID(f.runID), AttemptID: contracts.AttemptID(redeliveryID("att")),
		Fence: 1, WorkspaceHostPath: f.root,
	}); err != nil {
		t.Fatalf("ExecuteAttempt: %v", err)
	}
	if _, err := waitPgid(f.pidFile); err != nil {
		t.Fatalf("the background task never started: %v", err)
	}
}

// dispatch drives ONE tool call through the production dispatcher, with the attempt's environment key
// names resolved by the SHIPPED resolver — which is what orchestrator.go does at attempt start. withEnv
// false models a run whose revision names no environment, which is every run in every deployment before
// E25 and the state the fourth RED's negative half needs.
func (f *secretFixture) dispatch(t *testing.T, withEnv bool, name string, args map[string]any) (map[string]any, error) {
	t.Helper()
	ctx := context.Background()
	ch := &recordingChannel{}
	st := &attemptState{
		attempt: AttemptDescriptor{
			RunID: contracts.RunID(f.runID), AttemptID: contracts.AttemptID(redeliveryID("att")),
			Fence: 2, WorkspaceHostPath: f.root,
		},
		tenant: f.tenant, sessionID: f.sessionID, responseID: f.responseID,
		// THE ATTEMPT LANDS ON THIS FIXTURE'S MACHINE, the same one spawnLeakyBuild's dialer reaches. A
		// bare recordingChannel reaches an engine and no machine, which since A.3 T7 means it can start no
		// background task at all — and the calls below that expect a REFUSAL would then be refused by the
		// machine gate rather than by the ceiling they are about, which is the same green-for-the-wrong-
		// reason the credential claim itself exists to rule out.
		ch: hostMachineChannel{EngineChannel: ch, exec: f.exec, machineID: f.machineID},
	}
	if withEnv {
		keys, err := f.orch.resolveEnvKeys(ctx, st)
		if err != nil {
			t.Fatalf("resolveEnvKeys: %v", err)
		}
		if len(keys) != 1 {
			t.Fatalf("the run resolved %d environment keys, want 1 — the fixture's pin is wrong", len(keys))
		}
		st.envKeys = keys
	}
	if err := f.orch.dispatchTool(ctx, st, toolRequestFrame(redeliveryID("tc"), name, args)); err != nil {
		return nil, err
	}
	results := toolResults(ch)
	if len(results) != 1 {
		t.Fatalf("%s delivered %d tool.result frames, want 1", name, len(results))
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(results[0].content), &decoded); err != nil {
		t.Fatalf("decode %s result: %v", name, err)
	}
	return decoded, nil
}

// readLogThroughTheFileTool reads the task's output the way §2.2 says a model reads it: through the
// SHIPPED palai.workspace.file, with no new tool added for the purpose.
func (f *secretFixture) readLogThroughTheFileTool(t *testing.T, rel string) string {
	t.Helper()
	out, err := f.dispatch(t, true, "palai.workspace.file", map[string]any{"op": "read", "path": rel})
	if err != nil {
		t.Fatalf("palai.workspace.file read %s: %v", rel, err)
	}
	content, _ := out["content"].(string)
	return content
}

// ---------------------------------------------------------------------------------------------------
// RED 2 (§T6) — REDACTION LIVES ON THE READ PATH, AND ONE PLACE SERVES BOTH LANDING SITES.
// ---------------------------------------------------------------------------------------------------

// TestBothTheFileReadAndTheExitNoticeAreRedactedFromOnePlace is D8's whole correction, measured at both
// ends in one function BECAUSE THEY MUST NOT BE ABLE TO DIVERGE. T2 recorded the first landing site (a
// process writing its own log bypasses the redaction of a captured Go string) and T4 widened it to the
// second (the notice quotes the last 2 KiB into commands.payload and then delivered_messages). Two
// redaction points would be two chances for them to disagree — the argument T5 used to collapse two
// setters into one — so the assertion is that the same call masks both.
//
// THE NON-VACUITY HALF IS IN THE SAME FUNCTION and it is the raw file on disk: the process really did
// write the credential, so a passing read is a read that MASKED something rather than a read of a log
// that never contained it.
func TestBothTheFileReadAndTheExitNoticeAreRedactedFromOnePlace(t *testing.T) {
	f := newSecretFixture(t)
	f.spawnLeakyBuild(t)
	f.finishTask(t)

	taskID, _, outputPath, _, _ := f.taskRow(t)

	// NON-VACUITY, FIRST. The bytes on the allocation carry the value verbatim; that is what a background
	// process does and no read path can change it.
	onDisk, err := os.ReadFile(filepath.Join(f.root, outputPath))
	if err != nil {
		t.Fatalf("read the task's log from disk: %v", err)
	}
	if !strings.Contains(string(onDisk), envValueSentinel) {
		t.Fatalf("the build never printed its environment, so nothing below could be a redaction proof:\n%s", onDisk)
	}
	if !strings.Contains(string(onDisk), shapedSecret()) {
		t.Fatalf("the build never printed a secret-shaped token, so the shape half proves nothing:\n%s", onDisk)
	}

	// LANDING SITE ONE — the shipped file tool.
	content := f.readLogThroughTheFileTool(t, outputPath)
	if strings.Contains(content, envValueSentinel) {
		t.Fatalf("palai.workspace.file returned the environment VALUE to the model:\n%s", content)
	}
	if strings.Contains(content, shapedSecret()) {
		t.Fatalf("palai.workspace.file returned a secret-SHAPED token to the model:\n%s", content)
	}
	if !strings.Contains(content, wakeTailMarker) {
		t.Fatalf("the read lost the build's own output as well as its credential:\n%s", content)
	}
	if strings.Count(content, "***") < 2 {
		t.Fatalf("the read masked fewer than the two things it was supposed to mask:\n%s", content)
	}

	// LANDING SITE TWO — the shipped reconciler sweep and the shipped notice composer.
	f.sweepOnce(t)
	notices := f.backgroundNotices(t)
	if len(notices) != 1 {
		t.Fatalf("the sweep queued %d background notices, want 1", len(notices))
	}
	notice := notices[0]
	if strings.Contains(notice, envValueSentinel) {
		t.Fatalf("the exit notice quoted the environment VALUE into commands.payload:\n%s", notice)
	}
	if strings.Contains(notice, shapedSecret()) {
		t.Fatalf("the exit notice quoted a secret-SHAPED token into commands.payload:\n%s", notice)
	}
	if !strings.Contains(notice, taskID) || !strings.Contains(notice, wakeTailMarker) {
		t.Fatalf("the notice lost the task id or the build's marker, so it is not the excerpt under test:\n%s", notice)
	}
	if strings.Count(notice, "***") < 2 {
		t.Fatalf("the notice's excerpt masked fewer than two things:\n%s", notice)
	}

	// AND THE FAIL-CLOSED ARM OF THE SAME FUNCTION, which is what background_tasks.env_keys buys the
	// notice and cannot buy the read: a key the ROW says the task carried, whose value can no longer be
	// resolved — an environment value deleted while the log sat on disk — WITHHOLDS the bytes rather than
	// serving what it knows it cannot mask.
	if _, err := f.orch.redactTaskOutput(context.Background(), f.tenant, f.runID,
		[]string{"A_KEY_THIS_ENVIRONMENT_NO_LONGER_HAS"}, string(onDisk)); err == nil {
		t.Fatal("output of a task carrying an unresolvable environment key was served anyway")
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 3 (§T6) — NO VALUE IN THE ROW, DECODED RATHER THAN GREPPED.
// ---------------------------------------------------------------------------------------------------

// decodeAndScan walks a value the DATABASE decoded and reports every path at which a needle appears.
//
// IT DECODES BEFORE IT SCANS, and that is the whole difference between a proof and a decoration: a raw
// byte scan over an encoded or compressed field CAN NEVER FAIL, and this repository shipped exactly that
// mistake once (a "no secret in this bundle" assertion over gzip bytes, caught in E14 T7) and met its
// sibling in E20 T4 (a JSON encoder that escapes `<>&`, which made every raw substring assertion over a
// rendered block vacuous). So a string that parses as JSON is re-walked as JSON, and a string that
// decodes as base64 into text is re-walked as text.
func decodeAndScan(path string, v any, needle string) []string {
	var hits []string
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		hits = append(hits, decodeAndScanString(path, t, needle)...)
	case []byte:
		hits = append(hits, decodeAndScanString(path, string(t), needle)...)
	case map[string]any:
		for k, sub := range t {
			hits = append(hits, decodeAndScan(path+"."+k, sub, needle)...)
		}
	case []any:
		for i, sub := range t {
			hits = append(hits, decodeAndScan(fmt.Sprintf("%s[%d]", path, i), sub, needle)...)
		}
	case []string:
		for i, sub := range t {
			hits = append(hits, decodeAndScanString(fmt.Sprintf("%s[%d]", path, i), sub, needle)...)
		}
	default:
		hits = append(hits, decodeAndScanString(path, fmt.Sprintf("%v", t), needle)...)
	}
	return hits
}

func decodeAndScanString(path, s, needle string) []string {
	var hits []string
	if strings.Contains(s, needle) {
		hits = append(hits, path)
	}
	// A JSONB column arrives here already decoded, but a JSON DOCUMENT STORED AS TEXT does not — and the
	// tool ledger's arguments and result are exactly that shape.
	var nested any
	if err := json.Unmarshal([]byte(s), &nested); err == nil {
		switch nested.(type) {
		case map[string]any, []any:
			hits = append(hits, decodeAndScan(path+"|json", nested, needle)...)
		}
	}
	if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s)); err == nil && len(raw) > 0 {
		if decoded := string(raw); strings.ContainsRune(decoded, ' ') || isMostlyPrintable(decoded) {
			if strings.Contains(decoded, needle) {
				hits = append(hits, path+"|base64")
			}
		}
	}
	return hits
}

func isMostlyPrintable(s string) bool {
	printable := 0
	for _, r := range s {
		if r >= 0x20 && r < 0x7f {
			printable++
		}
	}
	return printable*4 >= len(s)*3
}

// scanTable decodes EVERY column of every row a query returns and scans each decoded field.
func scanTable(t *testing.T, f *secretFixture, label, query string, args ...any) []string {
	t.Helper()
	rows, err := f.spine.Pool().Query(storage.WithSystemScope(context.Background()), query, args...)
	if err != nil {
		t.Fatalf("read %s: %v", label, err)
	}
	defer rows.Close()
	var hits []string
	names := make([]string, 0, len(rows.FieldDescriptions()))
	for _, fd := range rows.FieldDescriptions() {
		names = append(names, fd.Name)
	}
	seen := 0
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("decode a %s row: %v", label, err)
		}
		for i, v := range values {
			hits = append(hits, decodeAndScan(fmt.Sprintf("%s[%d].%s", label, seen, names[i]), v, envValueSentinel)...)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", label, err)
	}
	if seen == 0 {
		t.Fatalf("%s returned no rows at all, so scanning it proves nothing", label)
	}
	return hits
}

// TestNoDecodedFieldOfATaskRowItsEventsOrItsCommandsCarriesAnEnvironmentValue is §T6's third RED.
//
// THE MEASUREMENT IS A DECODE, NOT A GREP, and the scanner's own non-vacuity is asserted first against
// the one place the value legitimately IS — the log file the process wrote — because a scanner that
// cannot find a value it is looking at proves nothing about the places it does not find one.
func TestNoDecodedFieldOfATaskRowItsEventsOrItsCommandsCarriesAnEnvironmentValue(t *testing.T) {
	// The control plane's own log lines are part of the claim, so they are captured for the whole flow.
	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	f := newSecretFixture(t)
	f.spawnLeakyBuild(t)
	f.finishTask(t)
	f.sweepOnce(t)
	_, _, outputPath, _, _ := f.taskRow(t)
	f.readLogThroughTheFileTool(t, outputPath)

	// NON-VACUITY OF THE SCANNER ITSELF.
	onDisk, err := os.ReadFile(filepath.Join(f.root, outputPath))
	if err != nil {
		t.Fatalf("read the task's log: %v", err)
	}
	if hits := decodeAndScan("log-file", string(onDisk), envValueSentinel); len(hits) == 0 {
		t.Fatal("the scanner cannot find the value in the file that certainly contains it: every assertion below would be vacuous")
	}
	// And it sees through an ENCODING, which is the specific failure this discipline exists for.
	encoded := base64.StdEncoding.EncodeToString([]byte("prefix " + envValueSentinel + " suffix"))
	if hits := decodeAndScan("encoded", encoded, envValueSentinel); len(hits) == 0 {
		t.Fatal("the scanner cannot see through base64, so it is the raw-byte scan this rule forbids")
	}

	var hits []string
	hits = append(hits, scanTable(t, f, "background_tasks",
		`SELECT * FROM background_tasks WHERE run_id = $1`, f.runID)...)
	hits = append(hits, scanTable(t, f, "events",
		`SELECT * FROM events WHERE session_id = $1 ORDER BY seq`, f.sessionID)...)
	hits = append(hits, scanTable(t, f, "commands",
		`SELECT * FROM commands WHERE run_id = $1`, f.runID)...)
	hits = append(hits, scanTable(t, f, "tool_calls",
		`SELECT * FROM tool_calls WHERE run_id = $1`, f.runID)...)
	if len(hits) > 0 {
		t.Fatalf("AN ENVIRONMENT VALUE IS IN A DURABLE ROW at: %s", strings.Join(hits, ", "))
	}
	if strings.Contains(logged.String(), envValueSentinel) {
		t.Fatalf("an environment value reached a control-plane log line:\n%s", logged.String())
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 4 (§0.2, §0.4) — UNBOUNDED PLUS A CREDENTIAL IS REFUSED, BY A CODE GATE.
// ---------------------------------------------------------------------------------------------------

// TestACredentialCarryingTaskCannotBeCreatedWithoutADeadline is the PRICE of §0.4's option (b), written
// as a refusal rather than as a warning. A value handed over in exec.Cmd.Env lives in the kernel's
// environ copy for the whole life of the process; if that life has no ceiling, neither does the
// exposure. §0.2 therefore makes the ceiling MANDATORY for a task that carries one.
//
// IT IS A CODE GATE AND NOT A CHECK CONSTRAINT, deliberately: choosing unbounded by writing a `0` is a
// legitimate operator decision for a task with NO credential, and the second half of this test is that
// decision still working.
func TestACredentialCarryingTaskCannotBeCreatedWithoutADeadline(t *testing.T) {
	t.Setenv("PALAI_BACKGROUND_MAX_WALL_TIME", "0") // explicitly unbounded, the one spelling that means it
	f := newSecretFixture(t)

	_, err := f.dispatch(t, true, "palai.workspace.shell", map[string]any{
		"argv": []any{"echo $$ > " + f.pidFile + "; sleep 30"}, "shell": true, "background": true,
	})
	if err == nil {
		t.Fatal("a task carrying an environment value was created with no deadline at all: the value's exposure has no ceiling")
	}
	if !strings.Contains(err.Error(), "PALAI_BACKGROUND_MAX_WALL_TIME") {
		t.Fatalf("the refusal does not name the setting an operator has to change: %v", err)
	}
	var rows int
	if err := f.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM background_tasks WHERE run_id = $1`, f.runID).Scan(&rows); err != nil {
		t.Fatalf("count background tasks: %v", err)
	}
	if rows != 0 {
		t.Fatalf("the refused call left %d background_tasks row(s) behind", rows)
	}
	if _, err := os.Stat(f.pidFile); err == nil {
		t.Fatal("the refused call started a process anyway")
	}

	// THE NEGATIVE HALF: the identical call on the identical harness, with NO environment on the attempt,
	// is ACCEPTED and its row carries a NULL deadline. Unbounded is a decision, and this is it working.
	out, err := f.dispatch(t, false, "palai.workspace.shell", map[string]any{
		"argv": []any{"echo $$ > " + f.pidFile + "; sleep 30"}, "shell": true, "background": true,
	})
	if err != nil {
		t.Fatalf("an unbounded task with NO credential was refused as well: %v", err)
	}
	t.Cleanup(func() {
		if pgid, perr := readPgid(f.pidFile); perr == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	})
	taskID, _ := out["task_id"].(string)
	var deadline *time.Time
	var envKeys []string
	if err := f.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT deadline_at, env_keys FROM background_tasks WHERE id = $1`, taskID).Scan(&deadline, &envKeys); err != nil {
		t.Fatalf("read the credential-free task's row: %v", err)
	}
	if deadline != nil {
		t.Fatalf("an explicit PALAI_BACKGROUND_MAX_WALL_TIME=0 still recorded a deadline (%s)", deadline)
	}
	if len(envKeys) != 0 {
		t.Fatalf("the credential-free task recorded env_keys %v", envKeys)
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 5 (§T6) — A PARKED RUN HOLDS NO VALUE, AND THE BACKGROUND PATH DID NOT MOVE THE RESOLUTION.
// ---------------------------------------------------------------------------------------------------

// TestARunParkedForApprovalOnABackgroundCallResolvedNoEnvironmentValue pins the ORDERING E25 T3 built
// the expiry guarantee on: resolveEnvValues runs one statement before Execute, after the durable
// consult and after BOTH approval parks. A spawn IS an Execute, so the background path inherits that
// ordering unchanged — and a run parked waiting for a human to approve a build holds no credential in
// memory while it waits.
//
// THE MEASUREMENT IS A CALL THAT DID NOT HAPPEN, counted at the resolver, because that is the only way
// to tell "never resolved" apart from "resolved and discarded". Its non-vacuity half is the same call
// with the gate OFF, which must resolve exactly once.
func TestARunParkedForApprovalOnABackgroundCallResolvedNoEnvironmentValue(t *testing.T) {
	f := newSecretFixture(t)
	// The shell tool as a deployment that GATES it registers it — through the shipped broker, so the park
	// below is the shipped approval gate rather than a branch this test wrote.
	f.orch.tools = gatedShellBroker(true)

	if _, err := f.dispatch(t, true, "palai.workspace.shell", map[string]any{
		"argv": []any{"echo $$ > " + f.pidFile + "; sleep 30"}, "shell": true, "background": true,
	}); err == nil {
		t.Fatal("a gated background call was dispatched rather than parked")
	}
	if n := atomic.LoadInt32(&f.resolves); n != 0 {
		t.Fatalf("a run parked for a human decision resolved %d environment value(s): the credential was in this process's memory while a person was thinking", n)
	}
	if _, err := os.Stat(f.pidFile); err == nil {
		t.Fatal("the parked call started a process")
	}

	// NON-VACUITY: the identical call with the gate OFF resolves exactly one value — so the zero above is
	// the park, not a fixture in which no value was ever resolvable.
	f.orch.tools = gatedShellBroker(false)
	if _, err := f.dispatch(t, true, "palai.workspace.shell", map[string]any{
		"argv": []any{"echo $$ > " + f.pidFile + "; sleep 30"}, "shell": true, "background": true,
	}); err != nil {
		t.Fatalf("the ungated background call failed: %v", err)
	}
	t.Cleanup(func() {
		if pgid, perr := readPgid(f.pidFile); perr == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	})
	if n := atomic.LoadInt32(&f.resolves); n != 1 {
		t.Fatalf("the ungated background call resolved %d environment value(s), want exactly 1", n)
	}
}

// gatedShellBroker registers the SHIPPED tools with palai.workspace.shell declared approval_required or
// not. It is how a deployment declares a gate, and building the broker rather than reaching past it is
// what makes the park above the shipped gate rather than a branch this file wrote.
func gatedShellBroker(gated bool) *toolbroker.Broker {
	shell := tools.ShellTool()
	shell.RequiresApproval = gated
	return toolbroker.New(shell, tools.FileTool(), tools.BackgroundKillTool())
}
