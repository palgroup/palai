//go:build live

// Package workspace_test is the E09 Task 1 live workspace-mount smoke. It runs only under the
// `live` build tag, in `make test-live-provider PROVIDER=provider-one CASE=workspace-mount`, which
// cross-builds the fixture engine into a digest-pinned image (PALAI_RUNNER_ENGINE_IMAGE_ID) and
// loads the real provider credential from .env.local. In ONE real run it proves: a real workspace
// allocation is bind-mounted into a real, hardened OCI engine container; the engine reads a seed
// staged there; a real provider-one chat completion happens mid-run (the runner bridges the
// engine's model.request to the real provider); and the file the engine writes persists in the
// host allocation after the container is destroyed.
//
// HONEST CEILING: this proves the REAL mount and a REAL provider round-trip, NOT model behaviour.
// The engine is a deterministic fixture and the model does not itself drive a file tool — that is
// E09 Task 4. The credential is used only as an opaque needle for the leak scan and never printed.
package workspace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	"github.com/palgroup/palai/packages/runner"
)

const credentialEnv = "OPENAI_API_KEY"

func liveModel() string {
	if m := os.Getenv("PALAI_LIVE_MODEL"); m != "" {
		return m
	}
	return "gpt-4o-mini"
}

// TestLiveWorkspaceMountRealProviderRun is CASE=workspace-mount: a real provider-one run bound to a
// real workspace allocation, proving the engine container sees the real /workspace mount and the
// seeded file persists — the mount, not model behaviour (E09 Task 1 honest ceiling).
func TestLiveWorkspaceMountRealProviderRun(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}
	engineImage := os.Getenv("PALAI_RUNNER_ENGINE_IMAGE_ID")
	if engineImage == "" {
		t.Skip("PALAI_RUNNER_ENGINE_IMAGE_ID is required; run make test-live-provider PROVIDER=provider-one CASE=workspace-mount")
	}

	// Stage a real workspace allocation and seed a fixed file the engine will read back.
	allocDir := newAllocation(t)
	if err := workspace.Prepare(allocDir); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	const seed = "e09-t1-live-mount-seed"
	if err := os.WriteFile(filepath.Join(allocDir, workspace.RepoDir, "seed"), []byte(seed), 0o644); err != nil {
		t.Fatalf("stage seed: %v", err)
	}
	makeSandboxWritable(t, allocDir)

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})

	sup := newStreamSupervisor(t)
	request := runner.EngineRequest{
		ImageDigest:       engineImage,
		RunID:             "run_liveworkspace",
		AttemptID:         "att_liveworkspace",
		Env:               map[string]string{"PALAI_ENGINE_MODE": "workspace_stream"},
		WorkspaceHostPath: allocDir,
		Limits: runner.Limits{
			WallTimeMS:      90000, // > the provider deadline, so a slow provider still terminates cleanly
			MaxStdoutBytes:  32 * 1024,
			MaxStderrBytes:  4 * 1024,
			MaxFrameBytes:   8 * 1024,
			MaxMemoryBytes:  64 * 1024 * 1024,
			MaxProcessCount: 16,
		},
	}

	var forwarded []contracts.EngineFrame
	sawModelRequest := make(chan struct{}, 1)
	sink := func(_ context.Context, frame contracts.EngineFrame) error {
		forwarded = append(forwarded, frame)
		if frame.Type == "model.request" {
			select {
			case sawModelRequest <- struct{}{}:
			default:
			}
		}
		return nil
	}

	// The bridge: when the engine requests a model step, call the REAL provider, then feed a
	// model.result so the engine finishes. The real chat completion happens mid-run, with the
	// workspace mounted into the live container.
	var streamed bytes.Buffer
	type routed struct {
		res modelbroker.Result
		err error
	}
	resultCh := make(chan routed, 1)
	inbound := make(chan contracts.EngineFrame, 1)
	go func() {
		<-sawModelRequest
		res, err := broker.Route(context.Background(), "provider-one", liveRequest(), func(d modelbroker.Delta) {
			streamed.WriteString(d.Text)
		})
		resultCh <- routed{res: res, err: err}
		inbound <- contracts.EngineFrame{
			Protocol:  "engine.v1",
			ID:        "frm_livectl1",
			Type:      "model.result",
			Sequence:  1,
			Time:      time.Now().UTC().Format(time.RFC3339),
			RunID:     request.RunID,
			AttemptID: request.AttemptID,
			Data:      map[string]any{"model_request_id": "mreq_workspace1"},
		}
		close(inbound)
	}()

	result, err := sup.Stream(context.Background(), request, inbound, sink)
	if err != nil {
		t.Fatalf("stream run error = %v, want a clean completion", err)
	}

	// The real provider round-trip: a genuine chat completion id, populated usage, no hidden retry.
	routedResult := <-resultCh
	if routedResult.err != nil {
		t.Fatalf("real provider route error = %v", routedResult.err)
	}
	res := routedResult.res
	if res.Error != nil {
		t.Fatalf("provider returned a sanitized error: code=%s status=%d", res.Error.Code, res.Error.Status)
	}
	if !strings.HasPrefix(res.ProviderRequestID, "chatcmpl") {
		t.Errorf("provider request id %q is not a real chat completion id", res.ProviderRequestID)
	}
	if res.Usage.TotalTokens <= 0 {
		t.Errorf("usage is not populated: %+v", res.Usage)
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1 (no hidden provider retry)", res.Attempts)
	}

	// The run reached its model step inside the real container (the mount is live around it).
	if !containsType(forwarded, "model.request") {
		t.Fatalf("the run never reached a model step in the container: %d frames forwarded", len(forwarded))
	}

	// The load-bearing mount proof is the persisted file: the engine echoed the seed it READ from
	// the bind-mounted /workspace back into it (proving the mount was readable), the runner reads it
	// back on the host (proving it was writable and persisted), and the container is destroyed. This
	// does not depend on the final run.terminal frame, which a fast clean exit can race out of the
	// streaming sink.
	assertContainerGone(t, result.ContainerID)
	persisted, err := os.ReadFile(filepath.Join(allocDir, workspace.ScratchDir, "out"))
	if err != nil {
		t.Fatalf("read persisted workspace write: %v", err)
	}
	if string(persisted) != "seed:"+seed {
		t.Fatalf("persisted workspace file = %q, want the engine's echo of the seed it read (\"seed:%s\")", persisted, seed)
	}

	// Leak scan by construction: the credential must not appear in any captured surface.
	resultJSON, _ := json.Marshal(res)
	for name, captured := range map[string][]byte{"streamed deltas": streamed.Bytes(), "provider result": resultJSON} {
		if bytes.Contains(captured, []byte(secret)) {
			t.Fatalf("%s contains the credential value", name)
		}
	}

	t.Logf("live workspace-mount PASS (real mount, real provider; NOT model behaviour): provider_request_id=%s… seed_persisted model=%s",
		safePrefix(res.ProviderRequestID), res.Model)
}

