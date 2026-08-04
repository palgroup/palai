package stack

// The native RUNNER's half of `palai up --native` (A.3 T4). What these pin is not "the runner can be
// native" but the two facts that make the Mac case work at all: it reaches a control plane that has no
// DNS name on this machine, and it is the ONLY runner in the pool while it does.

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func nativeRunnerTestConfig() Config {
	return Config{Project: "palai-test", APIPort: 18080, RunnerPort: 18443, PgPort: 15432, S3Port: 18333,
		BaseURL: "http://127.0.0.1:18080", ControllerDNS: controllerDNS}
}

// TestTheNativeBringUpStartsNoContainerRunner is the ambiguity guard, and it is the reason this task
// touches compose at all. Two runners in one pool means a command lands on whichever the gateway hands
// out — on a Mac, a coin toss between Xcode and no Xcode — which is exactly the "where did this run"
// question A.3 exists to make answerable.
func TestTheNativeBringUpStartsNoContainerRunner(t *testing.T) {
	services := nativeComposeServices()
	if len(services) == 0 {
		t.Fatal("the native bring-up starts no compose services at all, so this guard has checked nothing")
	}
	for _, s := range services {
		if s == "runner" {
			t.Fatalf("the native bring-up starts the compose service %q as well as the native runner: two machines "+
				"in one pool means a command lands on whichever the gateway hands out", s)
		}
	}
	// The backing services it DOES start, so a future edit cannot satisfy this by starting nothing.
	for _, want := range []string{"postgres", "object-store"} {
		if !containsString(services, want) {
			t.Errorf("the native bring-up no longer starts %q", want)
		}
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestTheNativeRunnerDialsLoopbackAndVerifiesTheNameOnTheCertificate is the fact that makes a native
// runner possible without touching /etc/hosts or minting a second certificate.
//
// certs.go signs ONE SAN — `control-plane` — and packages/runner refuses a leaf that does not carry
// exactly that one name. The CONTAINER satisfied it by resolving `control-plane` to the host gateway. A
// process on this machine has no such alias, and it does not need one: the dialled address and the
// verified identity are separate fields, so the connection goes to loopback while the name checked stays
// the name on the certificate.
func TestTheNativeRunnerDialsLoopbackAndVerifiesTheNameOnTheCertificate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PALAI_HOME", home)
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	got := envMapOf(nativeRunnerEnv(nativeRunnerTestConfig(), p, getFrom(nil), "sha256:deadbeef", "/tmp/palai-workspaces"))

	if want := "https://127.0.0.1:18443"; got["PALAI_CONTROLLER_URL"] != want {
		t.Errorf("PALAI_CONTROLLER_URL = %q, want %q — `control-plane` is a compose network name and resolves to "+
			"nothing on this machine", got["PALAI_CONTROLLER_URL"], want)
	}
	if got["PALAI_CONTROLLER_DNS"] != controllerDNS {
		t.Errorf("PALAI_CONTROLLER_DNS = %q, want %q — it is the TLS ServerName and the exact-SAN check, and the "+
			"certificate carries only that one name", got["PALAI_CONTROLLER_DNS"], controllerDNS)
	}
	// A container path in a native process names a file that is not there.
	for name, v := range got {
		if strings.HasPrefix(name, "PALAI_") && strings.HasPrefix(v, "/palai/") {
			t.Errorf("%s=%s is the CONTAINER's mount path: this process has no /palai", name, v)
		}
	}
	// PALAI_CONTROLLER_CA, not PALAI_RUNNER_CA_CERT: cmd/runner's loadConfig calls
	// mustEnv("PALAI_CONTROLLER_CA") directly (main.go:174). PALAI_RUNNER_CA_CERT is the compose/systemd
	// CONTRACT name — deploy/compose/runner-entrypoint.sh and scripts/package/runner/palai-runner.sh both
	// bridge it to PALAI_CONTROLLER_CA before exec'ing the binary. This process has no such bridge script,
	// so it has to hand the binary the name the binary actually reads.
	if got["PALAI_CONTROLLER_CA"] != p.caCert || got["PALAI_ENROLLMENT_TOKEN_FILE"] != p.runnerToken {
		t.Errorf("the CA and the enrollment token are not at their host paths: ca=%q token=%q",
			got["PALAI_CONTROLLER_CA"], got["PALAI_ENROLLMENT_TOKEN_FILE"])
	}
	// The session URL is a WEBSOCKET dial (packages/runner/session.go refuses anything not prefixed
	// wss://), and cmd/runner's own derivedEnv/joinPath fallback does not do that scheme swap — it just
	// appends the path onto PALAI_CONTROLLER_URL, which is https://. Both shipped bridge scripts special-case
	// this one URL for exactly that reason; this process must too, or the runner's own loadConfig hands
	// Session.Connect an "https://" URL and it refuses before it ever dials.
	if want := "wss://127.0.0.1:18443/v1/runner/connect"; got["PALAI_SESSION_URL"] != want {
		t.Errorf("PALAI_SESSION_URL = %q, want %q — cmd/runner's own derivation does not swap https for wss, "+
			"so this must be set explicitly or the session dial refuses at the prefix check", got["PALAI_SESSION_URL"], want)
	}
}

// TestTheNativeRunnerCarriesTheShellPostureAndTheWorkspaceRoot pins the two values without which this
// whole posture is pointless: the posture is what lets the machine run a command at all, and the root is
// where that command's workspace is.
//
// The posture is read through `get` (.env.local) rather than inherited, which is the lesson the
// tool-error budget cost this tree: a value that lives only in that file reaches no child unless it is
// named.
func TestTheNativeRunnerCarriesTheShellPostureAndTheWorkspaceRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PALAI_HOME", home)
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	get := getFrom(map[string]string{
		"PALAI_SHELL_NATIVE":       nativeShellPosture,
		"PALAI_RUNNER_CONCURRENCY": "2",
	})
	got := envMapOf(nativeRunnerEnv(nativeRunnerTestConfig(), p, get, "sha256:deadbeef", "/tmp/palai-workspaces"))

	if got["PALAI_SHELL_NATIVE"] != nativeShellPosture {
		t.Fatalf("PALAI_SHELL_NATIVE = %q, want %q — without it this machine runs no command and being native "+
			"bought nothing", got["PALAI_SHELL_NATIVE"], nativeShellPosture)
	}
	if got["PALAI_WORKSPACE_ROOT"] != "/tmp/palai-workspaces" {
		t.Errorf("PALAI_WORKSPACE_ROOT = %q, want the root the control plane mints under", got["PALAI_WORKSPACE_ROOT"])
	}
	if got["PALAI_RUNNER_CONCURRENCY"] != "2" {
		t.Errorf("PALAI_RUNNER_CONCURRENCY = %q, want 2 from .env.local", got["PALAI_RUNNER_CONCURRENCY"])
	}
	// An UNSET posture must not become a default. Declaring it deletes a security boundary, so it is the
	// operator's sentence to write and never this command's.
	bare := envMapOf(nativeRunnerEnv(nativeRunnerTestConfig(), p, getFrom(nil), "sha256:deadbeef", "/tmp/x"))
	if v, present := bare["PALAI_SHELL_NATIVE"]; present && v != "" {
		t.Errorf("an undeclared posture became %q: deleting the sandbox boundary must be declared, never defaulted", v)
	}
}

