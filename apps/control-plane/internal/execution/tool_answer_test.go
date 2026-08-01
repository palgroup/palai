package execution

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TestTheToolErrorBudgetDefaultsRatherThanUnboundsItself covers the failure mode a numeric env var has:
// a typo silently becoming "no limit". Unset is the shipped default; `0` is unbounded and has to be
// WRITTEN, which is the idiom PALAI_BACKGROUND_MAX_WALL_TIME already uses in this tree; anything
// unparseable falls back to the default rather than to the permissive reading.
func TestTheToolErrorBudgetDefaultsRatherThanUnboundsItself(t *testing.T) {
	t.Setenv("PALAI_TOOL_ERROR_BUDGET", "")
	// An EMPTY value is set-but-unparseable, which is exactly what an operator leaves behind after
	// deleting a number, and it must not read as unbounded.
	if got := toolAnswerErrorBudget(); got != defaultToolAnswerErrorBudget {
		t.Fatalf("empty budget = %d, want the %d default", got, defaultToolAnswerErrorBudget)
	}
	for _, raw := range []string{"not-a-number", "-1", "12x"} {
		t.Setenv("PALAI_TOOL_ERROR_BUDGET", raw)
		if got := toolAnswerErrorBudget(); got != defaultToolAnswerErrorBudget {
			t.Fatalf("budget %q = %d, want the %d default", raw, got, defaultToolAnswerErrorBudget)
		}
	}
	t.Setenv("PALAI_TOOL_ERROR_BUDGET", "3")
	if got := toolAnswerErrorBudget(); got != 3 {
		t.Fatalf("budget = %d, want 3", got)
	}
	if !exceedsToolAnswerErrorBudget(4) || exceedsToolAnswerErrorBudget(3) {
		t.Fatal("the budget is off by one: 3 must be inside it and 4 outside")
	}
	t.Setenv("PALAI_TOOL_ERROR_BUDGET", "0")
	if exceedsToolAnswerErrorBudget(1_000_000) {
		t.Fatal("0 must mean unbounded — an operator typed it")
	}
}

// TestABudgetlessDeploymentIsBoundedAtSixteen reads the number production runs with when nobody has
// configured anything, through the SAME function production calls. It is the composition-root reading
// this tree learned to make after a shell wall time was unbounded on the host while every sandbox test
// was green (docs/operations/palai-on-a-mac.md §1).
func TestABudgetlessDeploymentIsBoundedAtSixteen(t *testing.T) {
	// No t.Setenv: this asserts the value on a machine with nothing set, which is every fresh deployment.
	if _, set := os.LookupEnv("PALAI_TOOL_ERROR_BUDGET"); set {
		t.Skip("PALAI_TOOL_ERROR_BUDGET is set in this environment; the unset reading is not measurable here")
	}
	if got := toolAnswerErrorBudget(); got != 16 {
		t.Fatalf("a deployment that configures nothing is bounded at %d, want 16", got)
	}
	if !exceedsToolAnswerErrorBudget(17) {
		t.Fatal("the seventeenth refusal is not over the default budget")
	}
}

// TestARefusalIsDistinguishableFromASuccessThatHasAStatusField is the collision this shape was chosen
// to avoid: the background shell call's SUCCESS result is {task_id, output_path, status}. If the budget
// counted a top-level `status` alone it would charge every backgrounded command as a refusal.
func TestARefusalIsDistinguishableFromASuccessThatHasAStatusField(t *testing.T) {
	refusal, _ := json.Marshal(answerResult("palai.workspace.file",
		mustAnswer(toolbroker.Answerf(toolbroker.AnswerNotFound, "open README: no such file or directory")),
		toolbroker.ExecEnv{}))
	if !isAnswerResult(string(refusal)) {
		t.Fatalf("a refusal was not recognised: %s", refusal)
	}
	notRefusals := []string{
		`{"task_id":"bgt_1","output_path":"/w/x.log","status":"running"}`, // the background shell success
		`{"exit_code":1,"stdout":"","stderr":"","timed_out":false}`,       // `false`: a non-zero EXIT CODE
		`{"path":"repo/README","content":"Hello World!","size":13}`,       // an ordinary read
		`{"status":"error"}`,                    // status without a coded error object
		`{"status":"denied","reason":"policy"}`, // a before_tool DENY: not a tool refusal
		`not json at all`,
		``,
	}
	for _, raw := range notRefusals {
		if isAnswerResult(raw) {
			t.Fatalf("%s was counted as a tool refusal", raw)
		}
	}
}

// TestARefusalMessageIsRedactedOnItsWayToTheModelAndTheLedger. The message is about to be written to a
// durable tool_calls row AND read by a model — the same two destinations a shell result reaches — so it
// gets the same two redactors. Without the value-based one a tool error echoing a connection string
// would land verbatim in the database.
func TestARefusalMessageIsRedactedOnItsWayToTheModelAndTheLedger(t *testing.T) {
	const envValue = "s3cr3t-database-password"
	answer := mustAnswer(toolbroker.Answerf(toolbroker.AnswerFailed,
		"connect to postgres://palai:%s@db:5432 failed; also tried bearer abcdefghijklmnop", envValue))

	out := answerResult("some.tool", answer, toolbroker.ExecEnv{EnvValues: map[string]string{"DB_PASSWORD": envValue}})
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), envValue) {
		t.Fatalf("the attempt's own environment VALUE survived into the refusal: %s", raw)
	}
	if strings.Contains(string(raw), "bearer abcdefghijklmnop") {
		t.Fatalf("a bearer token survived into the refusal: %s", raw)
	}
	// Non-vacuity: the same message with NO environment values keeps its non-secret text, so the
	// assertions above are the redactors working rather than the message being empty.
	plain, _ := json.Marshal(answerResult("some.tool", answer, toolbroker.ExecEnv{}))
	if !strings.Contains(string(plain), "postgres://palai") {
		t.Fatalf("the message body was destroyed rather than redacted: %s", plain)
	}
	if !strings.Contains(string(plain), envValue) {
		t.Fatal("with no environment values declared there is nothing for RedactValues to mask; " +
			"if this fails the first assertion proves nothing")
	}
}

// TestTheRefusalNamesTheToolAndACode is what the model and an operator both read off it.
func TestTheRefusalNamesTheToolAndACode(t *testing.T) {
	out := answerResult("palai.workspace.file",
		mustAnswer(toolbroker.Answerf(toolbroker.AnswerRefused, "path escapes the workspace")), toolbroker.ExecEnv{})
	errObj, _ := out["error"].(map[string]any)
	if errObj["tool"] != "palai.workspace.file" || errObj["code"] != toolbroker.AnswerRefused {
		t.Fatalf("refusal = %+v, want it to name the tool and carry the code", out)
	}
}

func mustAnswer(err error) *toolbroker.AnswerError {
	answer, ok := toolbroker.AsAnswer(err)
	if !ok {
		panic("not an answer")
	}
	return answer
}
