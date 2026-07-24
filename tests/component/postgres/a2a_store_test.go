//go:build component

package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/a2a"
)

// TestA2AStoreRLSAndCanonicalRefInvariant proves the migration-000038 store against real PostgreSQL:
//  1. a published interface is readable only within its owning tenant (RLS) — a foreign tenant sees nothing;
//  2. the public-card resolve returns only SAFE card columns, and rendering it leaks no internal marker;
//  3. the external A2A task/context id is stored BESIDE the canonical run/session id and NEVER replaces it
//     (§38.2) — the round-tripped run_id equals the canonical id and differs from the external a2a_task_id;
//  4. a task ref is tenant-isolated (a foreign tenant cannot read it);
//  5. push configs round-trip through the JSONB column.
func TestA2AStoreRLSAndCanonicalRefInvariant(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	tenantA, _, canonicalRun := seedRun(t, pool)
	tenantB, _, _ := seedRun(t, pool)

	store := a2a.NewStore(pool, newID)
	ctx := context.Background()

	// Publish an interface for tenant A, projected from a revision carrying distinctive sensitive markers.
	iface := a2a.ProjectInterface("rev_pinned", a2a.RevisionSource{
		Organization: tenantA.Organization, Project: tenantA.Project,
		Model:        "provider-model-CONFIDENTIAL",
		Tools:        []string{"internal_TOOL"},
		Instructions: "SYSTEM-PROMPT-CONFIDENTIAL",
	}, a2a.PublishMeta{
		Name: "Store Planner", Description: "Plans.", Version: "1",
		Streaming: true, PushNotifications: true, ExtendedCard: true,
		InputModes: []string{"text/plain"}, OutputModes: []string{"application/json"},
		Skills: []a2a.AgentSkill{{ID: "plan", Name: "Plan"}}, AuthScheme: "bearer",
	})
	interfaceID, err := store.PublishInterface(ctx, iface)
	if err != nil {
		t.Fatalf("PublishInterface: %v", err)
	}

	// (1) RLS: tenant A reads it; tenant B does not.
	if _, ok, err := store.Get(ctx, tenantA.Organization, tenantA.Project, interfaceID); err != nil || !ok {
		t.Fatalf("owning tenant cannot read its interface: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.Get(ctx, tenantB.Organization, tenantB.Project, interfaceID); err != nil || ok {
		t.Fatalf("foreign tenant read a cross-tenant interface (RLS breach): ok=%v err=%v", ok, err)
	}

	// (2) Public resolve returns only safe card columns; the rendered card leaks nothing.
	pub, ok, err := store.ResolvePublic(ctx, interfaceID)
	if err != nil || !ok {
		t.Fatalf("ResolvePublic: ok=%v err=%v", ok, err)
	}
	card := a2a.RenderCard(pub, a2a.CardEndpoint{BaseURL: "https://cp.test", InterfaceID: interfaceID})
	blob, _ := json.Marshal(card)
	for _, forbidden := range []string{"provider-model-CONFIDENTIAL", "internal_TOOL", "SYSTEM-PROMPT-CONFIDENTIAL"} {
		if strings.Contains(string(blob), forbidden) {
			t.Fatalf("public card LEAKS %q: %s", forbidden, blob)
		}
	}

	// (3) §38.2: external A2A ids are stored beside the canonical run/session, never replacing them.
	const externalTaskID = "a2atask_ext_1"
	const externalContextID = "a2actx_ext_1"
	if err := store.Put(ctx, tenantA.Organization, tenantA.Project, a2a.TaskRef{
		InterfaceID: interfaceID, A2ATaskID: externalTaskID, A2AContextID: externalContextID,
		RunID: canonicalRun, SessionID: "ses_canonical",
	}); err != nil {
		t.Fatalf("Put task ref: %v", err)
	}
	ref, ok, err := store.GetRef(ctx, tenantA.Organization, tenantA.Project, interfaceID, externalTaskID)
	if err != nil || !ok {
		t.Fatalf("GetRef: ok=%v err=%v", ok, err)
	}
	if ref.RunID != canonicalRun {
		t.Fatalf("canonical run id was replaced: got %q, want %q", ref.RunID, canonicalRun)
	}
	if ref.A2ATaskID == ref.RunID || ref.A2AContextID == ref.SessionID {
		t.Fatalf("external A2A id collapsed onto the canonical id (§38.2 violated): %+v", ref)
	}

	// (4) Task ref is tenant-isolated.
	if _, ok, err := store.GetRef(ctx, tenantB.Organization, tenantB.Project, interfaceID, externalTaskID); err != nil || ok {
		t.Fatalf("foreign tenant read a cross-tenant task ref (RLS breach): ok=%v err=%v", ok, err)
	}

	// (5) Push configs round-trip through JSONB.
	cfgs := []a2a.PushNotificationConfig{{ID: "pc1", URL: "https://sink.test/hook"}}
	if err := store.SetPushConfigs(ctx, tenantA.Organization, tenantA.Project, interfaceID, externalTaskID, cfgs); err != nil {
		t.Fatalf("SetPushConfigs: %v", err)
	}
	ref, _, err = store.GetRef(ctx, tenantA.Organization, tenantA.Project, interfaceID, externalTaskID)
	if err != nil {
		t.Fatalf("GetRef after push: %v", err)
	}
	if len(ref.PushConfigs) != 1 || ref.PushConfigs[0].ID != "pc1" {
		t.Fatalf("push configs did not round-trip: %+v", ref.PushConfigs)
	}
}
