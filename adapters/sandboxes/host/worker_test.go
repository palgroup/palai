package host

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/macagent"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// standInWorker speaks the worker protocol over a real unix socket and RUNS WHAT IT IS SENT, using the
// same os/exec shape the shipped worker uses.
//
// ‼️ IT RUNS THE COMMAND RATHER THAN ANSWERING A CANNED RESULT, and that is the difference between this
// proving a transport and proving a posture. A fake that returned a fixed exit code would pass for an
// executor that never sent the argv at all — which is exactly the class of green this tree keeps
// finding. What it does NOT reproduce is the uid: only root can start a process as another account, so
// the drop itself is measured on a real Mac by scripts/live/ios-demo-smoke and asserted here only in
// the shape of the exchange.
func standInWorker(t *testing.T, socket string, seen *macagent.WorkerRequest) {
	t.Helper()
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("bind %s: %v", socket, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				req, err := macagent.ReadWorkerRequest(conn)
				if err != nil {
					body, _ := json.Marshal(macagent.WorkerResponse{Error: err.Error()})
					_, _ = conn.Write(body)
					return
				}
				*seen = req
				out, runErr := runLocally(req)
				body, _ := json.Marshal(out)
				_ = runErr
				_, _ = conn.Write(body)
			}()
		}
	}()
}

// runLocally is the stand-in's exec, and it classifies a non-zero exit the way the shipped worker does:
// as an ANSWER carrying the code, never as a failure of the transport.
func runLocally(req macagent.WorkerRequest) (macagent.WorkerResponse, error) {
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Dir
	cmd.Env = req.Env
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	resp := macagent.WorkerResponse{Stdout: stdout.String(), Stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		resp.ExitCode = exit.ExitCode()
	default:
		resp.Error = err.Error()
	}
	return resp, nil
}

// TestACommandRunsTHROUGHTheWorkerWhenThisProcessCannotDrop — THE POSTURE THAT REFUSED EVERY COMMAND.
//
// Only uid 0 may become another uid, and a control plane runs as the operator. So on every Mac that
// minted per-session accounts, procAttrFor took its refusing branch and the shell tool returned an
// error instead of running anywhere — measured on this machine on 2026-08-08, with the agent reporting
// "I am unable to list the files in the repository due to permission restrictions".
//
// This drives the shipped Executor at a non-root euid with a RunAs set, and asserts the command reached
// a worker and its output came back. The assertion is on the OUTPUT and not on the dial, because a dial
// that connected and sent nothing is the vacuous version of this test.
func TestACommandRunsTHROUGHTheWorkerWhenThisProcessCannotDrop(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test is about the branch a NON-root executor takes")
	}
	dir, err := os.MkdirTemp("", "pw")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "w.sock")
	var seen macagent.WorkerRequest
	standInWorker(t, socket, &seen)

	// The dial is redirected rather than the path: WorkerSocket derives from the account's home, which
	// this process cannot create, and a test that rewrote the derivation would stop measuring it.
	restore := dialWorker
	dialWorker = func(string) (net.Conn, error) { return net.Dial("unix", socket) }
	t.Cleanup(func() { dialWorker = restore })

	workspace := filepath.Join(dir, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("workspace: %v", err)
	}

	e := NewExecutor(30 * time.Second)
	result, err := e.Run(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"/bin/echo", "palai-worker-round-trip"},
		WorkspaceRoot: workspace,
		RunAs:         &toolbroker.RunAs{UID: macagent.UIDBase + 7, GID: macagent.GIDBase + 7},
	})
	if err != nil {
		t.Fatalf("a command that cannot drop privilege must go THROUGH the worker, not fail: %v", err)
	}
	if !strings.Contains(result.Stdout, "palai-worker-round-trip") {
		t.Errorf("stdout = %q, want the command's own output: the executor reached a worker but the command "+
			"did not run, which is a transport that proves nothing", result.Stdout)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	// AND THE REQUEST CARRIED THE WORKSPACE, because a command that ran in the worker's own home would
	// act on the account's scratch space rather than on the tenant's checkout — the failure that looks
	// like an agent whose edits vanish.
	if seen.Dir != workspace {
		t.Errorf("the worker was asked to run in %q, want the attempt's workspace %q", seen.Dir, workspace)
	}
	if len(seen.Env) == 0 {
		t.Error("the worker was sent an EMPTY environment: the allow-list and the attempt's own values are " +
			"built control-plane side precisely so both postures see the same thing")
	}
}

// TestAFailingCommandIsAnANSWERThroughTheWorker — a non-zero exit is a result, never a transport error.
// The tree records what the other reading costs: every Exec error wedged the run.
func TestAFailingCommandIsAnANSWERThroughTheWorker(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test is about the branch a NON-root executor takes")
	}
	dir, err := os.MkdirTemp("", "pw")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "w.sock")
	var seen macagent.WorkerRequest
	standInWorker(t, socket, &seen)
	restore := dialWorker
	dialWorker = func(string) (net.Conn, error) { return net.Dial("unix", socket) }
	t.Cleanup(func() { dialWorker = restore })

	e := NewExecutor(30 * time.Second)
	result, err := e.Run(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"/bin/sh", "-c", "exit 3"},
		WorkspaceRoot: dir,
		RunAs:         &toolbroker.RunAs{UID: macagent.UIDBase + 7, GID: macagent.GIDBase + 7},
	})
	if err != nil {
		t.Fatalf("a command that exited non-zero was reported as a transport failure: %v", err)
	}
	if result.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3 — the command's own answer", result.ExitCode)
	}
}

// TestNoWorkerNamesBOTHFailures — the two have different cures and neither message alone identifies
// which machine an operator is looking at: this process is not root (normal and permanent), and the
// worker that makes that irrelevant is not there (an install that needs finishing).
func TestNoWorkerNamesBOTHFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test is about the branch a NON-root executor takes")
	}
	restore := dialWorker
	dialWorker = func(string) (net.Conn, error) { return nil, errors.New("connect: no such file or directory") }
	t.Cleanup(func() { dialWorker = restore })

	e := NewExecutor(time.Second)
	_, err := e.Run(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"/bin/echo", "x"},
		WorkspaceRoot: t.TempDir(),
		RunAs:         &toolbroker.RunAs{UID: macagent.UIDBase + 7, GID: macagent.GIDBase + 7},
	})
	if err == nil {
		t.Fatal("a session with no worker ran its command somewhere; there is nowhere it could have run")
	}
	if !errors.Is(err, ErrCannotDropPrivilege) {
		t.Errorf("the error does not wrap ErrCannotDropPrivilege, so a caller cannot classify it: %v", err)
	}
	if !strings.Contains(err.Error(), "session worker") {
		t.Errorf("the error names only the privilege drop and not the missing worker, so an operator is "+
			"pointed at a fact they cannot change: %v", err)
	}
}
