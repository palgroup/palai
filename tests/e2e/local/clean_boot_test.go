//go:build e2e

package local

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestCleanBootUpDoctorDownRetainsData is the lifecycle proof: `palai init` + `local up`
// brings the four services healthy, doctor reports every check green, a created response
// survives a `local down` (volumes retained), and the second `up` serves it back. The
// body is idempotent under -count=3: each `up` mints a fresh one-use enrollment token, so
// repeated boots never reuse a spent identity (LP-012).
func TestCleanBootUpDoctorDownRetainsData(t *testing.T) {
	s := newStack(t)
	s.run("init")
	s.run("local", "up")

	report := s.doctor()
	if !report.OK {
		t.Fatalf("doctor not green after clean boot: %+v", report.Checks)
	}

	// A durable response created before the restart must survive it.
	id := createResponse(t, s, "persist me")

	s.run("local", "down")
	s.run("local", "up")

	resp := s.getResponse(id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET response after restart = %d, want 200 (data not retained)", resp.StatusCode)
	}
}

// TestResetRequiresConfirm proves the destructive reset is guarded: without --confirm it
// exits non-zero and deletes nothing; with --confirm the stack's volumes are actually
// removed (spec §44.4).
func TestResetRequiresConfirm(t *testing.T) {
	s := newStack(t)
	s.run("init")
	s.run("local", "up")

	if err := s.try("local", "reset"); err == nil {
		t.Fatal("local reset without --confirm exited zero; must refuse")
	}
	if vols := s.projectVolumes(); len(vols) == 0 {
		t.Fatal("local reset without --confirm removed volumes; must retain")
	}

	s.run("local", "reset", "--confirm")
	if vols := s.projectVolumes(); len(vols) != 0 {
		t.Fatalf("local reset --confirm left volumes behind: %v", vols)
	}
}

// TestProviderSecretIsAbsentFromComposeSurfaces proves the Option-B secret path keeps the
// raw credential out of every compose surface: after `provider add` writes a sentinel to
// the file secret, the sentinel appears in none of compose.yaml, `docker compose config`,
// or the control-plane container's inspected environment — only in the 0600 file secret.
func TestProviderSecretIsAbsentFromComposeSurfaces(t *testing.T) {
	s := newStack(t)
	s.run("init")

	const sentinel = "sk-SENTINEL-do-not-leak-9f3a1c7e"
	s.runInput(sentinel+"\n", "provider", "add", "provider-one")
	s.run("local", "up")

	// The committed compose file must never carry a secret literal.
	if strings.Contains(readCompose(t, s), sentinel) {
		t.Fatal("sentinel leaked into committed compose.yaml")
	}
	// The rendered, fully-interpolated config must not carry it either.
	if strings.Contains(s.composeConfigOutput(), sentinel) {
		t.Fatal("sentinel leaked into docker compose config output")
	}
	// It must not ride in the control-plane container's environment block.
	if strings.Contains(s.controlPlaneEnv(), sentinel) {
		t.Fatal("sentinel leaked into control-plane .Config.Env")
	}
	// It must live, and only live, in the 0600 file secret.
	secret := readSecret(t, s, "provider-one")
	if strings.TrimSpace(secret) != sentinel {
		t.Fatalf("file secret = %q, want the sentinel", secret)
	}
	assertMode0600(t, s, "secrets/provider-one")
}

// TestResponseCreateAdmitsOverBootstrapKey proves the documented command path: `response
// create` admits over the bootstrap key with no manual SQL (202 + a response id), and the
// resource is retrievable with the same key. The terminal outcome is out of scope here —
// the execution path is wired in Task 15.
func TestResponseCreateAdmitsOverBootstrapKey(t *testing.T) {
	s := newStack(t)
	s.run("init")
	s.run("local", "up")

	id := createResponse(t, s, "hello")
	if id == "" {
		t.Fatal("response create returned no id")
	}

	resp := s.getResponse(id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/responses/%s = %d, want 200", id, resp.StatusCode)
	}
}

// createResponse runs `response create` and returns the minted response id. The CLI
// prints a small JSON line with the id and status.
func createResponse(t *testing.T, s *stack, input string) string {
	t.Helper()
	out := s.run("response", "create", "--input", input)
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	// The command prints the response envelope on its last non-empty line.
	line := lastJSONLine(out)
	if err := json.Unmarshal([]byte(line), &created); err != nil {
		t.Fatalf("decode response create output %q: %v", out, err)
	}
	return created.ID
}
