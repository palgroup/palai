//go:build live

// Package live is the E09 Task 4 live coding-tool smoke: the FIRST real multi-step tool loop against
// a real provider. It runs only under the `live` build tag in `make test-live-provider
// PROVIDER=provider-one CASE=coding-tools`, which loads the real credential from .env.local and pins
// a busybox shell image (PALAI_SHELL_IMAGE_ID). In ONE run it drives the REAL provider through a
// forced (tool_choice:required) tool loop and executes the REAL coding tools through the production
// broker seam: the file tool writes into a real workspace, then the shell tool reads it back inside a
// real hardened OCI sandbox (uid 65532, no network, read-only rootfs, dropped caps, cgroup bounds).
//
// HONEST CEILING (mandatory, spec §10.2 discipline): this smoke wires a MINIMAL, smoke-scoped
// topology — the test process is the trusted control plane launching the hardened tool sandbox. It
// proves: real provider → FORCED tool_call → real hardened-sandbox file+shell exec → tool result →
// model, the first real multi-step tool loop live, plus real SAN enforcement (the busybox command
// runs unprivileged with no network). It does NOT prove spontaneous model tool-choice (the call is
// forced), and it does NOT prove the FINAL production tool-exec topology — control-plane-launch vs
// runner-relay is an explicit T7/T9 architecture decision, NOT silently Option A. The credential is
// used only as an opaque needle for the leak scan and is never printed.
package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	tools "github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

const credentialEnv = "OPENAI_API_KEY"

// marker is the distinctive content the model writes with the file tool and the shell tool reads
// back, so a match in shell stdout proves the two tools mutated and observed the SAME workspace.
const marker = "PALAI-E09-T4-LIVE-8f3a2c"

func liveModel() string {
	if m := os.Getenv("PALAI_LIVE_MODEL"); m != "" {
		return m
	}
	return "gpt-4o-mini"
}

