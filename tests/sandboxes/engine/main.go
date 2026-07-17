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
