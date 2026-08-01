package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/coordinator"
)

// THE ROUTE ALIAS THAT NOTHING READS (E29 provider models).
//
// MEASURED ON THE LIVE STACK, 2026-08-01, before this guard existed:
//
//	POST /v1/model-routes                                  {"name":"anthropic-default"}  -> 201
//	POST /v1/model-routes/{id}/revisions                   {"model":"claude-sonnet-5",…}  -> 201
//	POST /v1/model-routes/{id}/revisions/{rev}/publish                                    -> 200
//	POST /v1/responses                                     -> ran on the DEPLOYMENT DEFAULT
//
// Four calls, four successes, and the route was never consulted. Dispatch resolves exactly one alias —
// coordinator.DefaultModelRouteAlias, the literal "default", used at ProjectModelRoute — and nothing
// between the create and the run had a reason to say so.
//
// That is this tree's recurring defect verbatim: a thing declared, named and routed, read as a thing that
// HAPPENS. The file's own comment is honest about why one alias exists ("no config layer can yet ASK for
// one, so storing per-run alias selection would be dead config") — which is exactly what makes every OTHER
// name dead config, and the API accepted them all.

// The create REFUSES a name dispatch will never resolve, and the refusal names the alias that works. A 400
// at the moment the operator submits is the whole difference between this and a run at 3am on a model they
// did not choose.
func TestCreatingAModelRouteRefusesAnAliasDispatchWillNeverRead(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	scope := middleware.Scope{Organization: "org_1", Project: "prj_1"}

	out, err := s.CreateModelRoute(ctx, scope, []byte(`{"name":"anthropic-default"}`))
	if err != nil {
		t.Fatalf("CreateModelRoute() error = %v", err)
	}
	if out.MissingField == "" {
		t.Fatalf("CreateModelRoute({\"name\":\"anthropic-default\"}) = %+v, want a 400: this alias would be "+
			"created, published, and never consulted by a single run", out)
	}
	// The operator must be able to act on the refusal without reading the source, so the message carries
	// the one name that works.
	if !strings.Contains(out.MissingField, coordinator.DefaultModelRouteAlias) {
		t.Fatalf("the refusal is %q — it does not name %q, the only alias dispatch resolves",
			out.MissingField, coordinator.DefaultModelRouteAlias)
	}
}

// The alias that DOES work is unchanged. This is the whole existing surface — every create in this tree
// already passes coordinator.DefaultModelRouteAlias, so the refusal above costs nothing that was working.
func TestCreatingTheDefaultModelRouteAliasIsUnaffected(t *testing.T) {
	// requireProjectScope passes and the name check passes, so this reaches the (nil) spine. Reaching it is
	// the assertion: a refusal would have returned before the panic.
	defer func() {
		if recover() == nil {
			t.Fatalf("CreateModelRoute(%q) returned without reaching the spine — the alias that dispatch DOES "+
				"read was refused", coordinator.DefaultModelRouteAlias)
		}
	}()
	_, _ = (&Store{}).CreateModelRoute(context.Background(),
		middleware.Scope{Organization: "org_1", Project: "prj_1"},
		[]byte(`{"name":"`+coordinator.DefaultModelRouteAlias+`"}`))
}

// REFUSING NEW ONES DOES NOT FIND THE OLD ONES, and a sweep that only looks forward is this tree's other
// recurring defect. The live stack this was measured on ALREADY holds an `anthropic-default` row: the
// guard above cannot reach it, and only the read-back can tell its operator it is inert.
func TestTheRouteProjectionSaysWhetherDispatchReadsThisAlias(t *testing.T) {
	read := func(name string) map[string]any {
		t.Helper()
		raw, err := json.Marshal(modelRouteView(coordinator.ModelRouteRecord{ID: "mroute_1", Name: name}))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return out
	}

	live := read(coordinator.DefaultModelRouteAlias)
	if live["consulted_by_dispatch"] != true {
		t.Fatalf("the %q alias renders consulted_by_dispatch = %v, want true",
			coordinator.DefaultModelRouteAlias, live["consulted_by_dispatch"])
	}

	dead := read("anthropic-default")
	if dead["consulted_by_dispatch"] != false {
		t.Fatalf("an alias no run resolves renders consulted_by_dispatch = %v, want false — an operator "+
			"reading this list has no other way to learn the row is inert", dead["consulted_by_dispatch"])
	}
	// A boolean an operator has to interpret is a boolean an operator will interpret wrongly, so the dead
	// row carries the sentence too.
	if note, _ := dead["dispatch_note"].(string); !strings.Contains(note, coordinator.DefaultModelRouteAlias) {
		t.Fatalf("dispatch_note = %q, want a sentence naming %q", note, coordinator.DefaultModelRouteAlias)
	}
	if _, present := live["dispatch_note"]; present {
		t.Fatal("the live alias carries a dispatch note — a warning on the row that is working teaches an " +
			"operator to ignore warnings")
	}
}
