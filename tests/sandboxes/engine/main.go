// Command engine is the fixture engine image the runner Docker suites supervise.
// It is not a real engine: it emits one canonical engine.v1 frame (or deliberately
// bad output) selected by PALAI_ENGINE_MODE so the fault and security tiers can
// prove kill classification, JSONL bounds, and container isolation against a real
// Docker daemon. It reads only the allowlisted PALAI_* environment the supervisor
// injects and inspects its own sandbox for authority it must never receive.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const engineProtocol = "engine.v1"

// frame mirrors the canonical EngineFrame envelope (protocol,id,type,sequence,time)
// so a valid emission round-trips through the supervisor's contracts.EngineFrame.
type frame struct {
	Protocol  string    `json:"protocol"`
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	RunID     string    `json:"run_id"`
	AttemptID string    `json:"attempt_id"`
	Sequence  uint64    `json:"sequence"`
	Time      time.Time `json:"time"`
	Data      any       `json:"data"`
}

func main() {
	// The E09 Task 4 sandbox shell suites drive this same image with an explicit argv (`san <behavior>
	// ...`) rather than the PALAI_ENGINE_MODE env switch, so one fixture backs both the engine-protocol
	// tiers and the workspace shell-tool tiers.
	if len(os.Args) > 1 && os.Args[1] == "san" {
		os.Exit(sandboxCommand(os.Args[2:]))
	}

	mode := os.Getenv("PALAI_ENGINE_MODE")
	runID := os.Getenv("PALAI_RUN_ID")
	attemptID := os.Getenv("PALAI_ATTEMPT_ID")
	switch mode {
	case "valid":
		emit(runID, attemptID, map[string]any{"status": "completed"})
	case "malformed":
		fmt.Println("{not-json")
	case "oversized":
		emit(runID, attemptID, map[string]any{"padding": strings.Repeat("x", 64*1024)})
	case "stderr":
		_, _ = fmt.Fprint(os.Stderr, strings.Repeat("e", 4*1024))
		emit(runID, attemptID, map[string]any{"status": "completed"})
	case "inspect":
		emit(runID, attemptID, inspectAuthority())
	case "workspace":
		emit(runID, attemptID, probeWorkspace())
	case "workspace_stream":
		workspaceStream(runID, attemptID)
	case "interactive":
		interactive(runID, attemptID)
	case "hang":
		time.Sleep(30 * time.Second)
	default:
		fmt.Fprintln(os.Stderr, "unsupported fixture mode")
		os.Exit(2)
	}
}

func emit(runID, attemptID string, data any) {
	value := frame{
		Protocol:  engineProtocol,
		ID:        "frm_fixture1",
		Type:      "run.terminal",
		RunID:     runID,
		AttemptID: attemptID,
		Sequence:  1,
		Time:      time.Now().UTC(),
		Data:      data,
	}
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, "encode fixture frame")
		os.Exit(2)
	}
}

// interactive is the streaming fixture for the runner fault tier: it performs the §25.6
// handshake, requests one model step, writes a sentinel secret to stderr (proving the
// supervisor masks it before forwarding), then blocks for the controller's model.result
// before emitting a terminal. A container kill while it blocks proves the supervisor
// classifies the attempt lost/failed — never a false success — and stdin closing before
// a result exits non-zero for the same reason. Sequences are contiguous (1,2,3), as the
// supervisor's monotonic-sequence gate requires.
func interactive(runID, attemptID string) {
	seq := 0
	emitFrame := func(typ string, data any) {
		seq++
		value := frame{
			Protocol:  engineProtocol,
			ID:        fmt.Sprintf("frm_interactive%d", seq),
			Type:      typ,
			RunID:     runID,
			AttemptID: attemptID,
			Sequence:  uint64(seq),
			Time:      time.Now().UTC(),
			Data:      data,
		}
		if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
			fmt.Fprintln(os.Stderr, "encode interactive frame")
			os.Exit(2)
		}
	}

	stdin := bufio.NewScanner(os.Stdin)
	stdin.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if !stdin.Scan() { // supervisor.hello
		fmt.Fprintln(os.Stderr, "no supervisor.hello before run input")
		os.Exit(1)
	}
	emitFrame("engine.ready", map[string]any{
		"selected_protocol": engineProtocol,
		"engine":            map[string]any{"name": "palai-fault-fixture", "version": "0"},
		"max_frame_bytes":   1048576,
		"nonce":             "fault-nonce",
	})

	// A secret-shaped token on stderr the supervisor is required to redact.
	fmt.Fprintln(os.Stderr, "provider auth failed token=sk-live-FAULTREDACTSENTINEL0123456789")

	emitFrame("model.request", map[string]any{"model_request_id": "mreq_interactive1"})
	if !stdin.Scan() { // blocks for model.result; a kill or stdin-close ends the attempt here
		os.Exit(1)
	}
	emitFrame("run.terminal", map[string]any{"outcome": "completed"})
}

