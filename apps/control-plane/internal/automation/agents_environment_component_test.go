//go:build component

package automation

import (
	"context"
	"errors"
	"testing"

	"github.com/palgroup/palai/storage"
)

// E25 T3's referential check on the agent-revision write path, and the reason it EXISTS at all is worth
// stating before what it does: this package's own RevisionInput comment defers reference validation for
// tool_sets/mcp_connections/skills/hooks to CONSUMPTION, because a typo'd id there fails closed and
// harmlessly — it grants no capability and leaks nothing.
//
// An environment fails closed too (an unknown id resolves to zero keys), but "harmlessly" does not follow.
// The operator's belief is that the agent HAS the production credentials, and a run that silently receives
// none does not stop: `curl` succeeds anonymously, `gh` reads the public repository, a deploy script writes
// to the default target. That is a wrong answer that looks like a right one, and it is worth one query.

func seedEnvironment(t *testing.T, s *Store) string {
	t.Helper()
	id := testID("env")
	if _, err := s.pool.Exec(storage.WithSystemScope(context.Background()),
		`INSERT INTO environments (id, name) VALUES ($1,$2)`, id, testID("name")); err != nil {
		t.Fatalf("seed environment: %v", err)
	}
	return id
}

// TestARevisionNamingAnUnknownEnvironmentIsRefusedAtCreateAndAtPublish covers both places and both
// directions. The PUBLISH leg is the one that matters: publish is what makes a revision runnable, so it is
// the last moment at which "this agent has the credentials" can still be refused instead of discovered by a
// run that had none — and it is the single throat every revision passes through, whichever surface created
// it.
func TestARevisionNamingAnUnknownEnvironmentIsRefusedAtCreateAndAtPublish(t *testing.T) {
	s, project := openStore(t)
	ctx := context.Background()
	profileID, err := s.CreateProfile(ctx, project, "deployer")
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	// CREATE with an environment id that does not exist.
	if _, err := s.CreateRevision(ctx, project, profileID, []byte(`{"model":"m","environment":"env_does_not_exist"}`)); !errors.Is(err, ErrEnvironmentNotFound) {
		t.Fatalf("CreateRevision with an unknown environment = %v, want ErrEnvironmentNotFound", err)
	}

	// A REAL environment is accepted, so the refusal above is not "every create fails".
	envID := seedEnvironment(t, s)
	rev, err := s.CreateRevision(ctx, project, profileID, []byte(`{"model":"m","environment":"`+envID+`"}`))
	if err != nil {
		t.Fatalf("CreateRevision with a real environment: %v", err)
	}
	if rev.Environment != envID {
		t.Fatalf("the created revision reports environment %q, want %q — the column is written but not read back", rev.Environment, envID)
	}
	if published, exists, err := s.PublishRevision(ctx, project, rev.ID); err != nil || !published || !exists {
		t.Fatalf("publishing a revision with a real environment = (%v, %v, %v)", published, exists, err)
	}

	// THE PUBLISH LEG. The row is written directly — the create route already refuses this, so the only way
	// to reach publish with a dangling reference is a row that got in some other way (a future console
	// path, a CLI, an import, a hand-edit). That is exactly the case this check exists for.
	dangling := testID("arev")
	if _, err := s.pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO agent_revisions (id, project_id, profile_id, revision_number, model, environment)
		 VALUES ($1,$2,$3,99,'m','env_vanished')`,
		dangling, project, profileID); err != nil {
		t.Fatalf("seed a dangling revision: %v", err)
	}
	published, exists, err := s.PublishRevision(ctx, project, dangling)
	if !errors.Is(err, ErrEnvironmentNotFound) {
		t.Fatalf("PublishRevision on a dangling environment = %v, want ErrEnvironmentNotFound", err)
	}
	if published {
		t.Fatal("the dangling revision was published anyway")
	}
	// exists=true so the caller renders a 400 and not a 404: the revision is real, its environment is not,
	// and a 404 would send an operator looking for the wrong thing.
	if !exists {
		t.Fatal("the refusal reported the revision as unknown; the caller would render a 404 for a revision that exists")
	}
	// AND IT REALLY DID NOT PUBLISH — the row's published_at is still NULL. A refusal that returned an
	// error after flipping the stamp would be a refusal in name only, and publish is irreversible.
	var stamp *string
	if err := s.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT published_at::text FROM agent_revisions WHERE id=$1`, dangling).Scan(&stamp); err != nil {
		t.Fatalf("read the dangling revision's publish stamp: %v", err)
	}
	if stamp != nil {
		t.Fatalf("the dangling revision's published_at = %q — the refusal flipped the stamp it refused", *stamp)
	}

	// A revision naming NO environment publishes exactly as it always did. This is the bit-unchanged leg,
	// and it is every revision written before agent_revisions gained the `environment` column.
	bare, err := s.CreateRevision(ctx, project, profileID, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("CreateRevision with no environment: %v", err)
	}
	if bare.Environment != "" {
		t.Fatalf("a revision with no environment reports %q", bare.Environment)
	}
	if published, _, err := s.PublishRevision(ctx, project, bare.ID); err != nil || !published {
		t.Fatalf("publishing an environment-less revision = (%v, %v)", published, err)
	}
}

