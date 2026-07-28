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

// getFrom is the .env.local lookup shape resolveProvider/applySlackEnv already use.
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
		"PALAI_SECRET_PROVIDER_ONE":    "sk-test-value",
		"PALAI_SECRET_MASTER_KEY_FILE": p.masterKey,
		"PALAI_BOOTSTRAP_API_KEY_FILE": p.apiKey,
		"PALAI_RUNNER_SERVER_CERT":     p.serverCert,
		"PALAI_ENROLLMENT_TOKEN_FILE":  p.runnerToken,
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
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