// liveRequest is a minimal real chat completion: the point is a genuine provider round-trip during
// the run, not a tool call (the model does not drive tools in Task 1).
func liveRequest() modelbroker.Request {
	return modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_workspace1"),
		RouteRevision:  1,
		ModelStepID:    "step-1",
		Model:          liveModel(),
		Messages:       []modelbroker.Message{{Role: "user", Content: "Reply with the single word: ready."}},
		Deadline:       time.Now().Add(60 * time.Second),
		Reservation:    modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:         modelbroker.SecretRef("provider-one"),
	}
}

func containsType(frames []contracts.EngineFrame, typ string) bool {
	for _, f := range frames {
		if f.Type == typ {
			return true
		}
	}
	return false
}

func newStreamSupervisor(t *testing.T) *runner.StreamSupervisor {
	t.Helper()
	driver, err := oci.NewDockerInteractiveDriver()
	if err != nil {
		t.Fatalf("create Docker interactive driver: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := driver.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				t.Errorf("close interactive driver: %v", err)
			}
		}
	})
	return runner.NewStreamSupervisor(driver)
}

// newAllocation creates a Docker-shareable, symlink-resolved allocation directory under /tmp.
func newAllocation(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "palai-live-ws-")
	if err != nil {
		t.Fatalf("create allocation dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve allocation dir: %v", err)
	}
	return resolved
}

// makeSandboxWritable opens every allocation directory to the unprivileged sandbox uid.
func makeSandboxWritable(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
}

func assertContainerGone(t *testing.T, containerID string) {
	t.Helper()
	if containerID == "" {
		t.Fatal("supervisor did not return a created container ID")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := exec.Command("docker", "container", "inspect", containerID).Run(); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s still exists after supervisor returned", containerID[:12])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func safePrefix(id string) string {
	if len(id) > 16 {
		return id[:16]
	}
	return id
}
