package store

import (
	"context"
	"fmt"
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
		return nil
	}
	return identity.New(s.spine.Pool()).ProvisionFirstTenant(ctx, bootstrapKey)
}
