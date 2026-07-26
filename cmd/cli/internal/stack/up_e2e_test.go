//go:build e2e

package stack

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveProofFailsAgainstARealFakeStack is the crown RED, and it is docker-bound on purpose: the
// unit test proves proveLive refuses a fake ROUNDTRIP STRUCT, which is only worth something if the
// struct a real fake stack produces is the one it refuses. This brings up the actual four-service
// compose distribution on the deterministic adapter — the exact stack an operator gets when the
// selector is mis-spelled — drives one real run through it, and asserts the proof REFUSES it.
//
// If this test ever passes its assertion by the run failing for some other reason, it fails loudly:
// the refusal must name the fake adapter specifically, not merely be non-nil.
//
// Tagged e2e (the tag the docker-bound local-stack proofs already use), so a plain `go test` does
// not pretend to have run it. It brings up and tears down its own isolated stack.
func TestLiveProofFailsAgainstARealFakeStack(t *testing.T) {
	root := repoRoot(t)
	t.Chdir(root)
	t.Setenv("PALAI_HOME", t.TempDir())
	t.Setenv("PALAI_COMPOSE_FILE", filepath.Join(root, "deploy", "compose", "compose.yaml"))
	// The two knobs that make this a REAL fake stack rather than a broken one: the exec-path is on,
	// so runs actually execute and complete — they just complete on the in-process adapter.
	t.Setenv("PALAI_DISPATCH_WORKERS", "1")
	t.Setenv("PALAI_MODEL_PROVIDER", "fake")

	if err := Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := Up(); err != nil {
		t.Fatalf("local up: %v", err)
	}
	t.Cleanup(func() {
		if err := Reset(true); err != nil {
			t.Errorf("teardown: %v", err)
		}
	})

	cfg, p, err := loadConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	key, err := readTrimmed(p.apiKey)
	if err != nil {
		t.Fatalf("read api key: %v", err)
	}
	api := &apiClient{baseURL: cfg.BaseURL, key: key, http: &http.Client{Timeout: 30 * time.Second}}

	rt, err := roundTripProof(api)
	if err == nil {
		t.Fatalf("THE CROWN GUARANTEE IS BROKEN: the live proof accepted a stack running the fake adapter (%+v)", rt)
	}
	if !strings.Contains(err.Error(), "THE STACK IS FAKE") {
		t.Fatalf("the proof failed, but not for the fake adapter — this test would pass on any broken stack: %v", err)
	}
	// The run must genuinely have COMPLETED: a refusal earned by a failed run would make the
	// assertion above vacuous, since a failed run is refused on this stack and on a live one alike.
	if !strings.Contains(err.Error(), "completed on model \"fake\"") {
		t.Fatalf("the fake stack did not complete a run, so the refusal proves nothing about fake-detection: %v", err)
	}
	t.Logf("the live proof refused the fake stack: %v", err)
}

// repoRoot walks up from the test's directory to the module root — Up() shells `docker build
// engines/reference` and compose's build context is `../..`, both relative to cwd.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
