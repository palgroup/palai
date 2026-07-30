package execution

import (
	"context"
	"fmt"

	"github.com/palgroup/palai/storage"
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
	ctx = storage.ScopeToTenant(ctx, st.tenant.Organization, st.tenant.Project)
	rows, err := o.spine.Pool().Query(ctx, storage.Query("RunEnvironmentKeys"), string(st.attempt.RunID))
	if err != nil {
		return nil, fmt.Errorf("read run environment keys: %w", err)
	}
	defer rows.Close()
	var keys []envKey
	for rows.Next() {
		var k envKey
		if err := rows.Scan(&k.Key, &k.SecretName); err != nil {
			return nil, fmt.Errorf("scan run environment key: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run environment keys: %w", err)
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
		value, err := o.envSecrets(st.tenant.Organization, k.SecretName)
		if err != nil {
			// The KEY is named; the value is not, and neither is the derived secret name (it embeds the
			// environment id, which is fine, but the wrapped error from the resolver may not be).
			return nil, fmt.Errorf("resolve environment key %q for run %s: %w", k.Key, st.attempt.RunID, err)
		}
		values[k.Key] = string(value)
	}
	return values, nil
}
