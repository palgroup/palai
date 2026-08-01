package toolbroker

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

// TestAnUnclassifiedErrorIsNotAnAnswer is the property the whole distinction rests on: the safe
// direction is the DEFAULT. An error nobody wrapped must not be readable as an answer, because that is
// what keeps a fence violation and a lost database connection out of the model's transcript.
func TestAnUnclassifiedErrorIsNotAnAnswer(t *testing.T) {
	for name, err := range map[string]error{
		"a bare error":      errors.New("connection reset by peer"),
		"a wrapped error":   errors.Join(errors.New("outer"), errors.New("inner")),
		"a stdlib fs error": &fs.PathError{Op: "open", Path: "/x", Err: fs.ErrPermission},
	} {
		if _, ok := AsAnswer(err); ok {
			t.Fatalf("%s was classified as an answer; only an explicit toolbroker.Answer may be one", name)
		}
		if errors.Is(err, ErrToolAnswer) {
			t.Fatalf("%s matched ErrToolAnswer", name)
		}
	}
}

// TestAnAnswerKeepsItsCauseReachable proves the wrapper does not destroy what it wraps: a caller can
// still ask WHAT failed. Without this the file tool could not name a missing file `not_found` and a
// refused traversal `refused` from the same errors.Is chain.
func TestAnAnswerKeepsItsCauseReachable(t *testing.T) {
	cause := &fs.PathError{Op: "open", Path: "README", Err: fs.ErrNotExist}
	err := Answer(AnswerNotFound, cause)

	answer, ok := AsAnswer(err)
	if !ok {
		t.Fatal("AsAnswer(Answer(...)) = false, want true")
	}
	if answer.Code != AnswerNotFound {
		t.Fatalf("code = %q, want %q", answer.Code, AnswerNotFound)
	}
	if !errors.Is(err, ErrToolAnswer) {
		t.Fatal("an answer does not match its own sentinel")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("errors.Is(answer, fs.ErrNotExist) = false; the cause must stay reachable through the wrapper")
	}
	if answer.Error() != cause.Error() {
		t.Fatalf("Error() = %q, want the cause's own text %q", answer.Error(), cause.Error())
	}
}

// TestAnAnswerSurvivesTheBrokersOwnWrapping is the seam that would silently undo this: Broker.Execute
// wraps an invoke error with fmt.Errorf("tool %s: %w", …), and the dispatcher tests the WRAPPED value.
func TestAnAnswerSurvivesTheBrokersOwnWrapping(t *testing.T) {
	b := New(Tool{
		Name: "t", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		Invoke: func(map[string]any) (map[string]any, error) {
			return nil, Answerf(AnswerRefused, "refusing to read %q", "/etc/passwd")
		},
	})
	_, err := b.Execute(context.Background(), contracts.ToolCallID("tc_1"), "t", map[string]any{}, 1, ExecEnv{})
	answer, ok := AsAnswer(err)
	if !ok {
		t.Fatalf("AsAnswer(Execute error) = false, want true: %v", err)
	}
	if answer.Code != AnswerRefused {
		t.Fatalf("code through Execute = %q, want %q", answer.Code, AnswerRefused)
	}
}

// TestTheBrokerAnswersAnUnknownToolAndBadArgumentsButNotABadOutput pins the broker's own three
// classifications, and the third is the one worth reading: an OUTPUT-schema failure happens AFTER the
// tool ran, so it is a fault however harmless it looks beside its input-side twin.
func TestTheBrokerAnswersAnUnknownToolAndBadArgumentsButNotABadOutput(t *testing.T) {
	ctx := context.Background()
	b := New(Tool{
		Name: "strict",
		InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"n": map[string]any{"type": "number"}},
			"required": []any{"n"},
		},
		OutputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"out": map[string]any{"type": "number"}},
			"required": []any{"out"},
		},
		Invoke: func(map[string]any) (map[string]any, error) { return map[string]any{"wrong": true}, nil },
	})

	_, err := b.Execute(ctx, contracts.ToolCallID("tc_unknown"), "no-such-tool", map[string]any{}, 1, ExecEnv{})
	answer, ok := AsAnswer(err)
	if !ok || answer.Code != AnswerUnknownTool {
		t.Fatalf("unknown tool = (%v, %v), want an %q answer", err, ok, AnswerUnknownTool)
	}
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatal("the unknown-tool answer no longer matches ErrUnknownTool; existing callers key on it")
	}

	_, err = b.Execute(ctx, contracts.ToolCallID("tc_badargs"), "strict", map[string]any{}, 1, ExecEnv{})
	answer, ok = AsAnswer(err)
	if !ok || answer.Code != AnswerInvalidArguments {
		t.Fatalf("bad input = (%v, %v), want an %q answer", err, ok, AnswerInvalidArguments)
	}

	_, err = b.Execute(ctx, contracts.ToolCallID("tc_badout"), "strict", map[string]any{"n": 1}, 1, ExecEnv{})
	if err == nil {
		t.Fatal("a schema-violating OUTPUT was accepted")
	}
	if _, ok := AsAnswer(err); ok {
		t.Fatalf("a bad OUTPUT was classified as an answer: %v — the tool already ran, so it is uncertain", err)
	}
}

// TestAFailedOutcomeStillCarriesTheToolsClassAndNoPersistProperty is small and load-bearing: the
// dispatcher commits an answer onto the SAME ledger row a success would have taken, and it takes both
// of these off the outcome. A dropped class would write `pure` onto a reversible tool's row.
func TestAFailedOutcomeStillCarriesTheToolsClassAndNoPersistProperty(t *testing.T) {
	b := New(Tool{
		Name: "rev", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ReplayClass: ClassReversible, Unretained: true,
		Invoke: func(map[string]any) (map[string]any, error) { return nil, Answerf(AnswerFailed, "no") },
	})
	outcome, err := b.Execute(context.Background(), contracts.ToolCallID("tc_1"), "rev", map[string]any{}, 1, ExecEnv{})
	if err == nil {
		t.Fatal("Execute returned no error")
	}
	if outcome.ReplayClass != ClassReversible {
		t.Fatalf("failed outcome ReplayClass = %q, want %q", outcome.ReplayClass, ClassReversible)
	}
	if !outcome.Unretained {
		t.Fatal("failed outcome dropped Unretained; the refusal would be written down for a tool that forbids it")
	}
}

// TestAnswerRefusesToBuildAnEmptyAnswer keeps a nil cause from becoming an answer with nothing in it —
// which would deliver the model a refusal it cannot read and would count against the budget.
func TestAnswerRefusesToBuildAnEmptyAnswer(t *testing.T) {
	if err := Answer(AnswerNotFound, nil); err != nil {
		t.Fatalf("Answer(code, nil) = %v, want nil", err)
	}
	if answer, _ := AsAnswer(Answer("", errors.New("x"))); answer.Code != AnswerFailed {
		t.Fatalf("Answer(\"\", err) code = %q, want the %q fallback", answer.Code, AnswerFailed)
	}
}
