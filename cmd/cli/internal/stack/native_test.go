package stack

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The native posture's three refusals (E22 T5). Each one is a fact
// deploy/compose/native-control-plane.yml states in prose and nothing enforced: if the operator got
// it wrong the stack came up, looked healthy, and then failed twenty seconds later as a TLS or DNS
// error inside a container nobody was reading. These tests demand the refusal name the variable.

// getFrom is the .env.local lookup shape resolveProvider and applyGitHubAppEnv already use.
func getFrom(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

func TestNativeRunnerListenerRefusesALoopbackAddress(t *testing.T) {
	_, err := nativeRunnerListen(getFrom(map[string]string{"PALAI_RUNNER_LISTEN_ADDR": "127.0.0.1:8443"}), 8443)
	if err == nil {
		t.Fatal("a loopback runner listener was accepted: the runner runs in a CONTAINER and reaches this process through the host gateway, so 127.0.0.1 on the host is unreachable to it")
	}
	if !strings.Contains(err.Error(), "PALAI_RUNNER_LISTEN_ADDR") {
		t.Fatalf("the refusal does not name the variable that is wrong: %v", err)
	}
}

func TestNativeRunnerListenerRefusesAPortTheRunnerContainerDoesNotDial(t *testing.T) {
	// The overlay dials https://control-plane:${PALAI_RUNNER_PORT}. A listener on any other port is
	// a connection refused a full compose bring-up later.
	_, err := nativeRunnerListen(getFrom(map[string]string{"PALAI_RUNNER_LISTEN_ADDR": ":9999"}), 8443)
	if err == nil {
		t.Fatal("a runner listener on a port the container never dials was accepted")
	}
	if !strings.Contains(err.Error(), "PALAI_RUNNER_PORT") {
		t.Fatalf("the refusal does not name PALAI_RUNNER_PORT, the value the container actually dials: %v", err)
	}
}

func TestNativeRunnerListenerDefaultsToTheWildcardOnTheDialledPort(t *testing.T) {
	addr, err := nativeRunnerListen(getFrom(nil), 8443)
	if err != nil {
		t.Fatalf("unconfigured is the normal case and must not refuse: %v", err)
	}
	if addr != ":8443" {
		t.Fatalf("listener %q: the default must bind every interface on the port the runner dials", addr)
	}
}

func TestNativeWorkspaceRootRefusesUnsetAndRelativePaths(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"unset":    nil,
		"relative": {"PALAI_WORKSPACE_ROOT": "workspaces"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := nativeWorkspaceRoot(getFrom(values))
			if err == nil {
				t.Fatal("accepted: the overlay binds ${PALAI_WORKSPACE_ROOT} as both source and target and the Docker daemon resolves the source on the HOST")
			}
			if !strings.Contains(err.Error(), "PALAI_WORKSPACE_ROOT") {
				t.Fatalf("the refusal does not name the variable: %v", err)
			}
		})
	}
}

