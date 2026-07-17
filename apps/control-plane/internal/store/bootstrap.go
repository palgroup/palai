package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/palgroup/palai/packages/coordinator"
)

// Dev bootstrap identity: the single organization/project/principal a fresh local stack
// seeds so the documented CLI (`palai response create`) admits over a real API key
// without an operator ever running manual SQL (LP-001). The IDs are stable so a re-boot
// against a retained volume is a no-op.
const (
	bootstrapOrg       = "org_local"
	bootstrapProject   = "prj_local"
	bootstrapPrincipal = "prin_local"
	bootstrapKeyID     = "key_local"
)

// Bootstrap seeds the dev tenant and its API key when the deployment has no keys yet.
// Only the key's hash is stored (coordinator.HashAPIKey); the bearer value read from
// PALAI_BOOTSTRAP_API_KEY_FILE never reaches the database. It is idempotent: a stack whose
// api_keys already hold a row (a retained volume, a re-boot) is left untouched, and an
// empty key is a no-op (nothing to seed). This is the server half of LP-001's "documented
// command, no manual SQL".
func (s *Store) Bootstrap(ctx context.Context, bootstrapKey string) error {
	if strings.TrimSpace(bootstrapKey) == "" {
		return nil
	}
	pool := s.spine.Pool()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_keys`).Scan(&count); err != nil {
		return fmt.Errorf("count api keys: %w", err)
	}
	if count > 0 {
		return nil
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id) VALUES ($1) ON CONFLICT DO NOTHING`,
			[]any{bootstrapOrg}},
		{`INSERT INTO projects (id, organization_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			[]any{bootstrapProject, bootstrapOrg}},
		{`INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1, $2, $3, 'service') ON CONFLICT DO NOTHING`,
			[]any{bootstrapPrincipal, bootstrapOrg, bootstrapProject}},
		{`INSERT INTO api_keys (id, organization_id, project_id, principal_id, key_hash) VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
			[]any{bootstrapKeyID, bootstrapOrg, bootstrapProject, bootstrapPrincipal, coordinator.HashAPIKey(bootstrapKey)}},
	}
	for _, stmt := range statements {
		if _, err := tx.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			return fmt.Errorf("seed bootstrap identity: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit bootstrap: %w", err)
	}
	return nil
}
