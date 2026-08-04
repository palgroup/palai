//go:build live

// The Anthropic REASONING live smoke. It settles the one claim the thinking work cannot settle against a
// fake: that `thinking: {"type":"adaptive","display":"summarized"}` is the shape the live API accepts AND
// that it is what makes the model's reasoning readable.
//
// WHY THIS FILE IS A DIFFERENTIAL AND NOT A SINGLE ASSERTION. A test that only asked for reasoning and
// found some would pass just as happily against an adapter that sent no `thinking` key at all — because
// an adaptive model reasons either way, and the question is never "did it think" but "were we shown it".
// Anthropic's `display` defaults to "omitted", under which the response still carries thinking blocks and
// still bills for them while their text is the empty string
// (https://platform.claude.com/docs/en/build-with-claude/adaptive-thinking, checked 2026-08-04). So the
// proof has to be two runs of the SAME prompt against the SAME model differing only in the canonical mode:
// the asked leg must carry reasoning and the unasked leg must not. Delete `display` from the adapter and
// the asked leg goes empty; delete the whole `thinking` key and it goes empty too. Neither deletion can
// leave this file green.
//
// It also prints both legs' token usage, because the cost of asking is a fact an operator has to be able
// to look up rather than infer — see the usage log at the end of each leg.
package provider_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	providertwo "github.com/palgroup/palai/adapters/models/provider_two"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// thinkingPrompt is chosen to make an ADAPTIVE model actually reason, which a trivial prompt does not:
// measured 2026-08-04, claude-sonnet-5 answers simple arithmetic with no thinking block at all, so a test
// built on one would fail for a reason unrelated to what it asserts. This is a constraint-satisfaction
// puzzle with a single consistent answer that cannot be reached by pattern match.
const thinkingPrompt = "Three boxes are labelled A, B and C. Exactly one holds a prize. " +
	"Label A says 'the prize is not here'. Label B says 'the prize is here'. Label C says 'the prize is not in B'. " +
	"Exactly one of the three labels tells the truth. Which box holds the prize? " +
	"End your reply with the single letter on its own line."

// thinkingModelTwo matches the DEPLOYMENT's published route rather than picking a cheap model, for the same
// reason the vision leg does: proving reasoning on a model the operator does not route to would prove the
// wire shape while leaving the operator's own path unproven. prj_local's `default` route is claude-sonnet-5
// over a provider-two connection (measured 2026-08-04).
func thinkingModelTwo() string {
	if m := os.Getenv("PALAI_LIVE_MODEL_TWO"); m != "" {
		return m
	}
	return "claude-sonnet-5"
}

// runThinkingLeg sends the shared prompt once under the given canonical mode and reports what came back:
// the accumulated reasoning, whether any reasoning arrived as a STREAMED fragment, and the answer text.
//
// The streamed-fragment half matters on its own. Result.Thinking could in principle be assembled by an
// adapter that read the final message body, and the journal is not fed by Result — it is fed by the delta
// callback (model_dispatch.go's thinking sink). Asserting a fragment reached this callback is what ties the
// live wire to the event the operator actually reads.
func runThinkingLeg(t *testing.T, mode modelbroker.ThinkingMode) (thinking string, streamedFragments int, answer string, usage contracts.Usage) {
	t.Helper()

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-two": providertwo.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-two": anthropicCredentialEnv},
	})

	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_live_two_thinking_" + string(mode) + "x"),
		RouteRevision:  1,
		ModelStepID:    "step-1",
		Model:          thinkingModelTwo(),
		Thinking:       mode,
		Messages:       []modelbroker.Message{{Role: "user", Content: thinkingPrompt}},
		Deadline:       time.Now().Add(120 * time.Second),
		// Generous on purpose: max_tokens is a hard cap on thinking AND answer combined, so a budget sized
		// for the answer alone truncates the moment the model starts reasoning.
		Reservation: modelbroker.Reservation{MaxTotalTokens: 8000},
		Secret:      modelbroker.SecretRef("provider-two"),
	}

	var streamedThinking strings.Builder
	res, err := broker.Route(context.Background(), "provider-two", req, func(d modelbroker.Delta) {
		if d.Thinking != "" {
			streamedFragments++
			streamedThinking.WriteString(d.Thinking)
		}
	})
	if err != nil {
		t.Fatalf("Route(%q) error = %v", mode, err)
	}
	if res.Error != nil {
		t.Fatalf("Route(%q) sanitized error = %+v", mode, res.Error)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("Route(%q) result violates the canonical contract: %v", mode, err)
	}

	// The accumulated field and the streamed fragments must agree. If they ever diverge, one of the two is
	// being assembled from a source the other does not see, and the journal follows the streamed half.
	if got := streamedThinking.String(); got != res.Thinking {
		t.Fatalf("Route(%q): streamed reasoning (%d bytes) != Result.Thinking (%d bytes); the journal is fed by the streamed half, so a divergence means the event and the result disagree",
			mode, len(got), len(res.Thinking))
	}

	t.Logf("mode=%q model=%s input=%d output=%d total=%d thinking_bytes=%d fragments=%d",
		mode, res.Model, res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.TotalTokens,
		len(res.Thinking), streamedFragments)
	return res.Thinking, streamedFragments, res.Output, res.Usage
}

