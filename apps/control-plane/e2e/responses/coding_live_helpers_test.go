//go:build e2e

package responses

// Helpers for TestCodingLiveJourney: the real-provider seam turn (T4 discipline — separate FORCED
// broker.Route calls, not a live orchestrator loop), the live Git broker (a real GitHub App installation
// token when the App env is present, else the local broker over PALAI_GIT_REPO), and the tool schemas the
// provider sees. Kept in a sibling file so the journey reads top-to-bottom.

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

func envOrLive(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func liveCodingModel() string { return envOrLive("PALAI_LIVE_MODEL", "gpt-4o-mini") }

func fileToolBuiltin() toolbroker.Tool  { return tools.FileTool() }
func shellToolBuiltin() toolbroker.Tool { return tools.ShellTool() }

func liveDecode(t *testing.T, arguments string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(arguments), &m); err != nil {
		t.Fatalf("tool arguments are not valid JSON: %v (%q)", err, arguments)
	}
	return m
}

func liveJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func liveGitSHA(t *testing.T) string {
	return strings.TrimSpace(mustGit(t, "rev-parse", "--short", "HEAD"))
}

func liveRepoRoot(t *testing.T) string {
	return strings.TrimSpace(mustGit(t, "rev-parse", "--show-toplevel"))
}

func liveHash(parts ...string) string { return hashCoding(parts...) }

func safePrefixLive(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// liveForceTool issues ONE forced tool turn against the real provider and returns the tool call + its
// genuine chatcmpl id. Self-contained per turn (no cross-turn tool-call history threading, which the chat
// API rejects) — the multi-step proof is that each real tool acts on the SAME real workspace.
func liveForceTool(t *testing.T, ctx context.Context, models *modelbroker.Broker, stream func(modelbroker.Delta), mreqID, prompt string, schema modelbroker.ToolSchema) (modelbroker.ToolCall, string) {
	t.Helper()
	res, err := models.Route(ctx, "provider-one", modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID(mreqID), RouteRevision: 1, ModelStepID: mreqID, Model: liveCodingModel(),
		Messages: []modelbroker.Message{{Role: "user", Content: prompt}},
		Tools:    []modelbroker.ToolSchema{schema}, ForceToolCall: true,
		Deadline: time.Now().Add(60 * time.Second), Reservation: modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret: modelbroker.SecretRef("provider-one"),
	}, stream)
	if err != nil {
		t.Fatalf("route %s: %v", mreqID, err)
	}
	if res.Error != nil {
		t.Fatalf("%s: provider returned a sanitized error: %s", mreqID, res.Error.Code)
	}
	if !strings.HasPrefix(res.ProviderRequestID, "chatcmpl") {
		t.Fatalf("%s: provider request id %q is not a real chat completion id", mreqID, res.ProviderRequestID)
	}
	if len(res.ToolCalls) == 0 {
		t.Fatalf("%s: the forced tool call produced no tool request", mreqID)
	}
	return res.ToolCalls[0], res.ProviderRequestID
}

func liveFileSchema() modelbroker.ToolSchema {
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
			"required": []any{"op", "path"}, "additionalProperties": false,
		},
	}
}

func liveShellSchema() modelbroker.ToolSchema {
	return modelbroker.ToolSchema{
		Name:        "palai.workspace.shell",
		Description: "Run a command in the workspace sandbox. argv is the command and its arguments.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"argv": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []any{"argv"}, "additionalProperties": false,
		},
	}
}

// liveCodingBroker returns the Git broker: a REAL GitHub App installation-token broker when the App env is
// present (installation token > user PAT, §30.2), else the local broker over the clean PALAI_GIT_REPO URL.
func liveCodingBroker(t *testing.T) (repositories.Broker, string) {
	t.Helper()
	appID, installID, keyFile := os.Getenv("PALAI_GITHUB_APP_ID"), os.Getenv("PALAI_GITHUB_APP_INSTALLATION_ID"), os.Getenv("PALAI_GITHUB_APP_PRIVATE_KEY_FILE")
	if appID != "" && installID != "" && keyFile != "" {
		pem, err := os.ReadFile(keyFile)
		if err != nil {
			t.Fatalf("read GitHub App private key: %v", err)
		}
		var repos []string
		if r := os.Getenv("PALAI_GITHUB_APP_REPO"); r != "" {
			repos = []string{r}
		}
		broker, err := repositories.NewGitHubAppBroker(repositories.GitHubAppConfig{AppID: appID, InstallationID: installID, PrivateKeyPEM: pem, Repositories: repos})
		if err != nil {
			t.Fatalf("NewGitHubAppBroker: %v", err)
		}
		return broker, "github-app-installation-token (real)"
	}
	return repositories.NewLocalBroker(), "local-broker (PALAI_GIT_REPO, no App env)"
}

// liveCodingPRClient returns a real GitHub pull-request client when the App env + PALAI_GITHUB_REPO
// (owner/repo) are set, else nil — a push-only live run opens no PR.
func liveCodingPRClient(t *testing.T) repositories.PullRequestClient {
	t.Helper()
	appID, installID, keyFile := os.Getenv("PALAI_GITHUB_APP_ID"), os.Getenv("PALAI_GITHUB_APP_INSTALLATION_ID"), os.Getenv("PALAI_GITHUB_APP_PRIVATE_KEY_FILE")
	slug := os.Getenv("PALAI_GITHUB_REPO")
	i := strings.IndexByte(slug, '/')
	if appID == "" || installID == "" || keyFile == "" || i <= 0 {
		return nil
	}
	pem, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("read GitHub App private key: %v", err)
	}
	client, err := repositories.NewGitHubPullRequestClient(repositories.GitHubAppConfig{AppID: appID, InstallationID: installID, PrivateKeyPEM: pem}, slug[:i], slug[i+1:])
	if err != nil {
		t.Fatalf("NewGitHubPullRequestClient: %v", err)
	}
	return client
}

// liveAllocation creates a Docker-shareable, sandbox-writable workspace allocation the real clone lands in
// (at <root>/repo) and the OCI shell tool bind-mounts.
func liveAllocation(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "palai-live-journey-")
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
	_ = filepath.WalkDir(resolved, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o777)
		}
		return nil
	})
	return resolved
}
