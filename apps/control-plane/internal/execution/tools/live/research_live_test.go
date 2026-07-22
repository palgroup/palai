//go:build live

// CASE=web-research-citations (E12 Task 3, EXT-004 / TOL-015): a real provider-one run resolves a
// published AgentRevision whose tool ceiling is the research tool ONLY (no publish tool), builds the
// real request from that spec, FORCE-calls the research tool, and the tool performs a REAL internet
// fetch of a public URL — returning citations (final URL, title, retrieved_at, content_hash over the
// raw bytes). The negative half: the resolved effective set excludes the publish tool, so a publish
// capability is never advertised (capability never expands, journey 63.4).
//
// HONEST CEILING (mandatory, spec §10.2 discipline): the orchestrator's dispatchModel does NOT
// advertise tool schemas to the provider (the same gate the E10/E11 live tool cases sit behind). So the
// SPONTANEOUS-call half of the plan is proven by REQUEST CONSTRUCTION + a FORCED call (the E09 T4 broker
// seam) — the evidence carries "forced (T1 pending)". When E12 Task 1's tool-advertising lands in main,
// this upgrades to spontaneous choice. What is proven live NOW: real provider → FORCED research call →
// REAL egress fetch of a public URL → citations, and the publish tool never reaching the request (the
// ceiling excludes it). The credential is an opaque needle for the leak scan and is never printed.

package live

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	tools "github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

const (
	researchToolNameLive = "palai.research.fetch"
	publishToolNameLive  = "palai.publish.push"
	researchPublicURL    = "https://example.com"
)

// TestLiveWebResearchCitations resolves a research-only AgentRevision ceiling, force-calls the research
// tool on a real provider, executes a REAL internet fetch through the production broker seam, and
// asserts the citation shape — while proving the publish tool never reaches the request.
func TestLiveWebResearchCitations(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}

	// A published AgentRevision ceilings the tool set to the research tool ONLY; the project baseline
	// ALSO offers a publish tool the revision does not declare. The effective set admits research, never
	// publish — the capability-never-expands negative half, resolved by the exact production pure fn.
	snap := execution.Resolve(execution.ResolveInput{
		DeploymentModel:    "deployment-default-not-used",
		ProjectTools:       []string{researchToolNameLive, publishToolNameLive},
		AgentRevisionID:    "arev_research_live",
		AgentRevisionModel: liveModel(),
		AgentRevisionTools: []string{researchToolNameLive}, // ceiling: research only, publish excluded
	})
	for _, tool := range snap.Tools {
		if tool == publishToolNameLive {
			t.Fatalf("resolved tools = %v; the ceiling must exclude the publish tool", snap.Tools)
		}
	}

	// Build the REAL provider request from the resolved spec. Only the research tool's schema is
	// advertised (request-construction ceiling); the publish tool never reaches the wire.
	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_web_research"),
		RouteRevision:  1, ModelStepID: "step-research", Model: snap.Model,
		Messages:      []modelbroker.Message{{Role: "user", Content: "Fetch the page at " + researchPublicURL + " and cite it. Call the research tool with that URL."}},
		Tools:         researchSchemasForCeiling(snap.Tools),
		ForceToolCall: true,
		Deadline:      time.Now().Add(60 * time.Second),
		Reservation:   modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:        modelbroker.SecretRef("provider-one"),
	}
	for _, ts := range req.Tools {
		if ts.Name == publishToolNameLive {
			t.Fatalf("the provider request advertised the publish tool — capability expanded past the ceiling")
		}
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != researchToolNameLive {
		t.Fatalf("advertised tools = %v, want only the research tool", req.Tools)
	}

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	var streamed strings.Builder
	stream := func(d modelbroker.Delta) {
		streamed.WriteString(d.Text)
		if d.ToolCall != nil {
			streamed.WriteString(d.ToolCall.Name)
			streamed.WriteString(d.ToolCall.ArgumentsFragment)
		}
	}
	res, err := broker.Route(context.Background(), "provider-one", req, stream)
	if err != nil {
		t.Fatalf("route research turn: %v", err)
	}
	assertRealCompletion(t, res, "web-research")
	call := requireToolCall(t, res, "web-research")
	if call.Name != researchToolNameLive {
		t.Fatalf("forced call = %q, want the research tool", call.Name)
	}

	// Execute the research tool through the production broker seam — a REAL internet fetch (production
	// resolver/dialer/system roots, NO injected seam). Use the model's URL when it supplied a usable
	// https one; otherwise the known public URL (the fetch+citation claim is independent of the exact arg).
	args := decodeArgs(t, call.Arguments)
	if u, _ := args["url"].(string); !strings.HasPrefix(u, "https://") {
		args["url"] = researchPublicURL
	}
	tb := toolbroker.New(tools.ResearchFetchTool())
	out, err := tb.Execute(context.Background(), contracts.ToolCallID("tc_research_1"), researchToolNameLive, args, 1, toolbroker.ExecEnv{})
	if err != nil {
		t.Fatalf("execute research tool (real fetch): %v", err)
	}
	cites, ok := out.Result["citations"].([]any)
	if !ok || len(cites) == 0 {
		t.Fatalf("research result carried no citations: %v", out.Result)
	}
	cite := cites[0].(map[string]any)
	citeURL, _ := cite["url"].(string)
	if !strings.HasPrefix(citeURL, "https://") {
		t.Fatalf("citation url = %v, want the fetched https URL", cite["url"])
	}
	if ch, _ := cite["content_hash"].(string); !strings.HasPrefix(ch, "sha256:") {
		t.Fatalf("citation content_hash = %v, want sha256 over the fetched bytes", cite["content_hash"])
	}
	if excerpt, _ := out.Result["excerpt"].(string); excerpt == "" {
		t.Fatal("research excerpt is empty, want the extracted page text")
	}

	// Leak scan by construction: the credential appears in no captured surface.
	for name, captured := range map[string]string{
		"streamed deltas": streamed.String(),
		"research result": string(mustJSON(out.Result)),
	} {
		if strings.Contains(captured, secret) {
			t.Fatalf("%s contains the credential value", name)
		}
	}

	t.Logf("live web-research citations PASS (real provider, FORCED research call — T1 pending, not spontaneous): "+
		"turn=%s tool=%s fetched=%s content_hash=%s publish_excluded=true model=%s",
		safePrefix(res.ProviderRequestID), call.Name, citeURL, safePrefix(cite["content_hash"].(string)), res.Model)
}

// researchSchemasForCeiling advertises a schema only for the research tool (the ceiling in this smoke);
// a tool outside the ceiling has no schema, so it can never reach the request.
func researchSchemasForCeiling(toolsList []string) []modelbroker.ToolSchema {
	out := make([]modelbroker.ToolSchema, 0, len(toolsList))
	for _, tool := range toolsList {
		if tool == researchToolNameLive {
			out = append(out, researchToolSchema())
		}
	}
	return out
}

func researchToolSchema() modelbroker.ToolSchema {
	return modelbroker.ToolSchema{
		Name:        researchToolNameLive,
		Description: "Fetch a single URL and return a bounded text excerpt plus a citation. Provide the url to fetch.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":       map[string]any{"type": "string"},
				"max_bytes": map[string]any{"type": "number"},
			},
			"required":             []any{"url"},
			"additionalProperties": false,
		},
	}
}