// TestLiveProviderTwoReturnsVisibleReasoningOnlyWhenAsked is CASE=thinking-visible: two real streamed
// Anthropic messages, same prompt, same model, differing only in the canonical ThinkingMode.
//
// WHAT THE ASSERTION IS WORTH. The asked leg proves the wire shape is accepted and the reasoning is
// readable; the unasked leg proves the asking is what did it. Together they refuse the two ways this
// feature could be fake — a request that never carried the key, and a key that changed nothing.
func TestLiveProviderTwoReturnsVisibleReasoningOnlyWhenAsked(t *testing.T) {
	if os.Getenv(anthropicCredentialEnv) == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", anthropicCredentialEnv)
	}

	visible, fragments, answer, visibleUsage := runThinkingLeg(t, modelbroker.ThinkingVisible)
	if strings.TrimSpace(visible) == "" {
		t.Fatalf("asked for visible reasoning and got none; the adapter's thinking key is not reaching the wire, or `display` is not summarized")
	}
	if fragments == 0 {
		t.Fatalf("Result.Thinking carries %d bytes but no reasoning arrived as a streamed fragment; the journal is fed by the stream, so this reasoning would never become a model_step.thinking.v1 event", len(visible))
	}
	// The reasoning must be the model's WORKING, not a copy of its answer. A one-letter answer echoed into
	// the thinking field would satisfy a bare non-empty check while carrying nothing a person would read.
	if len(visible) < 80 {
		t.Fatalf("reasoning is only %d bytes (%q) — too short to be the model's working", len(visible), visible)
	}
	if strings.TrimSpace(answer) == "" {
		t.Fatalf("asked leg produced reasoning but no answer text; reasoning must not consume the answer")
	}

	silent, silentFragments, silentAnswer, silentUsage := runThinkingLeg(t, modelbroker.ThinkingDefault)
	if strings.TrimSpace(silent) != "" {
		t.Fatalf("did not ask for reasoning but got %d bytes of it: %q\n"+
			"the differential this case rests on is gone — either the adapter now sends the thinking key unconditionally, or the provider changed its default `display`",
			len(silent), silent)
	}
	if silentFragments != 0 {
		t.Fatalf("did not ask for reasoning but %d reasoning fragments streamed", silentFragments)
	}
	if strings.TrimSpace(silentAnswer) == "" {
		t.Fatalf("unasked leg produced no answer at all, so the two legs are not comparable")
	}

	// The cost of asking, recorded rather than asserted: output tokens cover the reasoning either way (the
	// model reasons under both modes and is billed for it under both), so this is the honest number for an
	// operator deciding whether to turn the ask on — not a threshold, because it moves per request.
	t.Logf("COST asked: in=%d out=%d total=%d | unasked: in=%d out=%d total=%d",
		visibleUsage.InputTokens, visibleUsage.OutputTokens, visibleUsage.TotalTokens,
		silentUsage.InputTokens, silentUsage.OutputTokens, silentUsage.TotalTokens)
	t.Logf("REASONING (first 300 bytes): %s", firstBytes(visible, 300))
	t.Logf("ANSWER: %s", strings.TrimSpace(answer))
}

func firstBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
