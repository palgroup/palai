package execution

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/mcp"
	"github.com/palgroup/palai/adapters/models/fake"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// capturedStep records one journalled model_step event so a test can assert the sampling step is visible and
// tagged source:"mcp_sampling".
type capturedStep struct {
	eventType string
	payload   map[string]any
}

// samplingJournal captures the events RouteSampling journals (the production emit closes over the coordinator
// spine; the unit test needs no Postgres to prove routing + budget + visibility).
type samplingJournal struct {
	mu    sync.Mutex
	steps []capturedStep
}

func (j *samplingJournal) emit(_ context.Context, _ mcp.CallScope, eventType string, payload []byte) error {
	var p map[string]any
	_ = json.Unmarshal(payload, &p)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.steps = append(j.steps, capturedStep{eventType: eventType, payload: p})
	return nil
}

func (j *samplingJournal) find(eventType string) (capturedStep, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, s := range j.steps {
		if s.eventType == eventType {
			return s, true
		}
	}
	return capturedStep{}, false
}

// brokerWithUsage builds a broker whose fake provider reports a fixed token usage — the lever a budget test
// pulls to force (or clear) the Admit cutoff.
func brokerWithUsage(totalTokens int) (*modelbroker.Broker, ModelRoute) {
	adapter := fake.Adapter{Script: fake.Script{
		ProviderRequestID: "fake-sampling-req",
		Model:             "fake-sampling-model",
		Output:            "sampled reply",
		Usage:             contracts.Usage{TotalTokens: totalTokens},
	}}
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"fake": adapter},
		Secrets:  modelbroker.StaticResolver{modelbroker.SecretRef("fake"): "unused"},
	})
	return broker, ModelRoute{Provider: "fake", Model: "fake", Secret: modelbroker.SecretRef("fake")}
}

const samplingParams = `{"messages":[{"role":"user","content":{"type":"text","text":"summarize"}}]}`

// TestSamplingEnabledRoutesBrokeredBudgetedVisibleStep proves TOL-010 half 2: an enabled sampling request is
// routed through the broker as a SEPARATE budgeted model step and shows up as visible model_step.created +
// model_step.completed events tagged source:"mcp_sampling" — and, crucially, that the SEPARATE Reservation
// actually CUTS OFF at Admit when the sampling call runs past the connection's budget.
func TestSamplingEnabledRoutesBrokeredBudgetedVisibleStep(t *testing.T) {
	ctx := context.Background()
	scope := mcp.CallScope{Org: "org", Project: "proj", SessionID: "sess", ResponseID: "resp", RunID: "run", CallID: "call"}

	// Within budget: the step is routed and both events are visible + tagged, and the result carries the
	// provider's completion text.
	t.Run("within budget routes a visible step", func(t *testing.T) {
		broker, route := brokerWithUsage(10)
		j := &samplingJournal{}
		r := NewMCPSamplingRouter(broker, route, j.emit)
		conn := mcp.ConnConfig{ID: "mcpc_1", SamplingEnabled: true, SamplingMaxTokens: 100}

		out, err := r.RouteSampling(ctx, scope, conn, json.RawMessage(samplingParams))
		if err != nil {
			t.Fatalf("RouteSampling within budget: %v", err)
		}
		var result struct {
			Role    string `json:"role"`
			Model   string `json:"model"`
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(out, &result) != nil || result.Content.Text != "sampled reply" || result.Role != "assistant" {
			t.Fatalf("sampling result = %s, want an assistant text completion", out)
		}
		created, ok := j.find(eventModelStepCreated)
		if !ok || created.payload["source"] != "mcp_sampling" || created.payload["connection_id"] != "mcpc_1" {
			t.Fatalf("created event = %+v, want source:mcp_sampling + connection_id", created.payload)
		}
		completed, ok := j.find(eventModelStepCompleted)
		if !ok || completed.payload["source"] != "mcp_sampling" || completed.payload["provider_request_id"] != "fake-sampling-req" {
			t.Fatalf("completed event = %+v, want source:mcp_sampling + provider_request_id", completed.payload)
		}
		if _, denied := completed.payload["denied"]; denied {
			t.Fatalf("within-budget completed event is marked denied: %+v", completed.payload)
		}
	})

	// Over budget: the SEPARATE Reservation cuts off at Admit — RouteSampling denies with budget_exceeded and
	// the completed event records the denial (the gate turns this into a JSON-RPC error to the server).
	t.Run("over budget is cut off at Admit", func(t *testing.T) {
		broker, route := brokerWithUsage(500)
		j := &samplingJournal{}
		r := NewMCPSamplingRouter(broker, route, j.emit)
		conn := mcp.ConnConfig{ID: "mcpc_1", SamplingEnabled: true, SamplingMaxTokens: 1}

		if _, err := r.RouteSampling(ctx, scope, conn, json.RawMessage(samplingParams)); err == nil {
			t.Fatal("RouteSampling over budget returned nil, want a budget cutoff")
		} else if !strings.Contains(err.Error(), "budget_exceeded") {
			t.Fatalf("over-budget err = %v, want budget_exceeded", err)
		}
		completed, ok := j.find(eventModelStepCompleted)
		if !ok || completed.payload["denied"] != true || completed.payload["reason"] != "budget_exceeded" {
			t.Fatalf("completed event = %+v, want denied:true reason:budget_exceeded", completed.payload)
		}
	})
}

// TestSamplingProviderErrorIsNotACompletion is the sampling twin of the dispatch guard (E13 T8 review
// SHOULD 1): a provider-side rejection rides on modelbroker.Result.Error, NOT on the Go error, so without a
// guard RouteSampling journals model_step.completed as a SUCCESS and hands the MCP server an assistant
// message with EMPTY text — a silent wrong answer. The call must fail, and the completed event must record
// the denial rather than claim a completion.
func TestSamplingProviderErrorIsNotACompletion(t *testing.T) {
	ctx := context.Background()
	scope := mcp.CallScope{Org: "org", Project: "proj", SessionID: "sess", ResponseID: "resp", RunID: "run", CallID: "call"}
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"fake": fake.Adapter{Script: fake.Script{
			Model: "fake-sampling-model",
			Err:   &modelbroker.SanitizedError{Code: "provider_error", Message: "upstream declined", Status: 429},
		}}},
		Secrets: modelbroker.StaticResolver{modelbroker.SecretRef("fake"): "unused"},
	})
	j := &samplingJournal{}
	r := NewMCPSamplingRouter(broker, ModelRoute{Provider: "fake", Model: "fake", Secret: "fake"}, j.emit)
	conn := mcp.ConnConfig{ID: "mcpc_1", SamplingEnabled: true, SamplingMaxTokens: 100}

	out, err := r.RouteSampling(ctx, scope, conn, json.RawMessage(samplingParams))
	if err == nil {
		t.Fatalf("RouteSampling returned a completion for a provider-rejected call: %s", out)
	}
	if !strings.Contains(err.Error(), "provider_error") {
		t.Fatalf("sampling error = %v, want the sanitized provider rejection", err)
	}
	completed, ok := j.find(eventModelStepCompleted)
	if !ok {
		t.Fatal("no model_step.completed event was journalled — the rejected step must stay visible")
	}
	if denied, _ := completed.payload["denied"].(bool); !denied {
		t.Fatalf("completed event = %+v, want denied:true for a provider rejection", completed.payload)
	}
}