// workspaceStream is the E09 Task 1 live-mount fixture: it completes the §25.6 handshake, reads the
// seed the runner staged in the bind-mounted /workspace, requests one model step (the runner bridges
// it to the REAL provider), then — after the real model.result returns — writes into the allocation
// and terminates reporting what it saw. It proves the real /workspace mount is present and live
// across a real provider round-trip. Honest ceiling: the engine is a deterministic fixture and the
// model does not itself drive a file tool (that is E09 Task 4); the mount and the provider round-trip
// are real.
func workspaceStream(runID, attemptID string) {
	seq := 0
	emitFrame := func(typ string, data any) {
		seq++
		value := frame{
			Protocol:  engineProtocol,
			ID:        fmt.Sprintf("frm_workspace%d", seq),
			Type:      typ,
			RunID:     runID,
			AttemptID: attemptID,
			Sequence:  uint64(seq),
			Time:      time.Now().UTC(),
			Data:      data,
		}
		if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
			fmt.Fprintln(os.Stderr, "encode workspace frame")
			os.Exit(2)
		}
	}

	stdin := bufio.NewScanner(os.Stdin)
	stdin.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if !stdin.Scan() { // supervisor.hello
		fmt.Fprintln(os.Stderr, "no supervisor.hello before run input")
		os.Exit(1)
	}
	emitFrame("engine.ready", map[string]any{
		"selected_protocol": engineProtocol,
		"engine":            map[string]any{"name": "palai-workspace-fixture", "version": "0"},
		"max_frame_bytes":   1048576,
		"nonce":             "workspace-nonce",
	})

	// Read the seed the runner staged at /workspace/repo/seed before the model step.
	seed, readErr := os.ReadFile("/workspace/repo/seed")

	emitFrame("model.request", map[string]any{"model_request_id": "mreq_workspace1"})
	if !stdin.Scan() { // block for the real provider's model.result
		os.Exit(1)
	}

	// The mount is still live after the real model round-trip: echo the seed the engine read back
	// into the allocation, so the single persisted file proves the mount was both readable (it
	// carries the seed) and writable (the runner reads it back on the host). This does not rely on
	// the final run.terminal frame, which a fast clean exit can race out of the streaming sink.
	writeErr := os.WriteFile("/workspace/scratch/out", []byte("seed:"+string(seed)), 0o644)
	emitFrame("run.terminal", map[string]any{
		"outcome":           "completed",
		"workspace_present": pathExists("/workspace"),
		"seed_readable":     readErr == nil,
		"seed_content":      string(seed),
		"wrote":             writeErr == nil,
	})
}

// inspectAuthority reports whether any credential, the Docker socket, or the runner
// key leaked into the sandbox, plus the exact environment the engine can see.
func inspectAuthority() map[string]any {
	forbiddenNames := []string{
		"OPENAI_API_KEY",
		"ANTROPHIC_API_KEY",
		"ANTHROPIC_API_KEY",
		"DATABASE_URL",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"PALAI_RUNNER_PRIVATE_KEY",
	}
	present := make([]string, 0)
	for _, name := range forbiddenNames {
		if _, exists := os.LookupEnv(name); exists {
			present = append(present, name)
		}
	}
	dockerSocketPresent := pathExists("/var/run/docker.sock") || pathExists("/run/docker.sock")
	runnerKeyPresent := pathExists("/secrets/runner.key")
	environmentNames := make([]string, 0)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		environmentNames = append(environmentNames, name)
	}
	sort.Strings(environmentNames)
	return map[string]any{
		"forbidden_environment": present,
		"environment_names":     environmentNames,
		"docker_socket_present": dockerSocketPresent,
		"runner_key_present":    runnerKeyPresent,
	}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// probeWorkspace reports what the engine container sees at the bind-mounted /workspace: whether
// the mount is present, the seed file the control plane staged is readable, and whether a write
// into the allocation succeeds (the runner reads it back on the host to prove persistence). It is
// the fixture half of the E09 Task 1 real-mount proof — the model itself does not touch /workspace
// yet (that is the file tool, E09 Task 4).
func probeWorkspace() map[string]any {
	seed, readErr := os.ReadFile("/workspace/repo/seed")
	result := map[string]any{
		"workspace_present": pathExists("/workspace"),
		"seed_readable":     readErr == nil,
		"seed_content":      string(seed),
	}
	writeErr := os.WriteFile("/workspace/scratch/out", []byte("engine-wrote-this"), 0o644)
	result["wrote"] = writeErr == nil
	if writeErr != nil {
		result["write_error"] = writeErr.Error()
	}
	return result
}
