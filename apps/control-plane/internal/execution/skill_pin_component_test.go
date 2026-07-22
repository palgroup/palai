//go:build component

package execution

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// skillPin mirrors the frozen run-pin shape (extensions.SkillPin) for assertion without importing it.
type skillPin struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Digest      string `json:"digest"`
	Path        string `json:"path"`
}

// TestSkillDigestPinnedRunRecordsExactDigest proves the run-start freeze (spec §28.16, TOL-011): a run
// records the EXACT digest of the skill's enabled revision at start, and a mid-run enable of a NEW
// revision NEVER changes the pin — the run resolves its frozen digest, never "latest".
func TestSkillDigestPinnedRunRecordsExactDigest(t *testing.T) {
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	pool := st.Spine().Pool()
	tenant := coordinator.Tenant{Organization: pinnedID("org"), Project: pinnedID("prj")}
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	exec(`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)

	// A skill with an ENABLED revision (digest D1). The archive bytes are immaterial to the pin.
	sessionID, profileID, revID, runID := pinnedID("ses"), pinnedID("aprof"), pinnedID("arev"), pinnedID("run")
	skillID, rev1ID := pinnedID("skill"), pinnedID("skillrev")
	const d1 = "sha256:d1d1d1"
	exec(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, tenant.Organization, tenant.Project)
	exec(`INSERT INTO skills (id, organization_id, project_id, name) VALUES ($1,$2,$3,'commit-convention')`,
		skillID, tenant.Organization, tenant.Project)
	exec(`INSERT INTO skill_revisions (id, organization_id, project_id, skill_id, revision_number, digest, state, metadata, archive)
	      VALUES ($1,$2,$3,$4,1,$5,'enabled','{"name":"commit-convention","description":"write commits"}','\x00')`,
		rev1ID, tenant.Organization, tenant.Project, skillID, d1)

	// A run pinning an agent revision that REQUESTS the skill.
	exec(`INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,'reviewer')`,
		profileID, tenant.Organization, tenant.Project)
	exec(`INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, tools, skills, published_at)
	      VALUES ($1,$2,$3,$4,1,'model-pinned','["file"]','["commit-convention"]', clock_timestamp())`,
		revID, tenant.Organization, tenant.Project, profileID)
	exec(`INSERT INTO runs (id, organization_id, project_id, session_id, state, agent_revision_id) VALUES ($1,$2,$3,$4,'running',$5)`,
		runID, tenant.Organization, tenant.Project, sessionID, revID)

	// Freeze the pins at run-start.
	if err := st.PinRunSkills(ctx, tenant, runID); err != nil {
		t.Fatalf("PinRunSkills: %v", err)
	}
	pins := readPins(t, ctx, st, tenant, runID)
	if len(pins) != 1 || pins[0].Digest != d1 || pins[0].Name != "commit-convention" || pins[0].Path != ".palai/skills/commit-convention/SKILL.md" {
		t.Fatalf("frozen pins = %+v, want one pin at digest %s with the workspace path", pins, d1)
	}

	// Mid-run: ENABLE a NEW revision (digest D2, higher number). The pin must NOT move.
	const d2 = "sha256:d2d2d2"
	rev2ID := pinnedID("skillrev")
	exec(`INSERT INTO skill_revisions (id, organization_id, project_id, skill_id, revision_number, digest, state, metadata, archive)
	      VALUES ($1,$2,$3,$4,2,$5,'enabled','{"name":"commit-convention","description":"v2"}','\x00')`,
		rev2ID, tenant.Organization, tenant.Project, skillID, d2)

	// A resumed attempt re-runs the pin path — it must be a no-op (already frozen).
	if err := st.PinRunSkills(ctx, tenant, runID); err != nil {
		t.Fatalf("PinRunSkills (resume): %v", err)
	}
	after := readPins(t, ctx, st, tenant, runID)
	if len(after) != 1 || after[0].Digest != d1 {
		t.Fatalf("pins after mid-run enable = %+v, want STILL digest %s (never 'latest')", after, d1)
	}
}

func readPins(t *testing.T, ctx context.Context, st *store.Store, tenant coordinator.Tenant, runID string) []skillPin {
	t.Helper()
	_, _, _, _, pinsJSON, err := st.Spine().PinnedExecConfig(ctx, tenant, runID)
	if err != nil {
		t.Fatalf("PinnedExecConfig: %v", err)
	}
	var pins []skillPin
	if len(pinsJSON) > 0 {
		if err := json.Unmarshal(pinsJSON, &pins); err != nil {
			t.Fatalf("decode skill pins: %v", err)
		}
	}
	return pins
}
