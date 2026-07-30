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

func seedEnvironment(t *testing.T, s *Store, org string) string {
	t.Helper()
	id := testID("env")
	if _, err := s.pool.Exec(storage.WithSystemScope(context.Background()),
		`INSERT INTO environments (id, organization_id, name) VALUES ($1,$2,$3)`, id, org, testID("name")); err != nil {
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
	s, org, project := openStore(t)
	ctx := context.Background()
	profileID, err := s.CreateProfile(ctx, org, project, "deployer")
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	// CREATE with an environment id that does not exist.
	if _, err := s.CreateRevision(ctx, org, project, profileID, []byte(`{"model":"m","environment":"env_does_not_exist"}`)); !errors.Is(err, ErrEnvironmentNotFound) {
		t.Fatalf("CreateRevision with an unknown environment = %v, want ErrEnvironmentNotFound", err)
	}

	// A REAL environment is accepted, so the refusal above is not "every create fails".
	envID := seedEnvironment(t, s, org)
	rev, err := s.CreateRevision(ctx, org, project, profileID, []byte(`{"model":"m","environment":"`+envID+`"}`))
	if err != nil {
		t.Fatalf("CreateRevision with a real environment: %v", err)
	}
	if rev.Environment != envID {
		t.Fatalf("the created revision reports environment %q, want %q — the column is written but not read back", rev.Environment, envID)
	}
	if published, exists, err := s.PublishRevision(ctx, org, project, rev.ID); err != nil || !published || !exists {
		t.Fatalf("publishing a revision with a real environment = (%v, %v, %v)", published, exists, err)
	}

	// THE PUBLISH LEG. The row is written directly — the create route already refuses this, so the only way
	// to reach publish with a dangling reference is a row that got in some other way (a future console
	// path, a CLI, an import, a hand-edit). That is exactly the case this check exists for.
	dangling := testID("arev")
	if _, err := s.pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, environment)
		 VALUES ($1,$2,$3,$4,99,'m','env_vanished')`,
		dangling, org, project, profileID); err != nil {
		t.Fatalf("seed a dangling revision: %v", err)
	}
	published, exists, err := s.PublishRevision(ctx, org, project, dangling)
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
	// and it is every revision in every deployment before migration 000046.
	bare, err := s.CreateRevision(ctx, org, project, profileID, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("CreateRevision with no environment: %v", err)
	}
	if bare.Environment != "" {
		t.Fatalf("a revision with no environment reports %q", bare.Environment)
	}
	if published, _, err := s.PublishRevision(ctx, org, project, bare.ID); err != nil || !published {
		t.Fatalf("publishing an environment-less revision = (%v, %v)", published, err)
	}
}

// TestARevisionCannotNameAnotherOrganizationsEnvironment closes the tenancy half of the same check. The
// refusal is RLS-driven — verifyEnvironment reads under the revision's own org scope — so a foreign id is
// invisible rather than forbidden, and reported as absent. That is the intended answer: an operator must
// not learn from an error message that an id exists in another tenant.
func TestARevisionCannotNameAnotherOrganizationsEnvironment(t *testing.T) {
	s, orgA, projectA := openStore(t)
	ctx := context.Background()

	// A second organization in the same database, with its own environment.
	orgB := testID("org")
	if _, err := s.pool.Exec(storage.WithSystemScope(ctx), `INSERT INTO organizations (id) VALUES ($1)`, orgB); err != nil {
		t.Fatalf("seed org B: %v", err)
	}
	envB := seedEnvironment(t, s, orgB)

	profileID, err := s.CreateProfile(ctx, orgA, projectA, "deployer")
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if _, err := s.CreateRevision(ctx, orgA, projectA, profileID, []byte(`{"model":"m","environment":"`+envB+`"}`)); !errors.Is(err, ErrEnvironmentNotFound) {
		t.Fatalf("A's revision naming B's environment = %v, want ErrEnvironmentNotFound — an agent in one org must not be bound to another's credentials", err)
	}
}
