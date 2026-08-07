package macagent

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

// WorkerSocket is where slot N's session worker listens, and it is DERIVED FROM THE SLOT on both sides
// exactly the way an account name is: the worker binds it from its own uid, the control plane joins it
// from the slot it was given, and neither tells the other a path.
//
// ‼️ IT IS INSIDE THE ACCOUNT'S HOME, WHICH IS WHAT MAKES THE PERMISSION ARGUMENT SHORT. The home is
// owned by the session account and its group is that slot's own group — the one the control plane is
// also a member of (see [SlotGroupName]) — so the two principals that must reach this socket are exactly
// the two the directory admits, and no third account on the machine can even traverse into it. A socket
// in a shared directory would have needed a rule about who may connect; this one needs none, because the
// filesystem already says it.
func WorkerSocket(slot int) (string, error) {
	home, err := HomeDir(slot)
	if err != nil {
		return "", err
	}
	return home + "/.palai-session.sock", nil
}

// WorkerRequest is one command for a session worker to run AS ITSELF.
//
// ‼️ READ THIS AS THE INVERSE OF [Request]. The daemon's protocol is closed because the daemon is root:
// it takes an integer and decides everything else. This one carries an argv, because the worker is not
// privileged — it IS the tenant, and running the tenant's own commands as the tenant is the entire
// reason it exists. Whoever can open the socket can already act as that account; the boundary is the
// directory the socket sits in, not the shape of what travels over it.
//
// The environment is COMPLETE and never merged with the worker's own. The control plane already builds
// exactly what a command may see (adapters/sandboxes/host's allow-list plus the attempt's own values),
// and a worker that added its inherited environment on top would put back the variables that list exists
// to keep out.
type WorkerRequest struct {
	Dir       string   `json:"dir"`
	Argv      []string `json:"argv"`
	Env       []string `json:"env"`
	TimeoutMS int      `json:"timeout_ms,omitempty"`
}

// WorkerResponse is what came back. Stdout and stderr are RAW: capping and redaction happen on the
// control-plane side, where they always have, so this transport cannot become a second place those
// rules are written and disagree.
//
// Error is for failures of the WORKER, never of the command. A command that exits 3 is a successful
// round trip with ExitCode 3 — this tree's own rule that a tool error is an answer, applied at the one
// boundary where it is easiest to get wrong.
type WorkerResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Error    string `json:"error,omitempty"`
}

// workerFrameLimit bounds one message in either direction. A build's output is the large case and it is
// already capped control-plane side well below this; the limit is here so a worker cannot be made to
// allocate without bound by whoever opens its socket, and so a control plane cannot be either.
const workerFrameLimit = 64 << 20

// AskWorker sends one command to a session worker and returns its answer.
//
// THE DEADLINE IS THE CALLER'S, and it is set on the CONNECTION rather than left to the request's own
// TimeoutMS, because those bound different things: the worker kills a command that overruns, and this
// bounds a worker that stopped answering at all. A caller that only set the first would wait forever on
// a worker that died mid-command.
func AskWorker(dial func() (net.Conn, error), req WorkerRequest, deadline time.Duration) (WorkerResponse, error) {
	conn, err := dial()
	if err != nil {
		return WorkerResponse{}, fmt.Errorf("dial session worker: %w", err)
	}
	defer conn.Close()
	if deadline > 0 {
		_ = conn.SetDeadline(time.Now().Add(deadline))
	}

	body, err := json.Marshal(req)
	if err != nil {
		return WorkerResponse{}, fmt.Errorf("encode worker request: %w", err)
	}
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return WorkerResponse{}, fmt.Errorf("write worker request: %w", err)
	}
	// Half-close so the worker sees EOF on its read. Without it a worker reading to EOF would block
	// forever and the deadline above would be the only thing that ever ended the exchange.
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}

	raw, err := io.ReadAll(io.LimitReader(conn, workerFrameLimit))
	if err != nil {
		return WorkerResponse{}, fmt.Errorf("read worker response: %w", err)
	}
	var resp WorkerResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return WorkerResponse{}, fmt.Errorf("decode worker response (%d bytes): %w", len(raw), err)
	}
	if resp.Error != "" {
		return resp, fmt.Errorf("session worker: %s", resp.Error)
	}
	return resp, nil
}

// ReadWorkerRequest is the worker's half of the same frame, kept here so the two ends cannot drift.
func ReadWorkerRequest(r io.Reader) (WorkerRequest, error) {
	raw, err := io.ReadAll(io.LimitReader(r, workerFrameLimit))
	if err != nil {
		return WorkerRequest{}, err
	}
	var req WorkerRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return WorkerRequest{}, fmt.Errorf("decode worker request (%d bytes): %w", len(raw), err)
	}
	if len(req.Argv) == 0 {
		return WorkerRequest{}, fmt.Errorf("a worker request carries no argv")
	}
	return req, nil
}
