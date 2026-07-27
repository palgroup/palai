package execution

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// TestContextOverflowIsClassifiedFromTheProviderCode. Compaction makes an overflow rare, not
// impossible — the budget is a BYTE estimate, not the provider's tokenizer — so the overflow that
// gets through must still be nameable. Until this task the tree had no way to name it: grepped on
// 2026-07-27, no string of the context_length_exceeded class appeared anywhere in the repo, so an
// overflow arrived as an unclassified upstream failure, was retried to the dead-letter ceiling, and
// projected the same "Internal error" as every other fault.
//
// The classification keys on the provider's stable error CODE and nothing else. That is not a
// preference: both adapters' sanitizeError deliberately throw the provider's free text away (it can
// echo a credential prefix) and substitute "provider returned HTTP N", so the message carries no
// signal to match on even if we wanted to.
func TestContextOverflowIsClassifiedFromTheProviderCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *modelbroker.SanitizedError
		want bool
	}{
		{"provider-one context window", &modelbroker.SanitizedError{Code: "context_length_exceeded", Status: 400}, true},
		{"provider-two request too large", &modelbroker.SanitizedError{Code: "request_too_large", Status: 413}, true},
		{"a rate limit is not an overflow", &modelbroker.SanitizedError{Code: "rate_limit_error", Status: 429}, false},
		{"an auth failure is not an overflow", &modelbroker.SanitizedError{Code: "authentication_error", Status: 401}, false},
		{"an unparsed provider error is not an overflow", &modelbroker.SanitizedError{Code: "provider_error", Status: 500}, false},
		// The named ceiling: Anthropic returns a plain 400 invalid_request_error for a prompt over the
		// context window — the SAME type as every other malformed request (docs fetched 2026-07-27).
		// Classifying it would fail every provider-two 400 as an overflow, so it stays unclassified.
		{"provider-two invalid_request stays unclassified", &modelbroker.SanitizedError{Code: "invalid_request_error", Status: 400}, false},
		{"no error at all", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isContextOverflow(tc.err); got != tc.want {
				t.Fatalf("isContextOverflow(%+v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestContextOverflowErrorIsRecognisableThroughWrapping: the classification is made where the
// provider result is read (model_dispatch) and acted on where the run loop decides an attempt's
// fate (orchestrator). Between them the error is wrapped with the request id, so errors.Is must
// still see it — a sentinel that does not survive the wrap is a sentinel nobody checks.
func TestContextOverflowErrorIsRecognisableThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("%w: %w", errContextOverflow,
		fmt.Errorf("model request mr_1: provider_error: provider returned HTTP 400 (code context_length_exceeded, status 400)"))
	if !errors.Is(wrapped, errContextOverflow) {
		t.Fatal("the wrapped provider failure no longer reports as a context overflow")
	}
	if errors.Is(errors.New("route model request mr_1: dial tcp: connection refused"), errContextOverflow) {
		t.Fatal("an unrelated transport failure reported as a context overflow")
	}
}

// TestContextOverflowProjectsADiagnosableTerminal is the half a person actually sees. Slack's
// failure line is "The run failed before it could answer." plus the projected problem's TITLE
// (renderSlackReply, extensions/slack_reply.go), so a run that died of an oversized conversation
// must not project the same generic title as a crashed engine — that is precisely the
// undiagnosable state this bullet exists to end.
func TestContextOverflowProjectsADiagnosableTerminal(t *testing.T) {
	generic := terminalProblem("failed")
	overflow := contextOverflowProblem()

	if overflow.Title == generic.Title {
		t.Fatalf("a context overflow projects the same title as any other failure (%q); the reader learns nothing", overflow.Title)
	}
	if overflow.Code == "" || overflow.Type == "" {
		t.Fatalf("context-overflow problem = %+v, want a stable code and a dereferenceable type", overflow)
	}
	// The detail must name what the person can DO. Both remedies are real and both are this task's:
	// a new thread is a new session, and clear resets the one they are in.
	if !strings.Contains(overflow.Detail, "clear") {
		t.Fatalf("context-overflow detail = %q, want it to name the clear command — a diagnosis with no remedy is half an answer", overflow.Detail)
	}
}
