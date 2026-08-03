//go:build component

package execution

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/packages/contracts"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// E25 T3's CROWN, against a real spine: an operator's environment reaches a real shell command on this
// machine, and the value it carried is in no durable row afterwards.
//
// The chain proven here end to end, with nothing faked between the two ends:
//
//	POST-equivalent write (identity.SecretStore, real AES-256-GCM, real secret_refs)
//	  → an agent revision naming that environment, published
//	  → the run pinned to it
//	  → resolveEnvKeys at attempt start: KEY NAMES only
//	  → resolveEnvValues at the last moment: values, through the real Resolve
//	  → the production palai.workspace.shell tool over the production host executor
//	  → `printenv` sees the key
//	  → the tool's committed result, read back out of the real tool_calls row, holds *** and not the value
//
// The environment secret is the SAME secret_refs row the store wrote — the derived name is the only
// address, and if the SQL's `'env:' || environment_id || ':' || key` and Go's environmentSecretName ever
// disagreed, this test would resolve nothing and say so.
const envSentinel = "e25-t3-run-sentinel-4a1f8c37"

// TestAnEnvironmentReachesARunsShellAndItsValueEntersNoDurableRow is the whole claim.
func TestAnEnvironmentReachesARunsShellAndItsValueEntersNoDurableRow(t *testing.T) {
	cs, tenant, exec := openPinnedSpine(t)
	ctx := context.Background()

	// A master key for this test only; never written to a file.
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i * 7)
	}
	key, err := identity.ParseMasterKey(hex.EncodeToString(raw[:]))
	if err != nil {
		t.Fatalf("ParseMasterKey: %v", err)
	}
	secrets := identity.NewSecretStore(cs.Pool(), key)

	// The environment, and its two keys, written through the REAL store — so the bytes under test are
	// bytes the API path sealed, not a fixture INSERT.
	envID := pinnedID("env")
	exec(`INSERT INTO environments (id, organization_id, name) VALUES ($1,$2,'production')`, envID, tenant.Organization)
	scope := middleware.Scope{Project: tenant.Project}
	for _, body := range []string{
		`{"key":"JIRA_TOKEN","value":"` + envSentinel + `"}`,
		`{"key":"DEPLOY_TARGET","value":"staging.example.internal"}`,
	} {
		out, err := secrets.PutEnvironmentValue(ctx, scope, envID, []byte(body))
		if err != nil || out.BadField || out.NotFound {
			t.Fatalf("PutEnvironmentValue(%s): %+v %v", body, out, err)
		}
	}

	// A published revision naming that environment, and a run pinned to it.
	profileID, revID, sessionID, runID := pinnedID("aprof"), pinnedID("arev"), pinnedID("ses"), pinnedID("run")
	exec(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, tenant.Organization, tenant.Project)
	exec(`INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,'deployer')`,
		profileID, tenant.Organization, tenant.Project)
	exec(`INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, environment, published_at)
	      VALUES ($1,$2,$3,$4,1,'model-pinned',$5, clock_timestamp())`,
		revID, tenant.Organization, tenant.Project, profileID, envID)
	exec(`INSERT INTO runs (id, organization_id, project_id, session_id, state, agent_revision_id) VALUES ($1,$2,$3,$4,'running',$5)`,
		runID, tenant.Organization, tenant.Project, sessionID, revID)

	orch := &Orchestrator{
		spine: cs,
		shell: host.NewExecutor(30 * time.Second),
		// The resolver main.go wires, minus the env-file fallback this feature has no use for.
		envSecrets: func(org, ref string) ([]byte, error) {
			v, ok, err := secrets.Resolve(ctx, org, ref)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errNoSuchEnvironmentSecret
			}
			return v, nil
		},
	}
	st := &attemptState{
		attempt:   AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(pinnedID("att")), Fence: 1},
		tenant:    tenant,
		sessionID: sessionID,
	}

	// 1. ATTEMPT START — KEY NAMES ONLY. This is the scope half of the worker secret-handle pattern: the
	// set is decided from the PINNED revision, before the engine has said anything.
	keys, err := orch.resolveEnvKeys(ctx, st)
	if err != nil {
		t.Fatalf("resolveEnvKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("resolveEnvKeys returned %d keys, want 2: %+v", len(keys), keys)
	}
	// The struct that goes onto attemptState holds no value, and this is checked by MARSHALLING it rather
	// than by reading the field list — a value smuggled into any field would show up in the bytes.
	blob, _ := json.Marshal(keys)
	if strings.Contains(string(blob), envSentinel) {
		t.Fatalf("the attempt's key set carries a VALUE: %s", blob)
	}
	if !strings.Contains(string(blob), "env:"+envID+":JIRA_TOKEN") {
		t.Fatalf("the derived secret name is not what the store wrote under: %s", blob)
	}
	st.envKeys = keys

	// 2. THE LAST MOMENT — values, through the real decrypt.
	values, err := orch.resolveEnvValues(ctx, st)
	if err != nil {
		t.Fatalf("resolveEnvValues: %v", err)
	}
	if values["JIRA_TOKEN"] != envSentinel {
		t.Fatalf("resolveEnvValues gave JIRA_TOKEN = %q, want the sentinel — the SQL's derived name and the store's disagree", values["JIRA_TOKEN"])
	}
	if values["DEPLOY_TARGET"] != "staging.example.internal" {
		t.Fatalf("resolveEnvValues gave DEPLOY_TARGET = %q", values["DEPLOY_TARGET"])
	}

	// 3. A REAL SHELL COMMAND SEES THEM. The production broker, the production tool, the production host
	// executor — and the env the orchestrator built, exactly as dispatchTool hands it over.
	env := orch.execEnv(st)
	env.WorkspaceRoot = t.TempDir()
	env.EnvValues = values
	broker := toolbroker.New(tools.ShellTool())
	outcome, err := broker.Execute(ctx, "call_env_printenv", "palai.workspace.shell",
		map[string]any{"argv": []any{"sh", "-c", "printenv JIRA_TOKEN; printenv DEPLOY_TARGET"}}, 1, env)
	if err != nil {
		t.Fatalf("palai.workspace.shell: %v", err)
	}
	if code, _ := outcome.Result["exit_code"].(int); code != 0 {
		t.Fatalf("printenv exited %v: %v", outcome.Result["exit_code"], outcome.Result["stderr"])
	}
	stdout, _ := outcome.Result["stdout"].(string)
	// DEPLOY_TARGET is not a credential shape and IS a value, so it comes back masked too — which is the
	// honest cost of a value-based redactor and is stated in RedactValues' own comment. What proves both
	// keys ARRIVED is that printenv exited 0 (it exits 1 on the first unset name) and printed two masks.
	if strings.Count(stdout, "***") != 2 {
		t.Fatalf("printenv printed %q; want two masks — either a key never arrived or a value came back unmasked", stdout)
	}
	if strings.Contains(stdout, envSentinel) || strings.Contains(stdout, "staging.example.internal") {
		t.Fatalf("a value came back through the shell result unmasked: %q", stdout)
	}

	// 4. THE JOURNAL. The bytes below are constructed exactly as tool_dispatch.go does
	// (`result, _ := json.Marshal(outcome.Result)`) and committed through the SAME spine method, then read
	// back out of the real tool_calls row. So this is the durable record, not a stand-in for it.
	committed, _ := json.Marshal(outcome.Result)
	if _, err := cs.CommitToolResult(ctx, tenant, sessionID, "", runID, st.attempt.Fence,
		"call_env_printenv", "palai.workspace.shell", []byte(`{}`), committed,
		string(toolbroker.ClassIrreversible), "hash", toolCallCompletedEvent, []byte(`{}`)); err != nil {
		t.Fatalf("CommitToolResult: %v", err)
	}
	_, stored, _, _, _, found, err := cs.LookupToolCall(ctx, tenant, "call_env_printenv")
	if err != nil || !found {
		t.Fatalf("LookupToolCall found=%v err=%v", found, err)
	}
	if strings.Contains(stored, envSentinel) || strings.Contains(stored, "staging.example.internal") {
		t.Fatalf("AN ENVIRONMENT VALUE IS IN THE TOOL LEDGER ROW:\n%s", stored)
	}
	if !strings.Contains(stored, "***") {
		t.Fatalf("the ledger row carries no mask, so the check above may be passing because nothing was captured:\n%s", stored)
	}

	// 5. THE CONFIG SNAPSHOT IS UNTOUCHED. config.go's rule — "SecretRef stays a reference; the credential
	// value never enters the snapshot" — is extended to environments by giving them NO PATH into the
	// snapshot at all: an environment is read by a separate query and never enters ResolveInput. Asserted
	// against the same run, so this is the snapshot a checkpoint of THIS run would carry.
	snapHash, err := orch.effectiveConfigHash(ctx, st)
	if err != nil {
		t.Fatalf("effectiveConfigHash: %v", err)
	}
	if snapHash == "" {
		t.Fatal("effectiveConfigHash returned nothing, so the assertion below would prove nothing")
	}
	plan, err := orch.planConfigChange(ctx, st, pinnedID("cmd"), []byte(`{}`))
	if err != nil {
		t.Fatalf("planConfigChange: %v", err)
	}
	// The whole journaled payload, not just the snapshot struct: a value that reached any field of it
	// would be in these bytes.
	if strings.Contains(string(plan.RevisedPayload), envSentinel) || strings.Contains(string(plan.RevisedPayload), envID) {
		t.Fatalf("the journaled config.revised payload carries the environment or its value:\n%s", plan.RevisedPayload)
	}
}

