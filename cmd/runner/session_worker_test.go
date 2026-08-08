package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/macagent"
)

// TestTheMachineRunnerIsNotSpawnedAsASessionAccount pins the half of A.5 T4 that is about what did NOT
// change, and it is worth a test because the change it guards against would look like progress.
//
// THERE ARE TWO PROCESSES ON A MAC AND THEY ARE NOT INTERCHANGEABLE. The MACHINE-level runner enrols,
// renews, polls settings and parks for leases; its identity is a certificate the control plane issued to
// this box, and it runs as the operator — the account macagent.Install put in the `palai` group, which
// is the entire credential for reaching the daemon. The SESSION worker is per slot, is `palai-sNN`, and
// holds one tenant's work. Making the first one a session account would not tighten anything: it would
// take the machine's identity, its group membership and therefore its ability to ask for accounts at
// all, and hand them to a principal that is deleted when the session ends.
//
// So the claim is that nothing on the enrolment path asks palai-agentd to run anything, and it is
// asserted from both sides — what goes over the wire, and what the source can even say.
func TestTheMachineRunnerIsNotSpawnedAsASessionAccount(t *testing.T) {
	t.Run("the bring-up probe asks only to list and to name, never to spawn", func(t *testing.T) {
		// The SHIPPED prober — the one admitMachine builds — driven against a socket that records every
		// line it is sent. Only the clock is substituted, so the real attempt policy is spent.
		prober, sent := recordingDaemon(t)

		// The prober directly: it is what reportIsolation and device.Measure both drive, and it runs on
		// the Linux box CI uses as well as on a Mac because the socket is the fake one above.
		health, err := prober.Probe(context.Background())
		if err != nil {
			t.Fatalf("a machine whose daemon is answering was refused: %v", err)
		}

		lines := sent()
		if len(lines) == 0 {
			t.Fatal("the probe sent nothing, so every assertion below is vacuous")
		}
		// The anchor. Without it a probe that had silently stopped talking to the daemon would satisfy
		// the refusal below by asking for nothing at all.
		if !containsVerb(lines, "list") {
			t.Fatalf("the probe never sent a `list`; it sent %q, and a bring-up that measures nothing is not "+
				"the thing this test is about", lines)
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "spawn") {
				t.Errorf("the machine runner's bring-up sent %q. Enrolment must not run as a session account: "+
					"the certificate, the hostname and the `palai` group membership all belong to the machine, "+
					"and palai-sNN is deleted when its session ends.", line)
			}
		}
		if len(health.Slots) == 0 {
			t.Error("the probe reported no slots, so it did not complete a round trip with the stand-in daemon")
		}
	})

	t.Run("no source under the machine runner names the spawn verb", func(t *testing.T) {
		// The other direction. The wire test above can only observe the path it drove; this reads what the
		// package is ABLE to say, which is the only way to catch a spawn wired onto a branch no test takes.
		found, anchored := macagentSelectors(t, ".", filepath.Join("..", "..", "packages", "runner"))
		if !anchored {
			t.Fatal("the scan found no macagent reference at all in cmd/runner or packages/runner, so it " +
				"asserts nothing about VerbSpawn either")
		}
		// ‼️ THE VERBS, AND NOT THE PATH. InstalledWorkerPath was on this list until 2026-08-08, and
		// narrowing it is a change of SELECTOR and not of rule: the property is that the process which
		// enrols this box never runs as a session account, and PLACING a program is not RUNNING as it.
		// `palai agentd install` lays down the worker beside the daemon — the same privileged step, on
		// the same machine, by the same operator — and then says whether it landed, because a machine
		// with a daemon and no worker mints accounts whose every command refuses. Spawning is what must
		// stay out of this binary, and spawning is exactly what these two names are.
		for _, name := range []string{"VerbSpawn", "OKSpawn"} {
			if found[name] {
				t.Errorf("the machine runner references macagent.%s. The process that enrols this box must not "+
					"be the process that holds a tenant's session.", name)
			}
		}
	})
}

func containsVerb(lines []string, verb string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, verb) {
			return true
		}
	}
	return false
}

