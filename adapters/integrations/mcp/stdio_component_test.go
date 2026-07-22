//go:build component

package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
)

// fixtureImage returns the digest-pinned MCP fixture image id the harness cross-built and exported, or skips.
func fixtureImage(t *testing.T) string {
	t.Helper()
	id := os.Getenv("PALAI_MCP_FIXTURE_IMAGE_ID")
	if id == "" {
		t.Skip("PALAI_MCP_FIXTURE_IMAGE_ID is required; run make test-component TEST=mcp")
	}
	return id
}

// TestMCPStdioDiscoveryCallProgressCancel proves TOL-008's stdio isolation face against the REAL fixture
// server running inside a hardened, network-less OCI container (one container per call): discovery lists the
// fixture tools; a tools/call echo round-trips; a long-running `slow` call emits progress; a cancelled ctx
// drives notifications/cancelled and the container dies. Teardown leaves no mcp-labelled container behind.
func TestMCPStdioDiscoveryCallProgressCancel(t *testing.T) {
	image := fixtureImage(t)
	driver, err := oci.NewDockerInteractiveDriver()
	if err != nil {
		t.Fatalf("docker interactive driver: %v", err)
	}
	m := NewManager(Config{
		Driver:         driver,
		DefaultTimeout: 15 * time.Second,
		Limits:         oci.Limits{WallTime: 15 * time.Second, MaxMemoryBytes: 256 << 20, MaxProcessCount: 32, NanoCPUs: 1_000_000_000},
	})
	conn := ConnConfig{ID: "mcpc_fixture", Name: "fixture", Transport: "stdio", ImageDigest: image, Cmd: []string{"/mcp"}}
	ctx := context.Background()

	// Discovery: the fixture advertises echo + slow.
	tools, err := m.Discover(ctx, conn)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
	}
	if !names["echo"] || !names["slow"] {
		t.Fatalf("discovered tools = %v, want echo + slow", names)
	}

	// tools/call echo round-trips through the sandboxed server.
	out, err := m.Call(ctx, CallScope{CallID: "tc1"}, conn, "echo", map[string]any{"message": "hi"})
	if err != nil {
		t.Fatalf("call echo: %v", err)
	}
	if out["echo"] != "hi" {
		t.Fatalf("echo result = %v, want echo:hi", out)
	}

	// A long-running call emits progress to the sink.
	var mu sync.Mutex
	var progress int
	m.cfg.Sink = progressCounter{fn: func() { mu.Lock(); progress++; mu.Unlock() }}
	if _, err := m.Call(ctx, CallScope{CallID: "tc2"}, conn, "slow", map[string]any{"steps": 3.0}); err != nil {
		t.Fatalf("call slow: %v", err)
	}
	mu.Lock()
	gotProgress := progress
	mu.Unlock()
	if gotProgress < 1 {
		t.Fatalf("progress notifications = %d, want >= 1", gotProgress)
	}
	m.cfg.Sink = nil

	// A cancelled ctx aborts an in-flight slow call (the manager tears the container down).
	cctx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if _, err := m.Call(cctx, CallScope{CallID: "tc3"}, conn, "slow", map[string]any{"steps": 50.0}); err == nil {
		t.Fatal("cancelled slow call returned nil error, want a cancellation")
	}

	assertNoMCPContainers(t)
}

// progressCounter is a ProgressSink that runs fn per progress notification.
type progressCounter struct{ fn func() }

func (p progressCounter) ToolProgress(context.Context, CallScope, Progress) { p.fn() }

// assertNoMCPContainers fails if any io.palai.sandbox=mcp container survives (a per-call container leak).
func assertNoMCPContainers(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "label="+sandboxLabel+"="+sandboxLabelMCP).Output()
	if err != nil {
		t.Fatalf("list mcp containers: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("mcp-labelled containers survived teardown: %q", strings.TrimSpace(string(out)))
	}
}

// TestOrphanMCPContainerSweptByLabelEngineContainersUntouched proves the §28.13 named gap closure against
// REAL containers: a manually-created mcp-labelled container AND an engine-labelled one are both aged past
// grace; the sweep force-removes the mcp orphan and NEVER touches the engine container.
func TestOrphanMCPContainerSweptByLabelEngineContainersUntouched(t *testing.T) {
	// A locally-present pinned image gives the decoy containers a valid reference; they are never started.
	const decoyImage = "postgres@sha256:17e67d7b9890c99b055ba1e0d5c5be4ec27c9d3a72bda32db24a5e5d8a85af0c"
	mcpName := "palai-mcp-orphan-" + randHex(t)
	engineName := "palai-mcp-engine-" + randHex(t)
	createDecoy(t, mcpName, sandboxLabel+"="+sandboxLabelMCP, decoyImage)
	createDecoy(t, engineName, sandboxLabel+"=engine", decoyImage)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", mcpName, engineName).Run() })

	apiClient, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer apiClient.Close()
	sweeper := NewSweeperWithClient(apiClient, time.Minute)
	// Treat "now" as an hour ahead so both decoys are aged past the grace window.
	sweeper.now = func() time.Time { return time.Now().Add(time.Hour) }

	reclaimed, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if reclaimed < 1 {
		t.Fatalf("reclaimed = %d, want >= 1 (the aged mcp orphan)", reclaimed)
	}
	if decoyExists(t, mcpName) {
		t.Fatalf("mcp orphan %s survived the sweep", mcpName)
	}
	if !decoyExists(t, engineName) {
		t.Fatalf("engine container %s was reaped by the mcp sweep — label scoping failed", engineName)
	}
}

func createDecoy(t *testing.T, name, label, image string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "create", "--name", name, "--label", label, image).CombinedOutput()
	if err != nil {
		t.Fatalf("create decoy %s: %v\n%s", name, err, out)
	}
}

func decoyExists(t *testing.T, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "name=^/"+name+"$").Output()
	if err != nil {
		t.Fatalf("list decoy %s: %v", name, err)
	}
	return strings.TrimSpace(string(out)) != ""
}

func randHex(t *testing.T) string {
	t.Helper()
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(raw[:])
}
