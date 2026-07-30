// This file adds the ENVIRONMENT surface to the identity package (E25 T3, migration 000046): a named
// group of key→value pairs an agent's shell receives.
//
// IT LIVES ON *SecretStore, AND THAT IS THE DESIGN RATHER THAN A CONVENIENCE. An environment value IS a
// secret_refs version under a derived name (`env:<environment_id>:<key>`) — there is no second table, no
// second cipher, no second master key. Hanging these methods off the store that already owns seal/open
// and the append-only write path makes the reuse STRUCTURAL: PutEnvironmentValue cannot accidentally
// invent an encryption path because it does not have one to invent, it calls insertVersion.
//
// A PLAN DEVIATION, WITH THE REASON. §1.5 files this as apps/control-plane/internal/store/environments.go.
// It is here instead because internal/store has no access to the master key or the seal, so putting it
// there would have meant either exporting the cipher (a second caller of a primitive that has exactly one)
// or writing a second sealing path — which is the ONE thing §1.4 spends four paragraphs forbidding.
//
// WHAT NO METHOD HERE RETURNS: A VALUE. environmentView and environmentKeyView carry names, versions and
// times. There is no read-back path, because there is no query that reads one (guarded by
// storage/ciphertext_sweep_test.go, not asserted here).
package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/storage"
)

// environmentView is an environment's METADATA projection: what it is called, what it is for, when it was
// made, and HOW MANY keys it holds. Keys is populated only by the detail read (GetEnvironment); the list
// carries the count so twenty environments do not drag eighty key names behind them.
//
// THIS STRUCT AND ITS SIBLING BELOW HAVE THEIR FIELD SETS PINNED BY A TEST (environments_view_test.go),
// for the same reason secretRefView's invariant is written down: the load-bearing property is a field
// that is NOT here. Adding `Value` — the single most natural thing for someone building a console screen
// to reach for — is red before it is reviewed.
type environmentView struct {
	ID          string               `json:"id"`
	Object      string               `json:"object"`
	Name        string               `json:"name"`
	Description string               `json:"description"`
	KeyCount    int                  `json:"key_count"`
	CreatedAt   *time.Time           `json:"created_at,omitempty"`
	Keys        []environmentKeyView `json:"keys,omitempty"`
}

