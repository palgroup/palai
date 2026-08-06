package stack

// The native RUNNER's half of `palai up --native` (A.3 T4). What these pin is not "the runner can be
// native" but the two facts that make the Mac case work at all: it reaches a control plane that has no
// DNS name on this machine, and it is the ONLY runner in the pool while it does.

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
	// ‼️ THE SESSION URL IS NOT SET HERE, AND THIS ASSERTS ITS ABSENCE. It used to be, with a comment
	// saying cmd/runner's derivation "does not swap https for wss" — true when written, and the reason
	// all three bridges special-cased one URL while the binary's own path went unexercised. loadConfig
	// derives it now (outboundSessionURL), so setting it here would restore the third copy of a
	// derivation whose whole defect was having three.
	//
	// The absence is what the guard has to hold: a map that starts setting it again would silently make
	// this process the only one whose session URL is not the binary's.
	if got, set := got["PALAI_SESSION_URL"]; set {
		t.Errorf("PALAI_SESSION_URL = %q, want it UNSET: the binary derives it, and a fourth copy of that "+
			"derivation is how the first three came to disagree with it", got)
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

// TestTheNativeRunnerCarriesTheDeclaredCapacity is the second half of a link that is useless alone: this
// process calls exec.Command on the binary directly, so a variable not NAMED in the forward list below
// never reaches the runner however carefully .env.local sets it. The same shape this tree already records
// for compose, which passes only the keys it interpolates.
//
// Together with cmd/runner's reader this is what makes `runners.capacity` reachable at all: before both,
// the column was declarable, stored and enforced, and no machine had ever sent a value.
func TestTheNativeRunnerCarriesTheDeclaredCapacity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PALAI_HOME", home)
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}

	get := getFrom(map[string]string{"PALAI_RUNNER_CAPACITY": "1"})
	got := envMapOf(nativeRunnerEnv(nativeRunnerTestConfig(), p, get, "sha256:deadbeef", "/tmp/palai-workspaces"))
	if got["PALAI_RUNNER_CAPACITY"] != "1" {
		t.Fatalf("PALAI_RUNNER_CAPACITY = %q, want 1 from .env.local — unforwarded, the ceiling is unreachable "+
			"on the one posture this epic measures", got["PALAI_RUNNER_CAPACITY"])
	}

	// UNSET STAYS UNSET. A default here would put a ceiling on every native machine that nobody chose —
	// the clamp the store deleted for exactly that reason.
	bare := envMapOf(nativeRunnerEnv(nativeRunnerTestConfig(), p, getFrom(nil), "sha256:deadbeef", "/tmp/x"))
	if v, present := bare["PALAI_RUNNER_CAPACITY"]; present && v != "" {
		t.Errorf("an undeclared capacity became %q: a ceiling must be declared, never defaulted", v)
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
	//
	// ‼️ THE BOUND IS A LIVENESS BOUND, NOT A SPEED ONE, and at two seconds it was the second. Measured
	// 2026-08-06: this test passes in 0.33s in isolation and failed TWICE inside `make verify` on a
	// machine at load 12–60 — a red that belongs to the machine and not to the product, which is the
	// worse kind because it blocks every ship while naming the wrong thing. The property is that the
	// group DIES; how many milliseconds a loaded kernel needs to reap it is not this test's subject.
	// Fifteen seconds costs nothing on an idle machine (the loop returns on the first iteration) and
	// stops a busy one from failing a claim it actually satisfies.
	for i := 0; i < 1500; i++ {
		if err := syscall.Kill(-pid, 0); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the process group is still alive fifteen seconds after stopNativeRunner")
}

// TestNativeRunnerStopWaitsForAChildThatOutlivesItsLeader — THE PROMISE IS THE GROUP, AND THE LOOP USED
// TO WATCH THE LEADER.
//
// ‼️ THIS DEFECT'S ONLY WITNESS WAS A LOAD-DEPENDENT RED, which is why it survived. stopNativeRunner's
// wait called processCommand(pid) — one process — so a leader that left on SIGTERM ended the wait while a
// child it had spawned was still running, and the next statement deleted the pid record, the only handle
// anything had on that group. The test above could not see it: its children take SIGTERM and die, so on
// an idle machine the group really was gone. It failed inside `make verify` on 2026-08-06 with the group
// alive fifteen seconds later, passed three times in isolation, and reads exactly like a flake.
//
// So the child here IGNORES SIGTERM. Now the difference between watching the leader and watching the
// group is not a race that a fast machine hides — it is the whole outcome, every run, on any machine.
//
// The leader sleeps rather than exiting immediately, because stopNativeRunner returns EARLY for a pid
// that is already gone (a stale record) and the fixture would then be measuring that arm instead.
func TestNativeRunnerStopWaitsForAChildThatOutlivesItsLeader(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PALAI_HOME", home)
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	// Short enough to keep this a fast test, long enough that the SIGTERM really is given its chance
	// first — at zero the escalation would be indistinguishable from killing outright.
	previous := nativeRunnerStopGrace
	nativeRunnerStopGrace = 300 * time.Millisecond
	t.Cleanup(func() { nativeRunnerStopGrace = previous })

	// ‼️ THE TRAPPING SHELL MUST BE THE ONE THAT SURVIVES, AND THE FIRST WRITING OF THIS LINE WAS
	// `trap "" TERM; sleep 120`. That protects the SHELL and not the `sleep` it runs in the foreground:
	// SIGTERM killed the sleep, the shell's command returned, and the shell exited — so the whole group
	// died on SIGTERM and the perturbation that should have reddened this test stayed GREEN. A busy loop
	// keeps the ignoring process itself alive: each `sleep 1` child dies to the signal and the shell,
	// which cannot receive it, spawns another.
	//
	// ‼️ AND IT TOUCHES A MARKER, BECAUSE THE FIXTURE'S SECOND WRITING WAS STILL VACUOUS. The perturbation
	// passed in 0.01s: SIGTERM reached the group before the outer shell had forked the child at all, so
	// what was signalled was a group of one and the whole thing died no matter which loop ran. The test
	// was measuring a world in which its own subject did not exist yet — the shape this tree records as a
	// fixture making the case unreachable — and only waiting for the child makes the two loops differ.
	marker := filepath.Join(home, "child-is-up")
	cmd := exec.Command("/bin/sh", "-c",
		fmt.Sprintf(`/bin/sh -c 'trap "" TERM; : > %s; while :; do sleep 1; done' & sleep 120`, marker))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the stand-in process: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	// ‼️ THE LEADER IS REAPED, AND WITHOUT THIS LINE THIS TEST PROVED NOTHING. A child no one calls Wait on
	// becomes a ZOMBIE, and a zombie is still a row in `ps` — so processCommand() answers "running" for a
	// leader that has already exited. The leader-watching loop this test exists to fail therefore never
	// broke early either: it ran to its deadline, escalated to SIGKILL, and killed the group anyway. The
	// perturbation stayed GREEN and the fixture was the reason, not the product. Reaping makes the leader
	// really gone, which is the only state in which watching it and watching the group differ.
	go func() { _ = cmd.Wait() }()
	if err := writeNativeRunnerRecord(p, pid, "/bin/sh"); err != nil {
		t.Fatalf("write the pid record: %v", err)
	}
	// WAIT FOR THE SUBJECT TO EXIST. Without this the stop below races the fork and signals a group of one.
	var up bool
	for i := 0; i < 200 && !up; i++ {
		if _, err := os.Stat(marker); err == nil {
			up = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !up {
		t.Fatal("the TERM-ignoring child never started, so nothing below is a statement about the group")
	}

	if _, err := stopNativeRunner(p); err != nil {
		t.Fatalf("stopNativeRunner: %v", err)
	}
	// ASSERTED THE INSTANT IT RETURNS, WITH NO POLLING, and that is the point rather than an economy.
	// The record is already deleted by now — a group still alive here is one nothing can name again, so
	// "it goes away shortly afterwards" is not a weaker version of this claim, it is a different one.
	if !groupGone(pid) {
		t.Fatal("stopNativeRunner returned with a live member still in the group, and it has already " +
			"deleted the pid record: whatever is running is now unreachable — no `palai` verb, no operator " +
			"and no later stop can name it, and it holds this Mac's workspace until someone finds it by hand")
	}
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

// TestThePrintedEnrolCommandCarriesTheLocalCA is a guard on a SENTENCE, and it is here because the
// sentence is an instruction an operator copies and runs.
//
// ‼️ IT DID NOT WORK. The line `up --native` printed for a plane with no agent omitted `--ca-file`, so a
// device following it fell back to the SYSTEM trust store — where this stack's self-signed local CA is
// not. On macOS the refusal is not even "unknown authority": Apple's verifier rejects a certificate whose
// validity exceeds 825 days FIRST, and `palai init` mints a ten-year local CA, so the operator reads
// `x509: "control-plane" certificate is not standards compliant` and has no path from that message to the
// missing flag. Measured 2026-08-06 by running the printed line against a live stack.
//
// The path is asserted to be the one this bring-up actually wrote, not merely that a flag is present: a
// --ca-file pointing somewhere else is the same failure with an extra step.
func TestThePrintedEnrolCommandCarriesTheLocalCA(t *testing.T) {
	body, err := os.ReadFile("native.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	const printed = "palai enroll --url https://127.0.0.1:%d --server-name %s --ca-file %s --key-file <pool key>"
	if !strings.Contains(source, printed) {
		t.Fatalf("the enrol line this bring-up prints is not %q — an operator who copies it reaches the "+
			"system trust store, which does not carry this stack's local CA", printed)
	}
	// The argument must be the CA this bring-up wrote. `p.caCert` is that path (config.go), and naming it
	// here is what stops the flag from being satisfied by an empty string or a placeholder.
	if !strings.Contains(source, "cfg.RunnerPort, cfg.ControllerDNS, p.caCert)") {
		t.Fatal("the printed --ca-file is not filled from p.caCert: a flag pointing anywhere else is the " +
			"same failure with an extra step")
	}
}
