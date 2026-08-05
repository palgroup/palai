package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/macagent"
)

// TestAMachineWithoutTheAgentSocketIsRefusedEnrolment is the fail-closed half of this phase, and until
// it existed the tree did the OPPOSITE — measured on this machine, 2026-08-05:
//
//	ls /var/run/palai-agentd.sock                       -> No such file or directory
//	ls /Library/LaunchDaemons/net.pallasite.*.plist     -> No such file or directory
//	dscl . -read /Groups/palai                          -> eDSRecordNotFound
//
// and a native bring-up enrolled anyway, noticed none of it, and looked like it was working. Every
// tenant then ran as uid 501 — the operator's own uid — so the 0700 mode on each home directory
// protected nothing: one principal, and permissions between a principal and itself are not a boundary.
//
// FAIL-CLOSED DELETES THE THIRD STATE. Of a hundred Macs each is provably isolated or not in the pool;
// the expensive one is the machine in between, and it is expensive because it is indistinguishable from
// a healthy one everywhere except here.
func TestAMachineWithoutTheAgentSocketIsRefusedEnrolment(t *testing.T) {
	if runtime.GOOS != "darwin" {
		// The gate is deliberately macOS-only: palai-agentd IS sysadminctl and dscl, and requiring it of
		// a Linux host would refuse machines for a mechanism that does not exist there. Skipping keeps
		// that ceiling honest instead of asserting a refusal CI could never see.
		t.Skip("session accounts are a macOS mechanism; the gate is scoped to darwin and says so")
	}

	t.Run("the unsandboxed-host posture with no daemon is refused", func(t *testing.T) {
		t.Setenv("PALAI_SHELL_NATIVE", "unsandboxed-host")
		t.Setenv("PALAI_SANDBOX_IMAGE", "")

		err := admitMachine(context.Background(), absentDaemon(t))
		if err == nil {
			t.Fatal("a Mac that will run tenant commands as its own uid, with no palai-agentd to make each tenant a different uid, was ADMITTED")
		}
		if !errors.Is(err, macagent.ErrNoIsolation) {
			t.Errorf("the refusal does not carry ErrNoIsolation, so a caller cannot tell it from a network failure: %v", err)
		}
		// The operator reads this on a machine that will not take work. It has to name the mechanism, or
		// the next person looks at the control plane.
		for _, want := range []string{"palai-agentd", "unsandboxed-host"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not mention %q: %v", want, err)
			}
		}
	})

	t.Run("the same machine WITH a daemon answering is admitted", func(t *testing.T) {
		// Without this the test above would pass against a gate that refused everything, which is a gate
		// nobody could ship.
		t.Setenv("PALAI_SHELL_NATIVE", "unsandboxed-host")
		t.Setenv("PALAI_SANDBOX_IMAGE", "")

		if err := admitMachine(context.Background(), presentDaemon(t)); err != nil {
			t.Fatalf("a Mac whose palai-agentd is answering was refused: %v", err)
		}
	})

	t.Run("the container posture is not gated on a mechanism it does not use", func(t *testing.T) {
		// There the boundary is the sandbox, not the account. Requiring the daemon here would refuse
		// every machine in every deployment that exists today.
		t.Setenv("PALAI_SHELL_NATIVE", "")
		t.Setenv("PALAI_SANDBOX_IMAGE", "palai/sandbox:pinned")

		if err := admitMachine(context.Background(), absentDaemon(t)); err != nil {
			t.Fatalf("a container runner was refused for not having a macOS daemon: %v", err)
		}
	})

	t.Run("a machine that declares no posture at all is not gated", func(t *testing.T) {
		// Every stack built before E22 is in this state, and it runs no shell commands anywhere.
		t.Setenv("PALAI_SHELL_NATIVE", "")
		t.Setenv("PALAI_SANDBOX_IMAGE", "")

		if err := admitMachine(context.Background(), absentDaemon(t)); err != nil {
			t.Fatalf("a runner with no declared shell posture was refused: %v", err)
		}
	})
}

// TestTheAdmissionGateRunsBeforeEnrolmentOnTheRealSocket reads main's own body, because a gate that is
// written and not CALLED is the exact shape this tree keeps finding — and here the ORDER is half the
// property: a refusal arriving after runner.Enroll would be a machine the control plane has already
// issued a certificate to and counted.
//
// There is no behaviour to observe for this: admitMachine cannot watch itself being called, and a test
// that drove the whole binary would need a control plane. So the call site is what is read.
func TestTheAdmissionGateRunsBeforeEnrolmentOnTheRealSocket(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "main" && fn.Recv == nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("main.go declares no main(); everything below would be vacuously green")
	}

	gate, enrol, socket := -1, -1, false
	for i, stmt := range body.List {
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				if fn.Name == "admitMachine" && gate < 0 {
					gate = i
					// AND ON THE REAL SOCKET. A gate wired to a path production never uses would pass
					// this ordering check and measure nothing on a machine.
					socket = strings.Contains(exprText(fset, call), "macagent.DefaultSocketPath")
				}
			case *ast.SelectorExpr:
				if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "runner" && fn.Sel.Name == "Enroll" && enrol < 0 {
					enrol = i
				}
			}
			return true
		})
	}

	switch {
	case gate < 0:
		t.Fatal("main() never calls admitMachine: the fail-closed gate is written and nothing reaches it")
	case enrol < 0:
		t.Fatal("main() never calls runner.Enroll; this test can no longer tell whether the gate precedes it")
	case gate >= enrol:
		t.Errorf("admitMachine is called at statement %d and runner.Enroll at %d: a machine refused AFTER enrolment is a machine the control plane has already admitted", gate, enrol)
	}
	if !socket {
		t.Error("main() calls admitMachine with a prober that is not macagent.DefaultSocketPath: a gate pointed anywhere else measures nothing on a real machine")
	}
}

// exprText renders a node back to source so an argument can be read as written.
func exprText(fset *token.FileSet, node ast.Node) string {
	start := fset.Position(node.Pos()).Offset
	end := fset.Position(node.End()).Offset
	src, err := os.ReadFile("main.go")
	if err != nil || end > len(src) {
		return ""
	}
	return string(src[start:end])
}

// absentDaemon is a prober pointed at a socket that does not exist and never will — the state this
// machine is genuinely in. Only the CLOCK is substituted, so the shipped attempt count is still spent.
func absentDaemon(t *testing.T) *macagent.Prober {
	t.Helper()
	dir, err := os.MkdirTemp("", "admit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	prober := macagent.NewProber(filepath.Join(dir, "s.sock"))
	prober.Sleep = func(time.Duration) {}
	return prober
}

// presentDaemon is a prober pointed at a real unix socket answering `list` the way palai-agentd does.
func presentDaemon(t *testing.T) *macagent.Prober {
	t.Helper()
	prober := absentDaemon(t)
	ln, err := net.Listen("unix", prober.SocketPath)
	if err != nil {
		t.Fatalf("binding the stand-in daemon: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
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
				if strings.HasPrefix(string(buf[:n]), "version") {
					_, _ = conn.Write([]byte("ok version 1.0.0\n"))
					return
				}
				_, _ = conn.Write([]byte("ok list 07\n"))
			}()
		}
	}()
	return prober
}
