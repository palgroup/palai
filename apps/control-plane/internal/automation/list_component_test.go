//go:build component

package automation

// E13 Task 4: the automation list reads (agent profiles/revisions, triggers) over real Postgres and RLS.
// Same tenant-scoped keyset the run-history list uses, proven one layer out.

import (
	"context"
	"testing"

	"github.com/palgroup/palai/storage"
)

func TestListAgentsAndTriggersTenantScoped(t *testing.T) {
	s, orgA, projA := openStore(t)
	ctx := context.Background()

	// A second tenant on the same pool, so the list must not cross the boundary.
	orgB, projB := testID("org"), testID("prj")
	sys := storage.WithSystemScope(ctx)
	if _, err := s.pool.Exec(sys, `INSERT INTO organizations (id) VALUES ($1)`, orgB); err != nil {
		t.Fatalf("seed org B: %v", err)
	}
	if _, err := s.pool.Exec(sys, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, projB, orgB); err != nil {
		t.Fatalf("seed project B: %v", err)
	}

	// Two profiles in A (one carrying two revisions), one in B.
	profileA, err := s.CreateProfile(ctx, orgA, projA, "reviewer")
	if err != nil {
		t.Fatalf("create profile A: %v", err)
	}
	if _, err := s.CreateProfile(ctx, orgA, projA, "planner"); err != nil {
		t.Fatalf("create profile A2: %v", err)
	}
	for _, m := range []string{`{"model":"m1"}`, `{"model":"m2"}`} {
		if _, err := s.CreateRevision(ctx, orgA, projA, profileA, []byte(m)); err != nil {
			t.Fatalf("create revision A: %v", err)
		}
	}
	if _, err := s.CreateProfile(ctx, orgB, projB, "b-only"); err != nil {
		t.Fatalf("create profile B: %v", err)
	}

	profiles, err := s.ListProfiles(ctx, orgA, projA, ListWindow{Limit: 10})
	if err != nil || len(profiles) != 2 {
		t.Fatalf("ListProfiles(A) err=%v len=%d, want 2 (tenant-scoped)", err, len(profiles))
	}

	revs, err := s.ListRevisions(ctx, orgA, projA, profileA, ListWindow{Limit: 10})
	if err != nil || len(revs) != 2 {
		t.Fatalf("ListRevisions(A, %s) err=%v len=%d, want 2", profileA, err, len(revs))
	}
	// An unknown/foreign profile yields an empty page (no existence oracle beyond emptiness).
	if empty, err := s.ListRevisions(ctx, orgA, projA, "aprof_missing", ListWindow{Limit: 10}); err != nil || len(empty) != 0 {
		t.Fatalf("ListRevisions(unknown) err=%v len=%d, want 0", err, len(empty))
	}

	// Keyset over the two profiles: page 1 then page 2 keyed off it returns the older row, no repeat.
	page1, err := s.ListProfiles(ctx, orgA, projA, ListWindow{Limit: 1})
	if err != nil || len(page1) != 1 {
		t.Fatalf("profiles page1 err=%v len=%d, want 1", err, len(page1))
	}
	last := page1[0]
	page2, err := s.ListProfiles(ctx, orgA, projA, ListWindow{Limit: 1, AfterCreatedAt: &last.CreatedAt, AfterID: last.ID})
	if err != nil || len(page2) != 1 || page2[0].ID == last.ID {
		t.Fatalf("profiles page2 err=%v len=%d id=%v, want 1 distinct row", err, len(page2), page2)
	}

	// Triggers: two in A, one in B.
	triggers := NewTriggerStore(s.pool)
	for _, n := range []string{"nightly", "on-push"} {
		if _, err := triggers.CreateTrigger(ctx, orgA, projA, "prin_test", n, "manual_api"); err != nil {
			t.Fatalf("create trigger A/%s: %v", n, err)
		}
	}
	if _, err := triggers.CreateTrigger(ctx, orgB, projB, "prin_test", "b-only", "manual_api"); err != nil {
		t.Fatalf("create trigger B: %v", err)
	}
	list, err := triggers.ListTriggers(ctx, orgA, projA, ListWindow{Limit: 10})
	if err != nil || len(list) != 2 {
		t.Fatalf("ListTriggers(A) err=%v len=%d, want 2", err, len(list))
	}
}