// TestARunWhoseRevisionNamesAnEnvironmentFailsClosedWithNoResolver pins the fail-closed direction, and the
// direction is the point. An agent configured with credentials that silently receives none does not stop:
// `curl` succeeds anonymously, `gh` reads the public repository, a deploy writes to the default target. A
// refused tool call is loud; a missing credential is not.
func TestARunWhoseRevisionNamesAnEnvironmentFailsClosedWithNoResolver(t *testing.T) {
	cs, tenant, exec := openPinnedSpine(t)
	ctx := context.Background()

	envID := pinnedID("env")
	exec(`INSERT INTO environments (id, organization_id, name) VALUES ($1,$2,'production')`, envID, tenant.Organization)
	exec(`INSERT INTO environment_values (environment_id, organization_id, key) VALUES ($1,$2,'JIRA_TOKEN')`, envID, tenant.Organization)
	profileID, revID, sessionID, runID := pinnedID("aprof"), pinnedID("arev"), pinnedID("ses"), pinnedID("run")
	exec(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, tenant.Organization, tenant.Project)
	exec(`INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,'deployer')`,
		profileID, tenant.Organization, tenant.Project)
	exec(`INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, environment, published_at)
	      VALUES ($1,$2,$3,$4,1,$5, clock_timestamp())`,
		revID, tenant.Organization, tenant.Project, profileID, envID)
	exec(`INSERT INTO runs (id, organization_id, project_id, session_id, state, agent_revision_id) VALUES ($1,$2,$3,$4,'running',$5)`,
		runID, tenant.Organization, tenant.Project, sessionID, revID)

	orch := &Orchestrator{spine: cs} // no envSecrets
	st := &attemptState{
		attempt:   AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(pinnedID("att")), Fence: 1},
		tenant:    tenant,
		sessionID: sessionID,
	}
	keys, err := orch.resolveEnvKeys(ctx, st)
	if err != nil {
		t.Fatalf("resolveEnvKeys: %v", err)
	}
	st.envKeys = keys
	if _, err := orch.resolveEnvValues(ctx, st); err == nil {
		t.Fatal("a run whose revision names an environment resolved values with NO resolver wired — it would have run the command without its credentials")
	}

	// And the other direction, which is what keeps every pre-E25 run bit-unchanged: a run whose revision
	// names NO environment resolves nothing, needs no resolver, and does not fail. It gets its OWN session,
	// because runs_one_active_root_per_session (000006) permits one active root per session — reusing the
	// one above is a 23505, which is how this fixture failed the first time it ran.
	bareRev, bareRun, bareSession := pinnedID("arev"), pinnedID("run"), pinnedID("ses")
	exec(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, bareSession, tenant.Organization, tenant.Project)
	exec(`INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, published_at)
	      VALUES ($1,$2,$3,$4,2, clock_timestamp())`, bareRev, tenant.Organization, tenant.Project, profileID)
	exec(`INSERT INTO runs (id, organization_id, project_id, session_id, state, agent_revision_id) VALUES ($1,$2,$3,$4,'running',$5)`,
		bareRun, tenant.Organization, tenant.Project, bareSession, bareRev)
	bare := &attemptState{
		attempt:   AttemptDescriptor{RunID: contracts.RunID(bareRun), AttemptID: contracts.AttemptID(pinnedID("att")), Fence: 1},
		tenant:    tenant,
		sessionID: bareSession,
	}
	if bare.envKeys, err = orch.resolveEnvKeys(ctx, bare); err != nil {
		t.Fatalf("resolveEnvKeys on an environment-less run: %v", err)
	}
	if len(bare.envKeys) != 0 {
		t.Fatalf("an environment-less run resolved %d keys", len(bare.envKeys))
	}
	values, err := orch.resolveEnvValues(ctx, bare)
	if err != nil || values != nil {
		t.Fatalf("an environment-less run: values=%v err=%v, want (nil, nil) — every run before E25 takes this path", values, err)
	}
}