// environmentKeyView is one key's METADATA: its NAME, the version its value is at, and when that version
// was written. A rotation moves Version and UpdatedAt; nothing here ever moves toward the value.
type environmentKeyView struct {
	Key       string     `json:"key"`
	Object    string     `json:"object"`
	Version   int        `json:"version"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// CreateEnvironment registers a named environment with no keys. The empty-keys start is deliberate and is
// half the reason environments is a table at all (migration 000046's header): the operator's flow is
// create the group, then fill it.
func (s *SecretStore) CreateEnvironment(ctx context.Context, scope middleware.Scope, body []byte) (api.ProvisionResult, error) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if strings.TrimSpace(in.Name) == "" {
		return api.ProvisionResult{MissingField: "name"}, nil
	}
	ctx = orgScope(ctx, scope)
	id := middleware.NewID("env")
	var createdAt time.Time
	err := s.pool.QueryRow(ctx, storage.Query("InsertEnvironment"), id, scope.Organization, in.Name, in.Description).Scan(&createdAt)
	if isUniqueViolation(err) {
		// A duplicate NAME, not a duplicate id. Reported as a conflict rather than silently returning the
		// existing row: "create production" answered with someone else's production environment is how an
		// operator writes a credential into a group they did not mean.
		return api.ProvisionResult{Conflict: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("insert environment: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(environmentView{
		ID: id, Object: "environment", Name: in.Name, Description: in.Description, CreatedAt: &createdAt,
	})}, nil
}

// ListEnvironments lists the caller's organization's environments with their key COUNTS.
func (s *SecretStore) ListEnvironments(ctx context.Context, scope middleware.Scope) (api.ProvisionResult, error) {
	ctx = orgScope(ctx, scope)
	rows, err := s.pool.Query(ctx, storage.Query("ListEnvironments"))
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("list environments: %w", err)
	}
	defer rows.Close()
	data := []environmentView{}
	for rows.Next() {
		v := environmentView{Object: "environment"}
		var createdAt time.Time
		if err := rows.Scan(&v.ID, &v.Name, &v.Description, &createdAt, &v.KeyCount); err != nil {
			return api.ProvisionResult{}, fmt.Errorf("scan environment: %w", err)
		}
		v.CreatedAt = &createdAt
		data = append(data, v)
	}
	if err := rows.Err(); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("iterate environments: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(listView{Object: "list", Data: data})}, nil
}

// GetEnvironment reads one environment with its KEY NAMES, their versions and their update times. An
// absent or foreign id is a miss (404) — RLS makes the two indistinguishable, which is the intended
// answer: telling an outsider that an id exists somewhere else is itself a disclosure.
//
// THE VALUES ARE NOT HERE AND THERE IS NO PARAMETER THAT WOULD ADD THEM.
func (s *SecretStore) GetEnvironment(ctx context.Context, scope middleware.Scope, id string) (api.ProvisionResult, error) {
	ctx = orgScope(ctx, scope)
	v := environmentView{Object: "environment"}
	var createdAt time.Time
	err := s.pool.QueryRow(ctx, storage.Query("GetEnvironment"), id).Scan(&v.ID, &v.Name, &v.Description, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ProvisionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("get environment: %w", err)
	}
	v.CreatedAt = &createdAt

	rows, err := s.pool.Query(ctx, storage.Query("ListEnvironmentKeys"), id)
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("list environment keys: %w", err)
	}
	defer rows.Close()
	v.Keys = []environmentKeyView{}
	for rows.Next() {
		k := environmentKeyView{Object: "environment_key"}
		var updatedAt *time.Time
		if err := rows.Scan(&k.Key, &k.Version, &updatedAt); err != nil {
			return api.ProvisionResult{}, fmt.Errorf("scan environment key: %w", err)
		}
		k.UpdatedAt = updatedAt
		v.Keys = append(v.Keys, k)
	}
	if err := rows.Err(); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("iterate environment keys: %w", err)
	}
	v.KeyCount = len(v.Keys)
	return api.ProvisionResult{Body: mustJSON(v)}, nil
}

// PutEnvironmentValue is CREATE-OR-ROTATE, in ONE transaction: the membership row and the sealed
// secret_refs version land together or neither lands.
//
// THE ATOMICITY IS NOT TIDINESS. The two halves fail in opposite directions. A membership row with no
// secret version is a key the console lists and the agent's shell never receives — a run that reads
// `printenv JIRA_TOKEN` gets nothing and calls Jira anonymously. A secret version with no membership row
// is a sealed credential nothing names, unreachable and undeletable (secret_refs grants no DELETE), and
// invisible to the operator who just typed it.
//
// CREATE AND ROTATE ARE THE SAME OPERATION, and that follows from secret_refs being append-only: there is
// no in-place update to distinguish a first write from a later one. The console says so in those words,
// because the alternative — a separate "rotate" route that 404s a new key — would make the first write
// and every later write two different mental models for one table.
func (s *SecretStore) PutEnvironmentValue(ctx context.Context, scope middleware.Scope, id string, body []byte) (api.ProvisionResult, error) {
	var in struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.Key == "" {
		return api.ProvisionResult{MissingField: "key"}, nil
	}
	if in.Value == "" {
		return api.ProvisionResult{MissingField: "value"}, nil
	}
	// KEY VALIDATION, PLACE ONE OF TWO. The same function the sandbox adaptors call at exec time
	// (toolbroker.ValidEnvKey), so the route and the executor cannot disagree about what a key may be
	// called. The exec-time check is the load-bearing one — this one exists so an operator finds out at
	// the moment they type it rather than at the moment a run fails.
	//
	// The COLLISION half of the rule is deliberately NOT checked here: which names a sandbox reserves is
	// a property of the posture (PATH/LANG/DEVELOPER_DIR/HOME/TMPDIR/PALAI_SIMCTL_SET on a Mac, HOME/PATH
	// in a container), a deployment can change posture without touching this table, and a list copied
	// into a write path is a list that rots. The executor derives it from the environment it actually
	// built plus the names it claims, and refuses there. So `PATH` reaches the DB and never reaches a
	// process — the honest order, because the second check is the one that cannot be reached around.
	if err := toolbroker.ValidEnvKey(in.Key); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}

	ctx = orgScope(ctx, scope)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("begin put environment value: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// The environment must exist IN THIS TENANT. Read inside the transaction and under the org scope, so
	// a foreign id is invisible (RLS) and reported as a 404 rather than as a foreign-key error naming
	// another tenant's table state.
	if err := tx.QueryRow(ctx, storage.Query("EnvironmentExists"), id).Scan(new(int)); errors.Is(err, pgx.ErrNoRows) {
		return api.ProvisionResult{NotFound: true}, nil
	} else if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("verify environment: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("UpsertEnvironmentValue"), id, scope.Organization, in.Key); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("record environment key: %w", err)
	}
	// requireExisting=false: a first write and a rotation are the same call, so there is no name for
	// which this should 404.
	version, createdAt, err := s.insertVersion(ctx, tx, scope.Organization, environmentSecretName(id, in.Key), in.Value, false)
	if err != nil {
		return api.ProvisionResult{}, err
	}
	// The commit is HERE, in the function that owns the transaction. Returning insertVersion's result
	// directly would discard both writes under the deferred rollback.
	if err := tx.Commit(ctx); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("commit put environment value: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(environmentKeyView{
		Key: in.Key, Object: "environment_key", Version: version, UpdatedAt: &createdAt,
	})}, nil
}

// DeleteEnvironmentValue removes the BINDING between an environment and a key. IT DOES NOT DELETE THE
// VALUE, and the API says so in the body it returns rather than only in a doc comment, because an
// operator who believes a credential is gone will not rotate it.
//
// The bytes stay as secret_refs versions: that table grants no DELETE and re-asserts the REVOKE on every
// boot (000031:45), on purpose — version history is retained for audit. What removal achieves is real
// and worth stating precisely: nothing names those versions afterwards (the derived name is only ever
// built from a membership row), and no run receives the key.
func (s *SecretStore) DeleteEnvironmentValue(ctx context.Context, scope middleware.Scope, id, key string) (api.ProvisionResult, error) {
	ctx = orgScope(ctx, scope)
	var removed string
	err := s.pool.QueryRow(ctx, storage.Query("DeleteEnvironmentValue"), id, key).Scan(&removed)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.ProvisionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("delete environment value: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(map[string]any{
		"key":     removed,
		"object":  "environment_key",
		"removed": true,
		"note": "the binding was removed; the stored value's versions are retained and unreachable " +
			"(secret_refs is append-only for audit). Nothing names them and no run receives this key.",
	})}, nil
}

// isUniqueViolation reports whether err is a Postgres 23505 unique_violation — here, a second
// environment claiming a name already taken in this organization. The third copy of this three-line
// predicate in the tree (internal/extensions/registry.go, internal/automation/deliver.go); it stays local
// rather than becoming a shared helper, because a shared one would need a home package that every store
// imports and none of them currently does.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// environmentSecretName derives the secret_refs name an environment key's value is stored under. The
// SAME construction is in storage/queries/environments.sql (RunEnvironmentKeys), which is where the
// orchestrator gets it — so the resolve path never rebuilds this string in Go.
func environmentSecretName(environmentID, key string) string {
	return "env:" + environmentID + ":" + key
}