// absentDaemon is a prober pointed at a path where no socket exists, with the clock substituted so the
// real attempt policy is spent without the wall time. It lives here because this file is its only user:
// the admission suite that owned it asserted a refusal that no longer exists.
// os.MkdirTemp, NOT t.TempDir, and the reason is a limit rather than a preference: t.TempDir bakes the
// test's NAME into the path, and a unix socket address is capped at ~104 bytes on Darwin. Binding under
// this test's name fails with `bind: invalid argument`, which reads like a permissions problem and is
// not one.
func absentDaemon(t *testing.T) *macagent.Prober {
	t.Helper()
	dir, err := os.MkdirTemp("", "palai-probe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	prober := macagent.NewProber(filepath.Join(dir, "s.sock"))
	prober.Sleep = func(time.Duration) {}
	return prober
}

// recordingDaemon is presentDaemon with a transcript: it answers the way palai-agentd does and keeps
// every request line, so a test can assert on what was ASKED rather than on what came back.
func recordingDaemon(t *testing.T) (*macagent.Prober, func() []string) {
	t.Helper()
	prober := absentDaemon(t)
	ln, err := net.Listen("unix", prober.SocketPath)
	if err != nil {
		t.Fatalf("binding the stand-in daemon: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	var lines []string
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, macagent.MaxRequestBytes)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				line := strings.TrimSpace(string(buf[:n]))
				mu.Lock()
				lines = append(lines, line)
				mu.Unlock()
				if strings.HasPrefix(line, "version") {
					_, _ = conn.Write([]byte("ok version 1.0.0\n"))
					return
				}
				_, _ = conn.Write([]byte("ok list 07\n"))
			}()
		}
	}()
	return prober, func() []string {
		// The prober's exchanges are synchronous, but the handler goroutine records after the read; give
		// it the moment it needs rather than racing it. A transcript read too early is the "completion
		// signal already true" shape this tree has recorded twice.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(lines)
			mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

// macagentSelectors reports every macagent.X referenced in the non-test sources of the given
// directories, and whether the scan saw any macagent reference at all.
func macagentSelectors(t *testing.T, dirs ...string) (map[string]bool, bool) {
	t.Helper()
	found := map[string]bool{}
	any := false
	for _, dir := range dirs {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", dir, err)
		}
		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				ast.Inspect(file, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "macagent" {
						found[sel.Sel.Name] = true
						any = true
					}
					return true
				})
			}
		}
	}
	return found, any
}

// TestTheSERVINGFormAcceptsItsOwnFlags — THE REFUSAL KILLED THE ENROLLED AGENT.
//
// `palai <unknown>` must refuse rather than fall through to the serving loop; that shipped on
// 2026-08-08 and it read os.Args[1] as a VERB. The LaunchAgent `palai enroll` writes invokes
//
//	/usr/local/bin/palai --config /Users/<u>/Library/Application Support/Palai/agent.json
//
// so the product's own service invocation became "unknown command", and every machine that took the
// update printed usage and exited. Measured in the agent's own log on this Mac the same day.
//
// A LEADING DASH IS THE SERVING FORM. Flags belong to `palai`; words belong to a subcommand; only the
// words can be wrong. This asserts the source draws that line, because the alternative — driving main()
// — would start a daemon inside the test suite.
func TestTheSERVINGFormAcceptsItsOwnFlags(t *testing.T) {
	body, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	if !strings.Contains(src, `!strings.HasPrefix(os.Args[1], "-")`) {
		t.Error("the unknown-command refusal does not exempt flags: the shipped LaunchAgent passes " +
			"`--config <path>`, so every enrolled machine that takes this build prints usage and exits " +
			"instead of serving")
	}
	// AND THE PLIST IS THE OTHER HALF. If enrolment stopped passing --config this guard would still pass
	// while describing a contract nobody has; anchor on the writer.
	plist, err := os.ReadFile(filepath.Join("..", "..", "packages", "device", "service.go"))
	if err == nil && !strings.Contains(string(plist), "--config") {
		t.Error("packages/device no longer writes --config into the service definition; this guard is " +
			"protecting a shape that is gone")
	}
}