// TestNativeWorkspaceRootRefusesAWorldWritableParent is E22 T2's half of fact 3, and it is the one
// place this epic can refuse something rather than merely document it.
//
// In the native posture the boundary is the uid, and every run's workspace, HOME, TMPDIR and
// CoreSimulator device set live under this root. `/private/tmp` and `/Users/Shared` are `drwxrwxrwt`
// — world-writable with the sticky bit — so a root under either puts all of that where ANY local
// account, not merely this uid, can create and replace paths alongside it. That is a rung BELOW the
// only boundary the posture claims, so it is refused at bring-up rather than written down
// (docs/research/macos-isolation-without-accounts.md §6).
//
// The ancestors are checked on the RESOLVED path: /tmp is a symlink to /private/tmp on macOS, and a
// check that reads the name it was given is a check an ordinary alias walks straight through.
func TestNativeWorkspaceRootRefusesAWorldWritableParent(t *testing.T) {
	// A sticky world-writable directory, built rather than borrowed so the case runs anywhere.
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(shared, 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(shared, 0o777|os.ModeSticky); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// Refused when the root IS the sticky directory, and when it is merely UNDER one: an attacker who
	// can create `<shared>/palai` before the operator does owns every allocation minted inside it.
	for name, root := range map[string]string{
		"the root itself":  shared,
		"a child of it":    filepath.Join(shared, "workspaces"),
		"a grandchild":     filepath.Join(shared, "palai", "workspaces"),
		"through /tmp-ish": filepath.Join(shared, ".", "workspaces"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := nativeWorkspaceRoot(getFrom(map[string]string{"PALAI_WORKSPACE_ROOT": root}))
			if err == nil {
				t.Fatal("accepted a workspace root under a world-writable sticky directory — every run's HOME, TMPDIR and device set would sit where any local account can plant a path")
			}
			if !strings.Contains(err.Error(), "PALAI_WORKSPACE_ROOT") {
				t.Fatalf("the refusal does not name the variable: %v", err)
			}
			if !strings.Contains(err.Error(), shared) {
				t.Fatalf("the refusal does not name the offending directory %q, so the operator cannot act on it: %v", shared, err)
			}
		})
	}

	// It must still ACCEPT an ordinary root, or it is a check that refuses everything.
	if _, err := nativeWorkspaceRoot(getFrom(map[string]string{"PALAI_WORKSPACE_ROOT": filepath.Join(t.TempDir(), "workspaces")})); err != nil {
		t.Fatalf("an ordinary private path was refused, so the check discriminates nothing: %v", err)
	}
}

// TestNativeWorkspaceRootResolvesToOneRealPathOnBothSides is the macOS half of fact 3: /tmp is a
// symlink to /private/tmp, so a control plane that names one and a daemon that resolves the other
// bind two different strings for the same directory.
func TestNativeWorkspaceRootResolvesToOneRealPathOnBothSides(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	root, err := nativeWorkspaceRoot(getFrom(map[string]string{"PALAI_WORKSPACE_ROOT": filepath.Join(link, "workspaces")}))
	if err != nil {
		t.Fatalf("an absolute path through a symlink is legitimate: %v", err)
	}
	if strings.HasPrefix(root, link) {
		t.Fatalf("workspace root %q still carries the symlink: the control plane would name a path the daemon resolves differently", root)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("the root was not created: %v", err)
	}
}

// TestNativeEnvironmentReachesTheContainersByTheirPublishedPorts pins what makes the process
// native at all: every backing service it used to reach by a compose service name is now a
// published loopback port, and every file secret the entrypoint bridged is now a host path.
func TestNativeEnvironmentReachesTheContainersByTheirPublishedPorts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PALAI_HOME", home)
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := os.MkdirAll(p.secretsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p.pgPassword, []byte("hunter2"), 0o600); err != nil {
		t.Fatalf("write pg password: %v", err)
	}
	if err := os.WriteFile(p.secretPath("provider-one"), []byte("sk-test-value"), 0o600); err != nil {
		t.Fatalf("write provider secret: %v", err)
	}
	cfg := Config{Project: "palai-test", APIPort: 18080, RunnerPort: 18443, PgPort: 15432, S3Port: 18333,
		BaseURL: "http://127.0.0.1:18080", ControllerDNS: controllerDNS}

	env, err := nativeEnv(cfg, p, getFrom(nil), "sha256:deadbeef", ":18443", "/tmp/palai-workspaces")
	if err != nil {
		t.Fatalf("nativeEnv: %v", err)
	}
	got := envMapOf(env)
	for name, want := range map[string]string{
		"PALAI_DATABASE_URL":           "postgres://palai:hunter2@127.0.0.1:15432/palai?sslmode=disable",
		"PALAI_S3_ENDPOINT":            "http://127.0.0.1:18333",
		"PALAI_LISTEN_ADDR":            "127.0.0.1:18080",
		"PALAI_RUNNER_LISTEN_ADDR":     ":18443",
		"PALAI_WORKSPACE_ROOT":         "/tmp/palai-workspaces",
		"PALAI_ENGINE_IMAGE":           "sha256:deadbeef",
		"PALAI_SECRET_MASTER_KEY_FILE": p.masterKey,
		"PALAI_BOOTSTRAP_API_KEY_FILE": p.apiKey,
		"PALAI_RUNNER_SERVER_CERT":     p.serverCert,
		"PALAI_ENROLLMENT_TOKEN_FILE":  p.runnerToken,
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
	// NO PROVIDER CREDENTIAL REACHES THIS ENVIRONMENT, and the assertion is inverted from what it was:
	// this map used to require PALAI_SECRET_PROVIDER_ONE="sk-test-value". The bridge was removed
	// 2026-08-04 because an environment value is readable by anything running as the same uid — which on
	// this posture includes the agent's own shell — and cannot be unset afterwards, since macOS answers
	// `ps` from the kernel's copy of the process's initial environment. The credential is sealed through
	// POST /v1/secret-refs and named by a model connection instead.
	//
	// Asserted by ABSENCE over the whole map rather than by deleting the old line, because deleting it
	// would have left the tree with no statement either way, and the next person restoring the bridge
	// would have found every test green.
	for name := range got {
		if strings.HasPrefix(name, "PALAI_SECRET_PROVIDER") || name == "PALAI_SECRET_OPENAI_COMPATIBLE" {
			t.Errorf("%s reached the native environment: a provider credential must never enter it", name)
		}
	}
	// A container path in a native process names a file that is not there. The entrypoint's own
	// bridge is what this environment replaces, so none of its destinations may survive into it.
	for name, v := range got {
		if strings.HasPrefix(name, "PALAI_") && strings.HasPrefix(v, "/run/secrets/") {
			t.Errorf("%s=%s is the CONTAINER's path: this process has no /run/secrets", name, v)
		}
	}
}

