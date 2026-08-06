//go:build component

package identity_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/packages/toolset"
	"github.com/palgroup/palai/storage"
)

// A TENANT IS BORN WITH THE TOOL BASELINE, OR ITS AGENTS NARRATE.
//
// ‼️ MEASURED ON A LIVE PLANE, 2026-08-06, on a project Palai Cloud had just opened for a customer. The
// run was asked to list a cloned repository's Swift files with the shell tool. It made ZERO tool calls,
// reached `completed`, and answered:
//
//	"I will use the shell tool to list the Swift files. Executing the command now.
//	 ```bash ls *.swift``` Please hold on while I gather the res…"
//
// A model with no tools does not refuse — it describes what it would have done. Every layer reported
// success, the smoke leg that drove it reported PASS, and the customer had a plane whose agent could not
// touch a file.
//
// The cause was WHERE the grant lived. `palai up` wrote toolset.Default() into the project it had made,
// at the CLI — so a self-hosted operator got a working agent and every project opened over the API got a
// mute one. POST /v1/projects is the only way Cloud opens a tenant.
//
// These two tests hold the fix from the two directions it can rot:
//
//	(1) the row a tenant is BORN with carries the baseline — asserted by reading the database, not the
//	    create response, because a projection can say anything;
//	(2) the create RESPONSE agrees with that row, so a caller's first read does not contradict its own
//	    creation.

func TestATenantIsBornWithTheCanonicalToolBaseline(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())

	parent, _ := provisionOrg(t, idstore, "baseline-birth")
	out, err := idstore.CreateProject(ctx, middleware.Scope{Project: parent}, []byte(`{"display_name":"born"}`))
	if err != nil {
		t.Fatalf("CreateProject error = %v", err)
	}
	var created struct {
		ID           string          `json:"id"`
		ConfigPolicy json.RawMessage `json:"config_policy"`
	}
	if err := json.Unmarshal(out.Body, &created); err != nil {
		t.Fatalf("decode project body: %v", err)
	}

	// (1) THE ROW, read straight out of the database. The response is a projection and could carry a
	// baseline the INSERT never wrote — which is exactly the shape this test exists to refuse.
	var stored []byte
	if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT config_policy FROM projects WHERE id = $1`, created.ID).Scan(&stored); err != nil {
		t.Fatalf("read the born project's policy: %v", err)
	}
	var policy struct {
		DefaultTools []string `json:"default_tools"`
	}
	if err := json.Unmarshal(stored, &policy); err != nil {
		t.Fatalf("the born policy is not an object (%s): %v", stored, err)
	}

	want := append([]string(nil), toolset.Default()...)
	got := append([]string(nil), policy.DefaultTools...)
	sort.Strings(want)
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("a newly opened tenant was granted %d tool(s), want the canonical %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("a newly opened tenant's baseline diverges from toolset.Default(): got %v, want %v", got, want)
		}
	}

	// (2) AND THE RESPONSE AGREES WITH THE ROW. A create that answered `null` while the database held the
	// baseline would make a caller's first read disagree with its own creation.
	var answered struct {
		DefaultTools []string `json:"default_tools"`
	}
	if err := json.Unmarshal(created.ConfigPolicy, &answered); err != nil {
		t.Fatalf("the create response's config_policy is not an object (%s): %v", created.ConfigPolicy, err)
	}
	if len(answered.DefaultTools) != len(want) {
		t.Fatalf("the create response reports %d tool(s) and the row holds %d", len(answered.DefaultTools), len(want))
	}
}
