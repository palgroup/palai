// Package compose holds the production-overlay posture guard and its test. The guard
// (production-entrypoint.sh) is the fail-closed check that the production stack cannot boot on
// a dev-default secret master key; this test exercises it directly (no Docker, no binary) by
// running it with a harmless final command, so the security invariant is proven, not asserted.
package compose

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The dev-default placeholders the guard rejects — the SAME literals production-entrypoint.sh
// hardcodes and docs/operations/install.md ships. If these drift out of lock-step the "refuse
// the dev default" contract silently breaks, so they are pinned here too.
const (
	devMasterPlaceholder    = "REPLACE_WITH_OPENSSL_RAND_HEX_32"
	devMasterZero           = "0000000000000000000000000000000000000000000000000000000000000000"
	devBootstrapPlaceholder = "REPLACE_WITH_A_REAL_BOOTSTRAP_KEY"
	realKey                 = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f900"
	realBootstrap           = "palai-deadbeefcafef00d"
)

// writeKey writes v to a temp file and returns its path (empty v => an empty file, the
// "missing/empty key" case).
func writeKey(t *testing.T, name, v string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// runGuard runs production-entrypoint.sh with `true` as its final command, so a boot that
// passes the guard exits 0 (guard reached its `exec "$@"`) and a refused boot exits non-zero
// with a message on stderr — never reaching the real control-plane bridge. Args are fixed
// literals (no user input), so the shell interpreter carries no injection surface. `true` is
// passed unqualified so exec resolves it on PATH (portable: /usr/bin/true on macOS, /bin/true
// in the alpine image).
func runGuard(t *testing.T, env map[string]string) (int, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "./production-entrypoint.sh", "true")
	// A minimal environment: only what each case sets. `set -u` in the script makes an
	// unset PALAI_SECRET_MASTER_KEY_FILE a distinct, tested failure.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stderr.String()
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("run guard: %v", err)
	}
	return exit.ExitCode(), stderr.String()
}

func TestProductionGuardRefusesDevMasterKey(t *testing.T) {
	cases := []struct {
		name    string
		master  string // "" => the var is unset entirely (not an empty file)
		wantMsg string
	}{
		{"unset", "", "PALAI_SECRET_MASTER_KEY_FILE is required"},
		{"empty-file", "\x00file", "missing or empty"},
		{"placeholder", devMasterPlaceholder, "refusing to boot on the dev-default master key"},
		{"all-zeros", devMasterZero, "refusing to boot on the dev-default master key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			switch {
			case tc.master == "":
				// leave PALAI_SECRET_MASTER_KEY_FILE unset
			case strings.HasPrefix(tc.master, "\x00"):
				env["PALAI_SECRET_MASTER_KEY_FILE"] = writeKey(t, "master", "") // empty file
			default:
				env["PALAI_SECRET_MASTER_KEY_FILE"] = writeKey(t, "master", tc.master)
			}
			code, stderr := runGuard(t, env)
			if code == 0 {
				t.Fatalf("guard passed a dev master key (%s); it must fail closed", tc.name)
			}
			if !strings.Contains(stderr, tc.wantMsg) {
				t.Fatalf("stderr %q does not mention %q", stderr, tc.wantMsg)
			}
		})
	}
}

func TestProductionGuardRefusesDevBootstrapKey(t *testing.T) {
	env := map[string]string{
		"PALAI_SECRET_MASTER_KEY_FILE": writeKey(t, "master", realKey),
		"PALAI_BOOTSTRAP_API_KEY_FILE": writeKey(t, "bootstrap", devBootstrapPlaceholder),
	}
	code, stderr := runGuard(t, env)
	if code == 0 {
		t.Fatal("guard passed the dev-default bootstrap key; it must fail closed")
	}
	if !strings.Contains(stderr, "dev-default bootstrap API key") {
		t.Fatalf("stderr %q does not mention the bootstrap refusal", stderr)
	}
}

func TestProductionGuardAdmitsRealKeys(t *testing.T) {
	// A real master key alone (no bootstrap file) passes.
	env := map[string]string{"PALAI_SECRET_MASTER_KEY_FILE": writeKey(t, "master", realKey)}
	if code, stderr := runGuard(t, env); code != 0 {
		t.Fatalf("guard rejected a real master key: exit %d, stderr %q", code, stderr)
	}
	// A real master key AND a real bootstrap key passes.
	env["PALAI_BOOTSTRAP_API_KEY_FILE"] = writeKey(t, "bootstrap", realBootstrap)
	if code, stderr := runGuard(t, env); code != 0 {
		t.Fatalf("guard rejected real master+bootstrap keys: exit %d, stderr %q", code, stderr)
	}
}