// TestLiveCodingToolRoundTrip is CASE=coding-tools: a real provider drives a forced file-then-shell
// tool loop, executed through the production broker seam against a real workspace and a real hardened
// sandbox. It is the first real multi-step coding tool loop against a live provider.
func TestLiveCodingToolRoundTrip(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}
	shellImage := os.Getenv("PALAI_SHELL_IMAGE_ID")
	if shellImage == "" {
		t.Skip("PALAI_SHELL_IMAGE_ID is required; run make test-live-provider PROVIDER=provider-one CASE=coding-tools")
	}

	allocDir := newAllocation(t)
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})

	driver, err := oci.NewDockerDriver()
	if err != nil {
		t.Fatalf("create Docker driver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	shell := workspace.NewShellExecutor(driver, shellImage, oci.Limits{
		WallTime: 30 * time.Second, MaxMemoryBytes: 256 << 20, MaxProcessCount: 128, NanoCPUs: 1_000_000_000,
	})

	// The production broker seam: the SAME broker.Execute path dispatchTool drives, with the real
	// coding tools registered. The file tool confines to allocDir; the shell tool runs behind the
	// injected sandbox runner.
	tb := toolbroker.New(tools.FileTool(), tools.ShellTool())
	env := toolbroker.ExecEnv{WorkspaceRoot: allocDir, Shell: shell}

	var streamed bytes.Buffer
	stream := func(d modelbroker.Delta) {
		streamed.WriteString(d.Text)
		if d.ToolCall != nil {
			streamed.WriteString(d.ToolCall.Name)
			streamed.WriteString(d.ToolCall.ArgumentsFragment)
		}
	}

	// --- Turn 1: the model is FORCED to call the file tool to write the marker file. ---
	fileReq := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_coding_file"),
		RouteRevision:  1, ModelStepID: "step-file", Model: liveModel(),
		Messages:      []modelbroker.Message{{Role: "user", Content: "Use the file tool to write the file repo/hello.txt with exactly this content: " + marker}},
		Tools:         []modelbroker.ToolSchema{fileToolSchema()},
		ForceToolCall: true,
		Deadline:      time.Now().Add(60 * time.Second),
		Reservation:   modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:        modelbroker.SecretRef("provider-one"),
	}
	res1, err := broker.Route(context.Background(), "provider-one", fileReq, stream)
	if err != nil {
		t.Fatalf("route file turn: %v", err)
	}
	assertRealCompletion(t, res1, "file turn")
	fileCall := requireToolCall(t, res1, "file turn")

	// Execute the file tool through the broker seam (fenced row + usage), exactly as dispatchTool does.
	fileArgs := decodeArgs(t, fileCall.Arguments)
	fileOut, err := tb.Execute(context.Background(), contracts.ToolCallID("tc_file_1"), fileCall.Name, fileArgs, 1, env)
	if err != nil {
		t.Fatalf("execute file tool: %v", err)
	}
	// The workspace really mutated: the file the model wrote is on disk with the marker content.
	written, err := os.ReadFile(filepath.Join(allocDir, "repo", "hello.txt"))
	if err != nil || !strings.Contains(string(written), marker) {
		t.Fatalf("file tool did not persist the marker to the workspace (got %q, err %v); result=%v", written, err, fileOut.Result)
	}

	// --- Turn 2: a fresh forced call — the model must use the shell tool to read the file back.
	// Self-contained (no tool-call history threading, which the chat API rejects across turns); the
	// multi-step proof is that this real tool reads the SAME workspace the previous real tool wrote. ---
	shellReq := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_coding_shell"),
		RouteRevision:  1, ModelStepID: "step-shell", Model: liveModel(),
		Messages: []modelbroker.Message{
			{Role: "user", Content: "The workspace has a file at repo/hello.txt. Use the shell tool to print its contents (for example: cat repo/hello.txt)."},
		},
		Tools:         []modelbroker.ToolSchema{shellToolSchema()},
		ForceToolCall: true,
		Deadline:      time.Now().Add(60 * time.Second),
		Reservation:   modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:        modelbroker.SecretRef("provider-one"),
	}
	res2, err := broker.Route(context.Background(), "provider-one", shellReq, stream)
	if err != nil {
		t.Fatalf("route shell turn: %v", err)
	}
	assertRealCompletion(t, res2, "shell turn")
	shellCall := requireToolCall(t, res2, "shell turn")

	// Execute the shell tool through the broker seam — the argv runs inside the REAL hardened sandbox.
	shellArgs := decodeArgs(t, shellCall.Arguments)
	shellOut, err := tb.Execute(context.Background(), contracts.ToolCallID("tc_shell_1"), shellCall.Name, shellArgs, 2, env)
	if err != nil {
		t.Fatalf("execute shell tool: %v", err)
	}
	if _, ok := shellOut.Result["exit_code"]; !ok {
		t.Fatalf("shell tool recorded no exit code (sandbox did not run): %v", shellOut.Result)
	}
	// The shell read back what the file tool wrote — the two real tools observed the same workspace.
	if stdout, _ := shellOut.Result["stdout"].(string); !strings.Contains(stdout, marker) {
		t.Fatalf("shell tool stdout did not contain the marker the file tool wrote; argv=%v exit=%v stdout=%q",
			shellArgs["argv"], shellOut.Result["exit_code"], shellOut.Result["stdout"])
	}

	// --- Turn 3: a final natural-language step that closes the loop (no forced tool). The shell
	// tool's real output is fed in as context so the model summarizes what the tools produced. ---
	finalReq := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_coding_final"),
		RouteRevision:  1, ModelStepID: "step-final", Model: liveModel(),
		Messages: []modelbroker.Message{
			{Role: "user", Content: "A shell command printed this file content:\n" + fmt.Sprint(shellOut.Result["stdout"]) + "\nIn one short sentence, state what the file contains."},
		},
		Deadline:    time.Now().Add(60 * time.Second),
		Reservation: modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:      modelbroker.SecretRef("provider-one"),
	}
	res3, err := broker.Route(context.Background(), "provider-one", finalReq, stream)
	if err != nil {
		t.Fatalf("route final turn: %v", err)
	}
	assertRealCompletion(t, res3, "final turn")

	// The loop was genuinely multi-step: three distinct real chat completions.
	ids := map[string]bool{res1.ProviderRequestID: true, res2.ProviderRequestID: true, res3.ProviderRequestID: true}
	if len(ids) != 3 {
		t.Fatalf("expected 3 distinct chat completion ids across the tool loop, got %v", ids)
	}

	// Leak scan by construction: the credential must appear in no captured surface.
	for name, captured := range map[string][]byte{
		"streamed deltas": streamed.Bytes(),
		"file result":     mustJSON(fileOut.Result),
		"shell result":    mustJSON(shellOut.Result),
	} {
		if bytes.Contains(captured, []byte(secret)) {
			t.Fatalf("%s contains the credential value", name)
		}
	}

	t.Logf("live coding-tool round-trip PASS (real provider, FORCED tool calls; NOT spontaneous choice): "+
		"file_turn=%s… shell_turn=%s… final_turn=%s… file_tool=%s shell_argv=%v marker_read_back=true model=%s",
		safePrefix(res1.ProviderRequestID), safePrefix(res2.ProviderRequestID), safePrefix(res3.ProviderRequestID),
		fileCall.Name, shellArgs["argv"], res3.Model)
}