// TestNativeEnvironmentHasNoDuplicateKeys is not a style check. os.Getenv in the child returns the
// FIRST occurrence of a key (os.copyenv keeps the first index), so appending an override to
// os.Environ() produces an environment where the override is silently ignored.
func TestNativeEnvironmentHasNoDuplicateKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PALAI_HOME", home)
	t.Setenv("PALAI_LISTEN_ADDR", ":8080") // the value a container stack left in this shell
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := os.MkdirAll(p.secretsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p.pgPassword, []byte("pw"), 0o600); err != nil {
		t.Fatalf("write pg password: %v", err)
	}
	env, err := nativeEnv(Config{APIPort: 18080, PgPort: 15432, S3Port: 18333}, p, getFrom(nil), "img", ":18443", "/tmp/w")
	if err != nil {
		t.Fatalf("nativeEnv: %v", err)
	}
	seen := map[string]int{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		seen[k]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("%s appears %d times: the child reads the FIRST one, so an override behind an inherited value does nothing", k, n)
		}
	}
	if envMapOf(env)["PALAI_LISTEN_ADDR"] != "127.0.0.1:18080" {
		t.Fatalf("the inherited PALAI_LISTEN_ADDR won over this stack's own port")
	}
}

// TestNativeStopKillsTheProcessGroupAndClearsTheRecord is item 3: a `palai local down` that leaves
// a control plane holding the runner port is worse than one that never started. The child here is a
// real process in its own group with a real child of its own.
func TestNativeStopKillsTheProcessGroupAndClearsTheRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PALAI_HOME", home)
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 120 & sleep 120")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the stand-in process: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if err := writeNativeRecord(p, pid, "/bin/sh"); err != nil {
		t.Fatalf("write the pid record: %v", err)
	}

	stopped, err := stopNative(p)
	if err != nil {
		t.Fatalf("stopNative: %v", err)
	}
	if !stopped {
		t.Fatal("stopNative reported it stopped nothing while a recorded process was running")
	}
	if _, err := os.Stat(p.nativePID); !os.IsNotExist(err) {
		t.Fatalf("the pid record survived the stop (%v): the next `up` would refuse against a process that is gone", err)
	}
	if err := syscall.Kill(-pid, 0); err == nil {
		t.Fatal("the process group is still alive after stopNative")
	}
}

