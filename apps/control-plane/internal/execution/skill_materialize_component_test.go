//go:build component

package execution

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// buildSkillTGZ packs a single-entry gzip-tar (a real archive) for the quarantine pipeline.
func buildSkillTGZ(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// TestSkillBodyMaterializedAndReadableViaFileTool proves progressive-loading half-2 (spec §28.16): a
// run's frozen skill body is materialized under <alloc>/.palai/skills/<name>/SKILL.md at exactly the
// pinned digest's content, and the FileTool reads it on-demand under the confined workspace root.
func TestSkillBodyMaterializedAndReadableViaFileTool(t *testing.T) {
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
	tenant := coordinator.Tenant{Project: pinnedID("prj")}
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO projects (id) VALUES ($1)`, tenant.Project)

	const bodyMarker = "Use conventional commits with a scope."
	skillMD := []byte("---\nname: commit-convention\ndescription: write commits\n---\n" + bodyMarker + "\n")
	q, err := extensions.Quarantine(buildSkillTGZ(t, "SKILL.md", skillMD))
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}

	sessionID, profileID, revID, runID := pinnedID("ses"), pinnedID("aprof"), pinnedID("arev"), pinnedID("run")
	skillID, skillRevID := pinnedID("skill"), pinnedID("skillrev")
	exec(`INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, sessionID, tenant.Project)
	exec(`INSERT INTO skills (id, project_id, name) VALUES ($1,$2,'commit-convention')`,
		skillID, tenant.Project)
	exec(`INSERT INTO skill_revisions (id, project_id, skill_id, revision_number, digest, state, metadata, archive)
	      VALUES ($1,$2,$3,1,$4,'enabled','{"name":"commit-convention","description":"write commits"}',$5)`,
		skillRevID, tenant.Project, skillID, q.Digest, q.Sanitized)
	exec(`INSERT INTO agent_profiles (id, project_id, name) VALUES ($1,$2,'reviewer')`,
		profileID, tenant.Project)
	exec(`INSERT INTO agent_revisions (id, project_id, profile_id, revision_number, model, tools, skills, published_at)
	      VALUES ($1,$2,$3,1,'m','["file"]','["commit-convention"]',clock_timestamp())`,
		revID, tenant.Project, profileID)
	exec(`INSERT INTO runs (id, project_id, session_id, state, agent_revision_id) VALUES ($1,$2,$3,'running',$4)`,
		runID, tenant.Project, sessionID, revID)

	if err := st.PinRunSkills(ctx, tenant, runID); err != nil {
		t.Fatalf("PinRunSkills: %v", err)
	}

	// Materialize into a throwaway allocation dir.
	alloc := t.TempDir()
	if err := st.MaterializeRunSkills(ctx, tenant, runID, skillWriterUnder(alloc)); err != nil {
		t.Fatalf("MaterializeRunSkills: %v", err)
	}
	onDisk := filepath.Join(alloc, ".palai", "skills", "commit-convention", "SKILL.md")
	got, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("materialized SKILL.md not found at %s: %v", onDisk, err)
	}
	if !strings.Contains(string(got), bodyMarker) {
		t.Fatalf("materialized SKILL.md = %q, want it to contain the skill body", got)
	}

	// The FileTool reads it under the confined workspace root (the pin's path).
	out, err := tools.FileTool().Exec(ctx, toolbroker.ExecEnv{WorkspaceRoot: alloc},
		map[string]any{"op": "read", "path": extensions.SkillBodyPath("commit-convention")})
	if err != nil {
		t.Fatalf("FileTool read of the skill body: %v", err)
	}
	if content, _ := out["content"].(string); !strings.Contains(content, bodyMarker) {
		t.Fatalf("FileTool read content = %q, want the skill body", out["content"])
	}
}

// TestSkillMaterializationRefusesEscapingName is the SEC-1 belt-and-suspenders: even if a pin's name
// somehow contains a `..` traversal, materialization must REFUSE to write outside <hostPath>/.palai/
// skills/ (the restoreEntry containment idiom). The name is validated at create, but a pin must never
// escape the skills root regardless.
func TestSkillMaterializationRefusesEscapingName(t *testing.T) {
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
	tenant := coordinator.Tenant{Project: pinnedID("prj")}
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO projects (id) VALUES ($1)`, tenant.Project)

	// A real sanitized archive stored under a digest the malicious pin references.
	q, err := extensions.Quarantine(buildSkillTGZ(t, "SKILL.md", []byte("---\nname: x\n---\nbody\n")))
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	sessionID, skillID, skillRevID, runID := pinnedID("ses"), pinnedID("skill"), pinnedID("skillrev"), pinnedID("run")
	exec(`INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, sessionID, tenant.Project)
	exec(`INSERT INTO skills (id, project_id, name) VALUES ($1,$2,'ok')`, skillID, tenant.Project)
	exec(`INSERT INTO skill_revisions (id, project_id, skill_id, revision_number, digest, state, metadata, archive)
	      VALUES ($1,$2,$3,1,$4,'enabled','{"name":"ok"}',$5)`,
		skillRevID, tenant.Project, skillID, q.Digest, q.Sanitized)
	exec(`INSERT INTO runs (id, project_id, session_id, state) VALUES ($1,$2,$3,'running')`,
		runID, tenant.Project, sessionID)

	// Inject a pin whose name escapes the skills root (bypassing CreateSkill validation via a raw write).
	// "../escaped/pwned" stays under <alloc> for the test's safety but Rel() still flags the `..` escape.
	exec(`UPDATE runs SET skill_pins = $2 WHERE id = $1`, runID,
		[]byte(`[{"name":"../escaped/pwned","description":"","digest":"`+q.Digest+`","path":"x"}]`))

	alloc := t.TempDir()
	if err := st.MaterializeRunSkills(ctx, tenant, runID, skillWriterUnder(alloc)); err == nil {
		t.Fatal("MaterializeRunSkills accepted an escaping skill name — containment must refuse it")
	}
	if _, statErr := os.Stat(filepath.Join(alloc, ".palai", "escaped")); statErr == nil {
		t.Fatal("materialization wrote outside <alloc>/.palai/skills/ — containment breached")
	}
}

// skillWriterUnder is the local stand-in for the attempt's confined workspace surface: MaterializeRunSkills
// takes a WRITER since A.3 T5 (the allocation may live on another machine), so a component test that owns a
// host directory supplies the host-writing equivalent of what the runner does. It mirrors the helper the
// e2e journey uses, and the paths it receives are SLASH-RELATIVE — filepath.Join is what makes them local.
//
// It is NOT the containment boundary and must not be read as one. MaterializeRunSkills refuses an escaping
// pin by NAME before it calls write at all, which is what TestSkillMaterializationRefusesEscapingName
// exercises; this closure would happily join a `..` if it were ever handed one.
func skillWriterUnder(root string) func(rel string, body []byte) error {
	return func(rel string, body []byte) error {
		dst := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, body, 0o644)
	}
}