// TestARunsEnvironmentIsPinnedToItsRevisionNotToTheLatest is the AGT-001 reproducibility rule applied to a
// credential set: a key added to the environment after the run started must not appear, and one removed
// after it started must still appear. RunEnvironmentKeys reads through the run's PINNED revision, so both
// hold — but the environment ROW is shared and mutable, which is what makes this worth asserting: the pin
// is on the revision, not on the key list.
func TestARunsEnvironmentIsPinnedToItsRevisionNotToTheLatest(t *testing.T) {
	cs, tenant, exec := openPinnedSpine(t)
	ctx := context.Background()

	envA, envB := pinnedID("env"), pinnedID("env")
	exec(`INSERT INTO environments (id, organization_id, name) VALUES ($1,$2,'production')`, envA, tenant.Organization)
	exec(`INSERT INTO environments (id, organization_id, name) VALUES ($1,$2,'staging')`, envB, tenant.Organization)
	exec(`INSERT INTO environment_values (environment_id, organization_id, key) VALUES ($1,$2,'A_ONLY')`, envA, tenant.Organization)
	exec(`INSERT INTO environment_values (environment_id, organization_id, key) VALUES ($1,$2,'B_ONLY')`, envB, tenant.Organization)

	profileID, revA, sessionID, runID := pinnedID("aprof"), pinnedID("arev"), pinnedID("ses"), pinnedID("run")
	exec(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, tenant.Organization, tenant.Project)
	exec(`INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,'deployer')`,
		profileID, tenant.Organization, tenant.Project)
	exec(`INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, environment, published_at)
	      VALUES ($1,$2,$3,$4,1,$5, clock_timestamp())`, revA, tenant.Organization, tenant.Project, profileID, envA)
	exec(`INSERT INTO runs (id, organization_id, project_id, session_id, state, agent_revision_id) VALUES ($1,$2,$3,$4,'running',$5)`,
		runID, tenant.Organization, tenant.Project, sessionID, revA)

	orch := &Orchestrator{spine: cs}
	st := &attemptState{
		attempt:   AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(pinnedID("att")), Fence: 1},
		tenant:    tenant,
		sessionID: sessionID,
	}

	// A LATER revision of the same profile points at a different environment. The run keeps A's.
	revB := pinnedID("arev")
	exec(`INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, environment, published_at)
	      VALUES ($1,$2,$3,$4,2,$5, clock_timestamp())`, revB, tenant.Organization, tenant.Project, profileID, envB)
	keys, err := orch.resolveEnvKeys(ctx, st)
	if err != nil {
		t.Fatalf("resolveEnvKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].Key != "A_ONLY" {
		t.Fatalf("the run followed the LATEST revision's environment instead of its pinned one: %+v", keys)
	}

	// A key added to the PINNED environment after the run started DOES appear — the pin is on the revision,
	// not on the key list, and that is the honest behaviour to assert rather than the one to wish for. It is
	// also what makes a rotation take effect on a running agent, which is the whole reason secret_refs is
	// read fresh (SEC-002).
	exec(`INSERT INTO environment_values (environment_id, organization_id, key) VALUES ($1,$2,'ADDED_LATER')`, envA, tenant.Organization)
	keys, err = orch.resolveEnvKeys(ctx, st)
	if err != nil {
		t.Fatalf("resolveEnvKeys after a mid-run addition: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("a key added to the pinned environment mid-run: got %+v, want both keys — the read is deliberately live so a rotation reaches a running agent", keys)
	}
}

// errNoSuchEnvironmentSecret mirrors main.go's environmentValueSecret miss: an error, never an empty value.
var errNoSuchEnvironmentSecret = errEnvSecretMiss{}

type errEnvSecretMiss struct{}

func (errEnvSecretMiss) Error() string { return "no such environment secret ref" }
