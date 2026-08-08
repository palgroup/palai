// Command palai-session-worker runs one session's commands AS that session's account.
//
// ‼️ IT EXISTS BECAUSE THE CONTROL PLANE CANNOT DO THIS ITSELF, AND THE ARITHMETIC IS THE WHOLE REASON.
// Only uid 0 may become another uid. The control plane runs as the operator, so every attempt to drop
// into a session account refused — `adapters/sandboxes/host` has carried a syscall.Credential since
// 85b62252 and it was never once spent, because procAttrFor takes its refusing branch on every machine
// that exists. The daemon is the one uid 0 in this system, and it starts THIS program at the account's
// uid; from then on the work is already where it belongs and nothing has to drop into anything.
//
// SO THE ISOLATION IS A FACT ABOUT WHO STARTED THE PROCESS, not about a check this program performs. It
// holds no privilege, refuses nothing on the grounds of identity, and cannot be persuaded to act as
// anyone else: it is the account, and the only thing it can do is run commands as itself.
//
// WHOEVER OPENS ITS SOCKET CAN ACT AS THIS ACCOUNT, and that is stated plainly rather than mitigated,
// because the mitigation is the filesystem and not this program. The socket lives in the account's home,
// whose group is that slot's own group; the two members of that group are the account and the control
// plane. No other session on the machine can traverse in, which is precisely what the per-slot group was
// introduced for.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/palgroup/palai/packages/macagent"
)

// ‼️ THERE IS NO IDLE TIMEOUT, AND THAT IS A LIFECYCLE DECISION RATHER THAN AN OMISSION. A worker lives
// exactly as long as the account it IS: palai-agentd starts it when the account is created and kills it
// when the account is destroyed, which is when the session goes idle. An idle exit would have produced
// the one state this design cannot answer — an account with no worker — because nothing on the
// control-plane side can start one, and a command arriving after that timeout would fail on a machine
// that looks completely healthy.

// commandCeiling bounds any single command when the caller named no timeout. The control plane sets its
// own wall time and is the authority on it; this is the backstop for a request that arrived without one,
// so a worker cannot be left holding a process for the machine's uptime.
const commandCeiling = 60 * time.Minute

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if err := run(); err != nil {
		log.Fatalf("palai-session-worker: %v", err)
	}
}

func run() error {
	// ‼️ THE SLOT COMES FROM THIS PROCESS'S OWN uid AND FROM NOWHERE ELSE. There is no flag and no
	// environment variable for it, because the daemon starts this program with no argv at all (see
	// macagent.InstalledWorkerPath for why that asymmetry is the safety argument for the spawn verb) —
	// and because a worker that could be TOLD which slot it is would be a worker that could be told to
	// bind another account's socket.
	uid := os.Getuid()
	slot, ok := macagent.SlotFromUID(uid)
	if !ok {
		return fmt.Errorf("this process runs as uid %d, which is not one palai-agentd allocates (%d..%d); a session "+
			"worker must BE a session account, and starting one as anybody else would run a tenant's commands as that "+
			"principal", uid, macagent.UIDBase+1, macagent.UIDBase+macagent.MaxSlot)
	}
	// ‼️ THE SOCKET IS IN THE DIRECTORY THE DAEMON STARTED THIS PROCESS IN, which is the slot's allocation
	// directory. Derived from the cwd rather than from the slot, because the two principals that must
	// meet here reach it differently: the control plane OWNS that directory and a running plane can never
	// join this account's group. See macagent.WorkerSocketName.
	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("this worker cannot read its own working directory, so it has nowhere to listen: %w", err)
	}
	socket := macagent.WorkerSocketIn(dir)

	// A socket left by a previous worker is a node nothing is listening on, and bind fails on it. It is
	// inside this account's own home, so removing it takes nothing from anybody else.
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing the previous socket at %s: %w", socket, err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socket, err)
	}
	defer ln.Close()
	// ‼️ 0660 AND NOT 0600. The control plane is not this account — it is the other member of the slot's
	// group — and it is the only caller this socket has. 0600 would produce a worker nothing can reach,
	// which is the failure that looks exactly like a worker that is working.
	if err := os.Chmod(socket, 0o660); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", socket, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("session worker for slot %02d (uid %d) listening on %s", slot, uid, socket)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		serve(ctx, conn)
	}
}

// serve answers one command. It is DELIBERATELY SEQUENTIAL: one session's commands are one agent's
// commands, they act on one workspace, and running two at once would let a build and an edit interleave
// in a directory neither expects the other in. The control plane already serialises a run's tool calls;
// this is the same rule stated where it cannot be forgotten.
func serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	req, err := macagent.ReadWorkerRequest(conn)
	if err != nil {
		writeResponse(conn, macagent.WorkerResponse{Error: err.Error()})
		return
	}

	timeout := commandCeiling
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Dir
	// COMPLETE, never merged with this process's own. The control plane builds exactly what a command
	// may see; a worker that added its inherited environment would put back the variables that
	// allow-list exists to keep out. An empty Env from the caller means an empty environment, and that
	// is the honest reading of "the caller named none" — os/exec would otherwise inherit ours.
	cmd.Env = req.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	// Setpgid plus a group kill, so a wall-time expiry reaps what a build spawned rather than only the
	// shell at its root — the same shape adapters/sandboxes/host uses, for the same reason.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr capped
	stdout.limit, stderr.limit = workerCapBytes, workerCapBytes
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	resp := macagent.WorkerResponse{}
	runErr := cmd.Run()
	resp.Stdout, resp.Stderr = string(stdout.buf), string(stderr.buf)
	switch {
	case runErr == nil:
		resp.ExitCode = 0
	default:
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			// ‼️ A NON-ZERO EXIT IS AN ANSWER AND NEVER AN ERROR OF THIS TRANSPORT. Reporting it as a
			// worker failure is how every failing command in a run turns into a wedged run — the exact
			// defect this tree records under "a tool error is an ANSWER".
			resp.ExitCode = exit.ExitCode()
		} else {
			resp.Error = runErr.Error()
		}
	}
	writeResponse(conn, resp)
}

// workerCapBytes bounds what one command may hand back through this socket. It is a backstop rather than
// the product's limit — the control plane caps and redacts on its own side, where those rules live — and
// it exists so a runaway command cannot make this process hold a machine's memory.
const workerCapBytes = 32 << 20

func writeResponse(conn net.Conn, resp macagent.WorkerResponse) {
	body, err := json.Marshal(resp)
	if err != nil {
		// A response that cannot be encoded is one the caller must still be told about, and the only
		// thing left that certainly encodes is a fixed string.
		body = []byte(`{"error":"the session worker could not encode its response"}`)
	}
	if _, err := conn.Write(body); err != nil {
		log.Printf("writing the response failed: %v", err)
	}
}

// capped is a writer that keeps the FIRST bytes and drops the rest. First rather than last, because a
// command's opening lines carry what it was doing when it went wrong, and a tail keeps the least
// informative part of a runaway loop.
type capped struct {
	buf   []byte
	limit int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - len(c.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		c.buf = append(c.buf, p[:room]...)
	}
	return len(p), nil
}