// TestNativeRunnerEnvironmentHasNoDuplicateKeys is the same non-style check its control-plane twin is:
// os.Getenv in the child returns the FIRST occurrence of a key, so an override appended to os.Environ()
// is silently ignored.
func TestNativeRunnerEnvironmentHasNoDuplicateKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PALAI_HOME", home)
	t.Setenv("PALAI_CONTROLLER_URL", "https://control-plane:8443") // what a container stack left in this shell
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	env := nativeRunnerEnv(nativeRunnerTestConfig(), p, getFrom(nil), "sha256:deadbeef", "/tmp/palai-workspaces")

	seen := map[string]int{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		seen[k]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("%s appears %d times: the child reads the first, so the override is ignored", k, n)
		}
	}
	if got := envMapOf(env)["PALAI_CONTROLLER_URL"]; got != "https://127.0.0.1:18443" {
		t.Fatalf("PALAI_CONTROLLER_URL = %q: the inherited container value won over this posture's", got)
	}
}

// TestNativeRunnerStopKillsTheProcessGroupAndClearsTheRecord. The GROUP matters more here than for the
// control plane: since A.3 this is the process an `xcodebuild` hangs off, so a stop that signalled one
// pid would leave a compiler running.
func TestNativeRunnerStopKillsTheProcessGroupAndClearsTheRecord(t *testing.T) {
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
	if err := writeNativeRunnerRecord(p, pid, "/bin/sh"); err != nil {
		t.Fatalf("write the pid record: %v", err)
	}

	stopped, err := stopNativeRunner(p)
	if err != nil {
		t.Fatalf("stopNativeRunner: %v", err)
	}
	if !stopped {
		t.Fatal("stopNativeRunner reported it stopped nothing while a recorded process was running")
	}
	if _, err := os.Stat(p.nativeRunnerPID); !os.IsNotExist(err) {
		t.Fatalf("the pid record survived the stop (%v)", err)
	}
	// POLLED RATHER THAN ASSERTED ONCE. stopNativeRunner waits for the group LEADER to go, and a child
	// the leader backgrounded can outlive it by a moment — so a single check here races the kernel. The
	// control plane's twin of this test asserts immediately and flakes about one run in three
	// (TestNativeStopKillsTheProcessGroupAndClearsTheRecord, measured 2026-08-04); this is the same
	// property without the race.
	for i := 0; i < 200; i++ {
		if err := syscall.Kill(-pid, 0); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the process group is still alive two seconds after stopNativeRunner")
}

// TestNativeRunnerStopRefusesAPidThatIsNotTheRunner is the pid-reuse guard: a stale record plus a
// recycled pid is how a teardown kills somebody else's process.
func TestNativeRunnerStopRefusesAPidThatIsNotTheRunner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PALAI_HOME", home)
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 120")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the stand-in process: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if err := writeNativeRunnerRecord(p, pid, "/opt/palai/bin/palai-runner"); err != nil {
		t.Fatalf("write the pid record: %v", err)
	}

	stopped, err := stopNativeRunner(p)
	if err == nil {
		t.Fatal("stopNativeRunner killed a pid that is running something else")
	}
	if stopped {
		t.Error("stopNativeRunner reported a stop it refused to make")
	}
	if !strings.Contains(err.Error(), p.nativeRunnerPID) {
		t.Errorf("the refusal does not name the record an operator has to delete: %v", err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("the unrelated process was killed anyway: %v", err)
	}
}

// TestTheRunnerSessionGaugeIsReadFromTheValueAndNotTheHelpText. Prometheus exposition repeats a metric's
// name on its # HELP and # TYPE lines, so a scraper that matched the name anywhere would parse the word
// "gauge" as a session count — and a bring-up would report a runner that never connected.
func TestTheRunnerSessionGaugeIsReadFromTheValueAndNotTheHelpText(t *testing.T) {
	exposition := "# HELP palai_runner_sessions Runner sessions currently connected to the gateway; 0 means no runner.\n" +
		"# TYPE palai_runner_sessions gauge\n" +
		"palai_runner_sessions 2\n"
	got, err := scrapeGauge(exposition, "palai_runner_sessions")
	if err != nil {
		t.Fatalf("scrapeGauge: %v", err)
	}
	if got != 2 {
		t.Fatalf("palai_runner_sessions = %v, want 2", got)
	}
	if _, err := scrapeGauge("# TYPE palai_runner_sessions gauge\n", "palai_runner_sessions"); err == nil {
		t.Fatal("an exposition carrying only HELP/TYPE lines yielded a value: a runner that never connected " +
			"would be reported as up")
	}
}
