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
	// AND IT IS ASSERTED AS THE OPERATOR READS IT, not as the field holds it. api/model_routes.go renders
	// `MissingField + " is required"`, so a message written as a sentence arrives as
	// "…never consulted by a single run is required" — grammatical nonsense at exactly the moment an
	// operator is trying to work out what they typed wrong. Measured live before this line existed; the
	// first version of this refusal did precisely that.
	rendered := out.MissingField + " is required"
	if strings.Contains(rendered, "run is required") {
		t.Fatalf("the operator reads %q — the message is a sentence, and its renderer appends a predicate to it", rendered)
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

// AND ONE LEVEL DOWN, THE SAME SHAPE. Found while RESTORING the live stack's route after the provider
// smoke, 2026-08-01:
//
//	GET /v1/model-routes/{id}/revisions  ->  rev 1 published  claude-sonnet-5
//	                                         rev 2 published  claude-sonnet-5
//	                                         rev 3 published  gpt-4o-mini
//	                                         rev 4 published  claude-sonnet-5
//
// Four rows, every one of them `"published": true`, and exactly ONE of them steers a run —
// ResolveProjectModelRoute takes `ORDER BY rev.revision DESC, rev.id DESC LIMIT 1` over the published
// set. Publishing does not un-publish, so "published" accumulates and stops distinguishing anything.
//
// An operator reading that list to answer "which model am I on" reads four answers, two of which are
// different models. `published` is a fact about a revision's history; `resolved_by_dispatch` is the fact
// they came for.
func TestExactlyOneRevisionIsMarkedAsTheOneDispatchResolves(t *testing.T) {
	// The order the spine returns (ORDER BY revision, ascending) — deliberately NOT the order dispatch
	// selects in, so a projection that just marked the last row it saw would pass by accident.
	revs := []coordinator.ModelRouteRevision{
		{ID: "mrev_1", Revision: 1, Model: "claude-sonnet-5", Published: true},
		{ID: "mrev_2", Revision: 2, Model: "claude-sonnet-5", Published: true},
		{ID: "mrev_4", Revision: 4, Model: "claude-sonnet-5", Published: true},
		{ID: "mrev_5", Revision: 5, Model: "gpt-4o-mini", Published: false}, // a draft steers nothing
		{ID: "mrev_3", Revision: 3, Model: "gpt-4o-mini", Published: true},
	}
	marked := map[string]bool{}
	for _, v := range modelRouteRevisionViews("mroute_1", revs) {
		if v["resolved_by_dispatch"] == true {
			marked[v["id"].(string)] = true
		}
	}
	if len(marked) != 1 {
		t.Fatalf("%d revisions marked as the one dispatch resolves (%v); a list where every published row "+
			"looks live answers 'which model am I on' four times", len(marked), marked)
	}
	// THE SAME RULE THE SQL USES: highest revision among the PUBLISHED, ties broken by id descending.
	// mrev_5 is higher but a draft; mrev_4 is the highest published.
	if !marked["mrev_4"] {
		t.Fatalf("marked %v, want mrev_4 — ResolveProjectModelRoute is ORDER BY revision DESC, id DESC over "+
			"the PUBLISHED set, and a projection that disagrees with it tells the operator the wrong model", marked)
	}

	// A route with no published revision marks NOTHING — there is no resolved revision to name, and
	// marking the newest draft would show a model no run will use.
	drafts := []coordinator.ModelRouteRevision{{ID: "mrev_9", Revision: 9, Model: "m", Published: false}}
	for _, v := range modelRouteRevisionViews("mroute_1", drafts) {
		if v["resolved_by_dispatch"] == true {
			t.Fatal("a DRAFT was marked as the revision dispatch resolves")
		}
	}
}