// --- schemas the provider sees (richer than the broker's minimal validator: argv is a real array) ---

func fileToolSchema() modelbroker.ToolSchema {
	return modelbroker.ToolSchema{
		Name:        "palai.workspace.file",
		Description: "Read or write a file in the workspace. To create a file, set op to \"write\".",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op":      map[string]any{"type": "string", "enum": []any{"read", "write", "list", "stat", "checksum"}},
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required":             []any{"op", "path"},
			"additionalProperties": false,
		},
	}
}

func shellToolSchema() modelbroker.ToolSchema {
	return modelbroker.ToolSchema{
		Name:        "palai.workspace.shell",
		Description: "Run a command in the workspace sandbox. argv is the command and its arguments.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"argv": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required":             []any{"argv"},
			"additionalProperties": false,
		},
	}
}

// --- helpers ---

func assertRealCompletion(t *testing.T, res modelbroker.Result, turn string) {
	t.Helper()
	if res.Error != nil {
		t.Fatalf("%s: provider returned a sanitized error: code=%s status=%d", turn, res.Error.Code, res.Error.Status)
	}
	if !strings.HasPrefix(res.ProviderRequestID, "chatcmpl") {
		t.Fatalf("%s: provider request id %q is not a real chat completion id", turn, res.ProviderRequestID)
	}
	if res.Usage.TotalTokens <= 0 {
		t.Fatalf("%s: usage is not populated: %+v", turn, res.Usage)
	}
	if res.Attempts != 1 {
		t.Fatalf("%s: attempts = %d, want exactly 1 (no hidden provider retry)", turn, res.Attempts)
	}
}

func requireToolCall(t *testing.T, res modelbroker.Result, turn string) modelbroker.ToolCall {
	t.Helper()
	if len(res.ToolCalls) == 0 {
		t.Fatalf("%s: the forced tool call produced no tool request", turn)
	}
	return res.ToolCalls[0]
}

func decodeArgs(t *testing.T, arguments string) map[string]any {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		t.Fatalf("tool arguments are not valid JSON: %v (%q)", err, arguments)
	}
	return args
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// newAllocation creates a Docker-shareable, sandbox-writable workspace allocation under /tmp.
func newAllocation(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "palai-live-coding-")
	if err != nil {
		t.Fatalf("create allocation dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve allocation dir: %v", err)
	}
	if err := workspace.Prepare(resolved); err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	err = filepath.WalkDir(resolved, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o777)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("open allocation to sandbox uid: %v", err)
	}
	return resolved
}

func safePrefix(id string) string {
	if len(id) > 16 {
		return id[:16]
	}
	return id
}