// TestAnEnvironmentIsVisibleToEveryProjectInTheInstallation records a CEILING, and it is deliberately the
// opposite assertion to the one that stood here.
//
// This test was TestARevisionCannotNameAnotherOrganizationsEnvironment and its sentence said the refusal
// was "RLS-driven — verifyEnvironment reads under the revision's own org scope, so a foreign id is
// invisible rather than forbidden". Both halves of that are gone. Organizations were removed, and
// `environments` carries no project_id (000001_core.up.sql:439), so 000002_row_level_security.up.sql:196
// applies the INSTALLATION policy to it. verifyEnvironment (agents.go:216) opens
// storage.WithInstallationScope and asks "does this id exist", not "does it exist in your tenant" —
// which agents.go:204 already states in the production comment. This test was the half left behind still
// claiming the opposite, and it is the only reason the removal was visible at all.
//
// Deleting it would delete the record together with the claim, so it asserts what is true instead, and it
// is written to RED the moment the ceiling closes: when `environments` gains a project_id and this read is
// re-scoped, the acceptance below becomes ErrEnvironmentNotFound and whoever closed it is sent here to
// restore the tenancy refusal. Within ONE installation "does it exist" and "does it exist in your tenant"
// are the same question; across two customers sharing one they are not, which is why the installation
// policy's own header names this table as needing a project_id BEFORE such an installation exists.
func TestAnEnvironmentIsVisibleToEveryProjectInTheInstallation(t *testing.T) {
	s, projectA := openStore(t)
	ctx := context.Background()

	// An environment seeded OUTSIDE projectA — and it cannot be seeded inside it, because the table has no
	// column that would put it there. That absence is the ceiling itself, not a shortcut of the fixture.
	envB := seedEnvironment(t, s)

	profileID, err := s.CreateProfile(ctx, projectA, "deployer")
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	rev, err := s.CreateRevision(ctx, projectA, profileID, []byte(`{"model":"m","environment":"`+envB+`"}`))
	if errors.Is(err, ErrEnvironmentNotFound) {
		t.Fatal("a project's revision was REFUSED an environment of its own installation — the ceiling this " +
			"test records has CLOSED, which is the wanted direction. Restore the tenancy claim it replaced: " +
			"a revision must not be bound to an environment belonging to another tenant.")
	}
	if err != nil {
		t.Fatalf("CreateRevision naming an installation environment: %v", err)
	}
	if rev.Environment != envB {
		t.Fatalf("the revision reports environment %q, want the installation environment %q it named", rev.Environment, envB)
	}
	// NON-VACUITY. The acceptance above has to be the installation scope answering "it exists", not
	// verifyEnvironment waving every string through — an id in NO project of this installation is still
	// refused, which is what makes the acceptance a statement about SCOPE rather than about nothing.
	if _, err := s.CreateRevision(ctx, projectA, profileID, []byte(`{"model":"m","environment":"env_vanished"}`)); !errors.Is(err, ErrEnvironmentNotFound) {
		t.Fatalf("a revision naming an id in no project of this installation = %v, want ErrEnvironmentNotFound", err)
	}
}
