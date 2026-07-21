//go:build component

package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
)

// pinnedID is a local id minter for this test (the package's other helpers are keyed to their suites).
func pinnedID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// TestPinnedRevisionConfigHashReflectsRevision proves the ExecutionSpec-resolution seam (spec §14,
// AGT-001): a run pinned to a published AgentRevision resolves its effective config from the revision,
// so effectiveConfigHash (checkpoint.go:187 — the config a checkpoint records) reflects the revision's
// model, and a profile-free run resolves differently. Crucially, publishing a LATER revision of the
// same profile leaves the old run's config hash UNCHANGED (the pin is fixed on the run row), so a
// checkpointed run stays reproducible under revision churn.
func TestPinnedRevisionConfigHashReflectsRevision(t *testing.T) {
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := cs.Pool()
	tenant := coordinator.Tenant{Organization: pinnedID("org"), Project: pinnedID("prj")}
	sessionID := pinnedID("ses")
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	exec(`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	exec(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, sessionID, tenant.Organization, tenant.Project)

	// A published revision that pins a distinctive model, and a run pinned to it.
	profileID, revID, pinnedRun := pinnedID("aprof"), pinnedID("arev"), pinnedID("run")
	exec(`INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,'reviewer')`,
		profileID, tenant.Organization, tenant.Project)
	exec(`INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, published_at)
	      VALUES ($1,$2,$3,$4,1,'model-pinned-v1', clock_timestamp())`,
		revID, tenant.Organization, tenant.Project, profileID)
	exec(`INSERT INTO runs (id, organization_id, project_id, session_id, state, agent_revision_id) VALUES ($1,$2,$3,$4,'running',$5)`,
		pinnedRun, tenant.Organization, tenant.Project, sessionID, revID)

	// A profile-free run in a separate session (one-active-root is per session).
	freeSession, freeRun := pinnedID("ses"), pinnedID("run")
	exec(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, freeSession, tenant.Organization, tenant.Project)
	exec(`INSERT INTO runs (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'running')`,
		freeRun, tenant.Organization, tenant.Project, freeSession)

	// The orchestrator with a deployment route whose model differs from the revision's, so a change in
	// the hash can only come from the pinned revision layer.
	orch := &Orchestrator{spine: cs, route: ModelRoute{Model: "deployment-default", Secret: "model"}}
	hashFor := func(runID, session string) string {
		st := &attemptState{
			attempt:   AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(pinnedID("att"))},
			tenant:    tenant,
			sessionID: session,
		}
		h, err := orch.effectiveConfigHash(ctx, st)
		if err != nil {
			t.Fatalf("effectiveConfigHash(%s): %v", runID, err)
		}
		return h
	}

	pinnedHash := hashFor(pinnedRun, sessionID)
	freeHash := hashFor(freeRun, freeSession)
	if pinnedHash == freeHash {
		t.Fatal("pinned-revision run and profile-free run resolved the SAME config hash; the revision layer did not reach checkpoint.go:187")
	}

	// Publish a LATER revision of the same profile with a different model. The old run's pin is fixed,
	// so its config hash must not move — an old checkpointed run stays reproducible under churn.
	newRevID := pinnedID("arev")
	exec(`INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, published_at)
	      VALUES ($1,$2,$3,$4,2,'model-pinned-v2', clock_timestamp())`,
		newRevID, tenant.Organization, tenant.Project, profileID)
	if again := hashFor(pinnedRun, sessionID); again != pinnedHash {
		t.Fatalf("old run's config hash changed after a later revision was published: %q -> %q (pin must be immutable)", pinnedHash, again)
	}
}