// TestNativeStopRefusesAPidThatIsNotTheControlPlane is the pid-reuse guard. A stale record plus a
// recycled pid is how a teardown kills somebody else's process.
func TestNativeStopRefusesAPidThatIsNotTheControlPlane(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PALAI_HOME", home)
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 120")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the stand-in process: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = syscall.Kill(-pid, syscall.SIGKILL) }()
	// The record claims this pid is the control plane; the process is a shell.
	if err := writeNativeRecord(p, pid, "/usr/local/bin/palai-control-plane"); err != nil {
		t.Fatalf("write the pid record: %v", err)
	}

	stopped, err := stopNative(p)
	if err == nil {
		t.Fatalf("a pid running something else was killed (stopped=%v): pid reuse would make `palai local down` kill an unrelated process", stopped)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the unrelated process was killed anyway: %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(pid)) {
		t.Fatalf("the refusal does not name the pid it refused to kill: %v", err)
	}
}

func TestNativeStopIsANoOpWithNoRecord(t *testing.T) {
	t.Setenv("PALAI_HOME", t.TempDir())
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	stopped, err := stopNative(p)
	if err != nil || stopped {
		t.Fatalf("stopNative on a stack that never ran one: stopped=%v err=%v", stopped, err)
	}
}

// TestNativeStopSurvivesAProcessThatAlreadyExited: a record left by a crashed control plane must not
// fail a teardown.
func TestNativeStopSurvivesAProcessThatAlreadyExited(t *testing.T) {
	t.Setenv("PALAI_HOME", t.TempDir())
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := writeNativeRecord(p, cmd.Process.Pid, "/usr/local/bin/palai-control-plane"); err != nil {
		t.Fatalf("write the pid record: %v", err)
	}
	// Give the kernel a moment to reap; the pid is this test's own child and is already a zombie.
	time.Sleep(50 * time.Millisecond)
	if _, err := stopNative(p); err != nil {
		t.Fatalf("a dead recorded process failed the teardown: %v", err)
	}
	if _, err := os.Stat(p.nativePID); !os.IsNotExist(err) {
		t.Fatal("the record of a dead process survived the stop")
	}
}

// envMapOf is the test's view of an environment slice.
func envMapOf(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := out[k]; !dup {
			out[k] = v // first wins, exactly as the child's os.Getenv does
		}
	}
	return out
}

// TestTheToolErrorBudgetReachesTheNativeControlPlane. MEASURED 2026-08-01, and the measurement is the
// reason this test exists rather than a review comment: with `PALAI_TOOL_ERROR_BUDGET=2` written in
// .env.local, a native run was handed FOUR tool refusals and completed. nativeEnv MERGES over
// os.Environ(), and .env.local is read through `get` rather than exported into this process — so a
// variable that lives only in that file and is not named here never reaches the child. A ceiling an
// operator can write in the file the documentation points them at, and that the process never sees, is
// a ceiling that does not exist.
func TestTheToolErrorBudgetReachesTheNativeControlPlane(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PALAI_HOME", home)
	// Prove it is NOT inherited from this process: the value can only arrive through the dotenv reader.
	// t.Setenv registers the restore, then the Unsetenv makes it genuinely ABSENT — `t.Setenv(k, "")`
	// leaves `k=` in os.Environ, which nativeEnv inherits, and would have made the second half of this
	// test pass for the wrong reason.
	t.Setenv("PALAI_TOOL_ERROR_BUDGET", "sentinel")
	if err := os.Unsetenv("PALAI_TOOL_ERROR_BUDGET"); err != nil {
		t.Fatalf("unset: %v", err)
	}
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := os.MkdirAll(p.secretsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p.pgPassword, []byte("hunter2"), 0o600); err != nil {
		t.Fatalf("write pg password: %v", err)
	}
	cfg := Config{Project: "palai-test", APIPort: 18080, RunnerPort: 18443, PgPort: 15432, S3Port: 18333}

	env, err := nativeEnv(cfg, p, getFrom(map[string]string{"PALAI_TOOL_ERROR_BUDGET": "2"}), "sha256:x", ":18443", "/tmp/w")
	if err != nil {
		t.Fatalf("nativeEnv: %v", err)
	}
	if got := envMapOf(env)["PALAI_TOOL_ERROR_BUDGET"]; got != "2" {
		t.Fatalf("PALAI_TOOL_ERROR_BUDGET = %q, want %q from .env.local", got, "2")
	}
	// And an operator who set nothing gets the binary's own default rather than an empty string, which
	// the control plane would read as unparseable.
	env, err = nativeEnv(cfg, p, getFrom(nil), "sha256:x", ":18443", "/tmp/w")
	if err != nil {
		t.Fatalf("nativeEnv: %v", err)
	}
	if _, set := envMapOf(env)["PALAI_TOOL_ERROR_BUDGET"]; set {
		t.Fatal("an unset budget was passed as an empty variable; unset must stay unset so the binary's default applies")
	}
}

