package execution

import (
	"context"
	"fmt"

	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// THE CLAIM — the capability-worker secret-handle pattern applied to a RUN (E25 T3, plan §T3).
//
// internal/workers/store.go's RedeemSecretHandle guards a worker's credential with three things: a FENCE
// (the job's own generation), a SCOPE (the secret name read from the append-only journal, never from the
// worker's claim), and an EXPIRY (a deadline the redeem must precede). A run needs the same guarantees and
// gets them from a different mechanism, which is why this file is short:
//
//   - SCOPE. The key NAMES come from the run's PINNED revision, read from the database at attempt start
//     (RunEnvironmentKeys). Nothing the engine, the model or a tool says can add a name to that set — the
//     analogue of "read from the journal, not from the claim".
//   - EXPIRY AND FENCE. The ATTEMPT is both. A value exists in this process between resolveEnvValues and
//     the Execute call it is handed to, and nowhere else: it is not on attemptState, not on the
//     Orchestrator, not in a struct that outlives the call. There is no handle to redeem later because
//     there is nothing later.
//
// AND SINCE E26 T6 THE SECOND BULLET IS TRUE OF THIS PROCESS ONLY. A BACKGROUND task is handed the same
// map and then OUTLIVES the Execute call that handed it over (§0.4, §3.6 D9): the value sits in the
// kernel's environ copy for the whole life of that process, and killing the control plane does not take
// it back. The three guarantees are therefore restated rather than repeated for it —
//
//	SCOPE  — unchanged; the names still come from the pinned revision and nothing a model says adds one.
//	EXPIRY — the TASK's deadline_at, not the attempt (§0.2). A task carrying a key cannot be created
//	         without one, which is StartBackground's gate: unbounded exposure is what a missing ceiling
//	         would mean.
//	FENCE  — the task ROW. It is killable by id, killed by its run's cancellation, and ended by the
//	         reaper; the value's life ends when the process does, and the row is what ends the process.
//
// — and one more obligation appears that a synchronous call does not have: the bytes the process writes
// to its OWN log are not the captured Go string the executors redact, so redaction moves to the READ
// path. redactTaskOutput below is the whole of it and both readers call it.
//
// WHAT IS DELIBERATELY SEPARATE FROM THE CONFIG RESOLVER. An environment does NOT flow through
// ResolveInput / Resolve / ConfigSnapshot. It could have — PinnedExecConfig already reads the same revision
// row — and it must not: ConfigSnapshot is content-HASHED, journaled on config.revised, and written into
// every checkpoint. Its own comment (config.go) says "SecretRef stays a reference — the credential value
// never enters the snapshot", and the cheapest way to keep that true for environments was to give them no
// path into the snapshot at all. So this is a separate read with a separate query, and config_test.go's
// snapshot pins are untouched by construction.

// envKey is one environment key of a run: the NAME the shell will see and the secret_refs name its value
// is stored under. Both come from the database; the derived name is built by the SQL (RunEnvironmentKeys)
// so no Go code concatenates it at resolve time.
type envKey struct {
	Key        string
	SecretName string
}

// SetEnvironmentSecrets wires the resolver an environment VALUE is read through (E25 T3). Nil means no
// environment value can be resolved, and an attempt whose revision names an environment then FAILS its
// tool call rather than running without the credential — the fail-closed direction, for the reason
// repositoryConnectionSecret gives: a `gh` or `curl` that runs without the token it was supposed to have
// succeeds anonymously often enough to look like a working run.
//
// main.go wires it from the same DB-backed store the four other secret resolvers front.
func (o *Orchestrator) SetEnvironmentSecrets(secrets SecretResolver) {
	o.envSecrets = secrets
}

// resolveEnvKeys reads the run's environment key NAMES at attempt start. NAMES ONLY — no value is read
// here, and this is the only place the set is decided.
//
// A run with no pinned revision, a revision with no environment, or an environment with no keys all return
// nil, which is every run in every deployment before E25: the attempt then carries no environment and a
// shell command's environment is bit-identical to what it was.
func (o *Orchestrator) resolveEnvKeys(ctx context.Context, st *attemptState) ([]envKey, error) {
	return o.runEnvKeys(ctx, st.tenant, string(st.attempt.RunID))
}

// runEnvKeys is resolveEnvKeys without an attempt, because the READ path has no attempt: the reconciler
// sweep that quotes a finished task's log into a notification runs on a loop that never saw the run, and
// may be in a control plane that never saw the spawn. One query, one caller shape, two callers.
func (o *Orchestrator) runEnvKeys(ctx context.Context, tenant coordinator.Tenant, runID string) ([]envKey, error) {
	names, secretNames, err := o.spine.RunEnvironmentKeys(ctx, tenant, runID)
	if err != nil {
		return nil, err
	}
	keys := make([]envKey, len(names))
	for i := range names {
		keys[i] = envKey{Key: names[i], SecretName: secretNames[i]}
	}
	return keys, nil
}

// resolveEnvValues turns this attempt's key names into values, and is called IMMEDIATELY before the exec
// call — after the durable consult, after the approval gate, after the before_tool hook. That ordering is
// the whole of the expiry guarantee: a run parked waiting for a human to approve a tool holds no
// credential in memory, because the park returns from dispatchTool long before this runs.
//
// A MISS IS AN ERROR, NOT AN EMPTY VALUE. The write path lands the membership row and the secret version
// in one transaction, so a named key with no resolvable value means the database was edited by hand or the
// master key changed. Handing the shell an empty string there would run the command anonymously and report
// success.
//
// The returned map is handed to Execute and dropped. It is never stored on the orchestrator or the attempt
// state, never marshalled, and never logged — the error below names the KEY, which is safe, and never the
// value, which never is.
func (o *Orchestrator) resolveEnvValues(ctx context.Context, st *attemptState) (map[string]string, error) {
	if len(st.envKeys) == 0 {
		return nil, nil
	}
	if o.envSecrets == nil {
		return nil, fmt.Errorf("this run's revision names an environment with %d key(s) but no secret resolver is wired: "+
			"refusing to run a command without the credentials it was configured with", len(st.envKeys))
	}
	values := make(map[string]string, len(st.envKeys))
	for _, k := range st.envKeys {
		value, err := o.envSecrets(st.tenant, k.SecretName)
		if err != nil {
			// The KEY is named; the value is not, and neither is the derived secret name (it embeds the
			// environment id, which is fine, but the wrapped error from the resolver may not be).
			return nil, fmt.Errorf("resolve environment key %q for run %s: %w", k.Key, st.attempt.RunID, err)
		}
		values[k.Key] = string(value)
	}
	return values, nil
}

// redactTaskOutput is THE ONE PLACE bytes a BACKGROUND PROCESS wrote to a file are masked before a model
// sees them (E26 T6, §3.6 D8), and BOTH of the two places those bytes surface call exactly this function:
//
//	palai.workspace.file's read  — through ExecEnv.Background.RedactOutput (tools/file.go)
//	the exit notice's 2 KiB tail — through backgroundTail (background.go)
//
// TWO REDACTION POINTS WOULD BE TWO CHANCES FOR THEM TO DIVERGE, which is the argument T5 used to collapse
// two setters into one, and it applies harder here: the two sites have different callers, different
// lifetimes and different tests, so a shape added to one and not the other would be a leak nobody looked
// for. One function, both callers, one test asserting both in the same run.
//
// BOTH REDACTORS RUN AND NEITHER SUBSTITUTES FOR THE OTHER. RedactSecrets is SHAPE-based and cannot see
// an environment value — a Jira token, a database password, an internal base URL match none of its four
// patterns. RedactValues is VALUE-based and cannot see a token the build read out of a file we never
// handed it. A build log carries both kinds, so the read path applies both.
//
// THE VALUES ARE RE-RESOLVED HERE, AT READ TIME, and that is forced rather than chosen: the row holds KEY
// NAMES and never a value (migration 000047), the reconciler has no attempt to borrow a resolved map from,
// and the whole reason the column exists is that a later reader has to be able to rebuild the set. This is
// E25 T3's "there is no handle to redeem" being rewritten for the first time — for a background task there
// IS one, and this is it.
//
// `carried` IS THE ROW'S OWN env_keys AND IT IS A FAIL-CLOSED CHECK, not a filter. A key the task carried
// that no longer resolves — an environment value deleted while the log sat on disk — means we cannot mask
// what we know is in those bytes, so the caller is given an error and serves a note instead of the output.
// The masking itself uses every value the run resolves NOW, which is a superset and therefore never masks
// less than the task's own set.
//
// THE COST OF RE-RESOLVING, STATED: this puts the run's credential values in the RECONCILER's memory for
// the length of one masking pass, on a loop that never held one before. It is the narrowest form of the
// same exposure resolveEnvValues already accepts — a local slice, not stored, not logged, not returned —
// and the alternative is worse in the direction that matters, because it is serving the value to a model.
//
// HONEST CEILING, and it is the same one RedactValues states about itself: this is literal substring
// matching. A build that base64s a value, prints it one character per line or splits it across two commands
// defeats it, and nothing here can prevent that — giving an agent a secret IS the agent having it. What
// this is real against is the leak that happens BY DEFAULT: a build log that echoes its own environment.
//
// A CALLER WITH NO ENVIRONMENT PAYS ONE QUERY AND GETS SHAPE REDACTION, which is every run in every
// deployment before E25. That is a widening of what palai.workspace.file used to return and it is
// deliberate: §28.8's rule is that captured output is masked, and a file read was the one output path that
// never was.
func (o *Orchestrator) redactTaskOutput(ctx context.Context, tenant coordinator.Tenant, runID string, carried []string, s string) (string, error) {
	byName, err := o.runEnvValuesByName(ctx, tenant, runID)
	if err != nil {
		return "", err
	}
	for _, key := range carried {
		if _, ok := byName[key]; !ok {
			return "", fmt.Errorf("this background task carried environment key %q and its value can no longer be resolved: "+
				"refusing to serve output that may contain it unmasked", key)
		}
	}
	// EnvValueList rather than a range loop, for the reason it was written: `for k := range m` is one
	// character away from `for _, v := range m`, and a redactor fed KEY NAMES masks nothing and reports
	// success. Both executors already take the values through it.
	return toolbroker.RedactValues(toolbroker.RedactSecrets(s), toolbroker.EnvValueList(byName)), nil
}

// runEnvValuesByName is resolveEnvValues for a run rather than for an attempt. It fails closed on a miss
// for resolveEnvValues' reason, in the read direction: a value that cannot be resolved cannot be masked,
// and returning the bytes anyway would put the credential on the model's screen.
func (o *Orchestrator) runEnvValuesByName(ctx context.Context, tenant coordinator.Tenant, runID string) (map[string]string, error) {
	keys, err := o.runEnvKeys(ctx, tenant, runID)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}
	if o.envSecrets == nil {
		return nil, fmt.Errorf("run %s names an environment with %d key(s) but no secret resolver is wired: "+
			"refusing to serve a background task's output that cannot be masked", runID, len(keys))
	}
	values := make(map[string]string, len(keys))
	for _, k := range keys {
		value, err := o.envSecrets(tenant, k.SecretName)
		if err != nil {
			// The KEY is named and the value never is, exactly as in resolveEnvValues.
			return nil, fmt.Errorf("re-resolve environment key %q for run %s: %w", k.Key, runID, err)
		}
		values[k.Key] = string(value)
	}
	return values, nil
}
