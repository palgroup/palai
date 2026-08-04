package store

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/palgroup/palai/apps/control-plane/internal/identity"

	"github.com/palgroup/palai/storage"
)

// Bootstrap seeds ONLY the first tenant and its admin API key when the deployment has no keys yet, and
// does it through the very same provisioning path the API uses for every later tenant
// (internal/identity) — so a SECOND tenant is opened purely over POST /v1/projects, with no restart and no
// manual SQL. It said "the first organization … over /v1/organizations" until A.2 Task 6 dropped the
// table and unmounted that route; identity.ProvisionFirstTenant is the path this calls, and the rows it
// seeds are a project, a service principal, an admin key and a default runner pool. Only the key's hash is stored (identity/coordinator.HashAPIKey); the bearer value read from
// PALAI_BOOTSTRAP_API_KEY_FILE never reaches the database. It is idempotent: a stack whose api_keys already
// hold a row (a retained volume, a re-boot) is left untouched, and an empty key is a no-op. This is the
// server half of LP-001's "documented command, no manual SQL".
func (s *Store) Bootstrap(ctx context.Context, bootstrapKey string) error {
	if strings.TrimSpace(bootstrapKey) == "" {
		return nil
	}
	// The count read establishes whether a tenant exists yet; it runs under the system scope for the same
	// reason VerifyAPIKey does — there is no tenant to scope to before the first one is seeded.
	var count int
	if err := s.spine.Pool().QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM api_keys`).Scan(&count); err != nil {
		return fmt.Errorf("count api keys: %w", err)
	}
	if count > 0 {
		return s.reconcileBootstrapScopes(ctx)
	}
	return identity.New(s.spine.Pool()).ProvisionFirstTenant(ctx, bootstrapKey)
}

// reconcileBootstrapScopes gives an ALREADY-BOOTSTRAPPED installation the scope set a fresh one is now
// born with. Without it those installations have no way to reach the platform surfaces at all.
//
// WHAT HAPPENED. ProvisionFirstTenant seeded `[]string{}` until 4155709a, which changed it to
// {system, provision, approve}. An empty set is unrestricted for every ORDINARY capability, so that was
// enough — until `system` was carved out of the empty-set rule precisely so a tenant admin could not
// inherit the platform. From that commit on, a fresh install's operator holds `system` explicitly and an
// EXISTING install's operator holds nothing that satisfies HasSystem. Bootstrap returned early on
// `count > 0`, and no statement anywhere updated a key's scopes, so the gap had no closing path.
//
// It was survivable only through a hole: POST /v1/api-keys stored caller-supplied scopes verbatim, so the
// operator could mint themselves a `system` key. Closing that (Scope.CanGrant) removed the workaround and
// left those installations locked out — which is why this lands with it rather than after it.
//
// THE TRIGGER IS EXACTLY EMPTY, and that precision is the safety argument: empty is not a set anybody
// chose, it is the only shape the old code could produce. A key whose scopes are non-empty was narrowed
// deliberately — by an operator or by CreateAPIKey — and is never touched here. Nor is any key but
// `key_local`: this repairs the installation's own bootstrap credential, not every key on it.
//
// IT IS NOT PURELY A WIDENING AND THE COMMENT SAYS SO. Empty means "every ordinary capability", so moving
// to the explicit triple ADDS `system` and NARROWS the ordinary half to {provision, approve}. Those are
// the only two ordinary capabilities anything in this tree gates on, so nothing reachable changes today —
// and the result is byte-identical to what a fresh install gets, which is the property worth having:
// after this runs, an upgraded stack and a new one are in the same state.
func (s *Store) reconcileBootstrapScopes(ctx context.Context) error {
	tag, err := s.spine.Pool().Exec(storage.WithSystemScope(ctx),
		`UPDATE api_keys SET scopes = $1 WHERE id = 'key_local' AND cardinality(scopes) = 0 AND revoked_at IS NULL`,
		identity.FirstTenantScopes())
	if err != nil {
		return fmt.Errorf("reconcile bootstrap key scopes: %w", err)
	}
	if tag.RowsAffected() > 0 {
		log.Printf("bootstrap: key_local carried no scopes (pre-4155709a seed); granted %v so this "+
			"installation can reach the platform surfaces a fresh install can", identity.FirstTenantScopes())
	}
	return nil
}