// TestTheContainerPostureAlsoCarriesTheToolErrorBudget is the same claim for the OTHER deployment. The
// two postures build their environments in completely different places — a Go map here, a compose
// `environment:` block there — so a variable added to one and not the other is the everyday way a knob
// comes to work on a Mac and do nothing in Docker.
func TestTheContainerPostureAlsoCarriesTheToolErrorBudget(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "deploy", "compose", "compose.yaml"))
	if err != nil {
		t.Fatalf("read compose.yaml: %v", err)
	}
	if !strings.Contains(string(raw), "PALAI_TOOL_ERROR_BUDGET: ${PALAI_TOOL_ERROR_BUDGET:-}") {
		t.Fatal("compose.yaml does not pass PALAI_TOOL_ERROR_BUDGET to the control plane: the budget " +
			"documented in docs/operations/tool-errors.md would be unsettable on a container stack")
	}
}

// TestTheScriptedExchangeReachesBothPostures is the same two-posture claim for PALAI_FAKE_SCRIPT_FILE —
// the file the deterministic adapter replays, and the only way a stack with no provider credential drives
// a run that calls a tool. A variable that lives in .env.local and is named in neither posture's
// environment reaches nothing, and the run it was written to drive answers the built-in "ok" and calls
// nothing at all. That is not a visible failure: it is a proof that looks like it ran.
func TestTheScriptedExchangeReachesBothPostures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PALAI_HOME", home)
	// Genuinely absent from this process, so the value can only arrive through the dotenv reader (see
	// the budget test above for why t.Setenv(k, "") would not be absent).
	t.Setenv("PALAI_FAKE_SCRIPT_FILE", "sentinel")
	if err := os.Unsetenv("PALAI_FAKE_SCRIPT_FILE"); err != nil {
		t.Fatalf("unset: %v", err)
	}
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := os.MkdirAll(p.secretsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p.pgPassword, []byte("hunter2"), 0o600); err != nil {
		t.Fatalf("write pg password: %v", err)
	}
	cfg := Config{Project: "palai-test", APIPort: 18080, RunnerPort: 18443, PgPort: 15432, S3Port: 18333}

	env, err := nativeEnv(cfg, p, getFrom(map[string]string{"PALAI_FAKE_SCRIPT_FILE": "/tmp/uname.json"}), "sha256:x", ":18443", "/tmp/w")
	if err != nil {
		t.Fatalf("nativeEnv: %v", err)
	}
	if got := envMapOf(env)["PALAI_FAKE_SCRIPT_FILE"]; got != "/tmp/uname.json" {
		t.Fatalf("PALAI_FAKE_SCRIPT_FILE = %q, want the path from .env.local", got)
	}
	// Unset stays unset: an empty value is a path, and the control plane refuses a path it cannot read.
	env, err = nativeEnv(cfg, p, getFrom(nil), "sha256:x", ":18443", "/tmp/w")
	if err != nil {
		t.Fatalf("nativeEnv: %v", err)
	}
	if _, set := envMapOf(env)["PALAI_FAKE_SCRIPT_FILE"]; set {
		t.Fatal("an unrouted script was passed as an empty variable; unset must stay unset so the built-in script applies")
	}

	// The container posture, whose environment is a compose block rather than a Go map.
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "deploy", "compose", "compose.yaml"))
	if err != nil {
		t.Fatalf("read compose.yaml: %v", err)
	}
	if !strings.Contains(string(raw), "PALAI_FAKE_SCRIPT_FILE: ${PALAI_FAKE_SCRIPT_FILE:-}") {
		t.Fatal("compose.yaml does not pass PALAI_FAKE_SCRIPT_FILE to the control plane: a scripted " +
			"exchange would be settable on a Mac and do nothing in Docker")
	}
}
