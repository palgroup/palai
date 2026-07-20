package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/contracts"
)

// EngineProtocolV1 is the JSONL frame protocol the supervisor speaks with an engine.
const EngineProtocolV1 = "engine.v1"

const sandboxLabelKey = "io.palai.sandbox"

// composeProjectLabelKey tags each engine sandbox with the compose project the runner
// belongs to (PALAI_COMPOSE_PROJECT, injected by `palai local up`), so `palai local down`
// can force-remove exactly this stack's orphaned engines without touching a concurrent
// stack's. It mirrors the compose project a co-located service carries.
const composeProjectLabelKey = "io.palai.project"

var (
	// ErrInvalidEngineOutput reports stdout that is not strict, in-protocol JSONL.
	ErrInvalidEngineOutput = errors.New("invalid engine output")
	// ErrStdoutLimit reports stdout that exceeded its configured bound.
	ErrStdoutLimit = errors.New("engine stdout exceeded configured bound")
	// ErrEngineTimeout reports an engine force-killed at the wall-time bound. It is a
	// terminal, lost outcome — never a success.
	ErrEngineTimeout = errors.New("engine exceeded wall-time bound")
	// ErrEngineExit reports a non-zero engine exit.
	ErrEngineExit = errors.New("engine exited unsuccessfully")
	// ErrForbiddenEnv reports a requested environment key outside the engine allowlist.
	ErrForbiddenEnv = errors.New("engine environment key is not on the allowlist")
)

// allowedEnvKey is the engine environment allowlist: only Palai application keys may
// be forwarded. Provider, database, object-store, and runner secrets (which never
// carry this prefix) are rejected, and the reserved keys below cannot be overridden.
var allowedEnvKey = regexp.MustCompile(`^PALAI_[A-Z0-9_]+$`)

// reservedEnvKeys are set by the supervisor or must never enter the engine, even
// though they match the allowlist prefix.
var reservedEnvKeys = map[string]bool{
	"PALAI_RUN_ID":             true,
	"PALAI_ATTEMPT_ID":         true,
	"PALAI_RUNNER_PRIVATE_KEY": true,
}

// Limits are the lease-carried execution bounds, wire-shaped (milliseconds and byte
// counts) as they arrive from the control plane.
type Limits struct {
	WallTimeMS      int64 `json:"wall_time_ms"`
	MaxStdoutBytes  int64 `json:"max_stdout_bytes"`
	MaxStderrBytes  int64 `json:"max_stderr_bytes"`
	MaxFrameBytes   int64 `json:"max_frame_bytes"`
	MaxMemoryBytes  int64 `json:"max_memory_bytes"`
	MaxProcessCount int64 `json:"max_process_count"`
}

func (l Limits) validate() error {
	if l.WallTimeMS <= 0 || l.MaxStdoutBytes <= 0 || l.MaxStderrBytes <= 0 ||
		l.MaxFrameBytes <= 0 || l.MaxMemoryBytes <= 0 || l.MaxProcessCount <= 0 {
		return errors.New("all lease resource and output bounds must be positive")
	}
	if l.MaxFrameBytes > l.MaxStdoutBytes {
		return errors.New("frame bound cannot exceed stdout bound")
	}
	return nil
}

// EngineRequest is one attempt to supervise: the pinned image, the run/attempt
// identity, the lease fencing token, an allowlisted environment, and the execution
// bounds. Fence is carried into the supervisor.hello as a hash so the engine can bind
// the handshake to the lease that authorized it (§25.6).
type EngineRequest struct {
	ImageDigest string
	RunID       contracts.RunID
	AttemptID   contracts.AttemptID
	Fence       uint64
	Env         map[string]string
	Limits      Limits
	// WorkspaceHostPath, when set, is the host allocation directory bind-mounted to /workspace in
	// the engine sandbox (spec §29.9). Empty means no workspace — the pre-E09 behaviour. ReadOnly
	// binds a child's read-only snapshot; a root writer binds read-write (spec §29.8, enforced in
	// E09 Task 6). The engine never learns the host path (§29.9 — exact host paths are hidden).
	WorkspaceHostPath string
	WorkspaceReadOnly bool
}

// workspaceMountTarget is the fixed in-sandbox mount point for a workspace allocation. The
// documented logical paths (/workspace/repo, /workspace/scratch, /workspace/artifacts) live
// beneath it (spec §29.9), laid out on the host allocation directory before the mount.
const workspaceMountTarget = "/workspace"

func (r EngineRequest) validate() error {
	if !r.RunID.Valid() || !r.AttemptID.Valid() {
		return errors.New("engine run and attempt IDs must be canonical identifiers")
	}
	for key := range r.Env {
		if !allowedEnvKey.MatchString(key) || reservedEnvKeys[key] {
			return fmt.Errorf("%w: %q", ErrForbiddenEnv, key)
		}
	}
	return r.Limits.validate()
}

// EngineResult is the classified outcome of a supervised attempt. Frames are the
// parsed, in-protocol stdout; they are populated only on a clean success.
type EngineResult struct {
	ContainerID     string
	ImageID         string
	ExitCode        int64
	Frames          []contracts.EngineFrame
	StdoutBytes     int64
	Stderr          []byte
	StderrBytes     int64
	StderrTruncated bool
}

// Supervisor runs an engine attempt in an OCI sandbox and enforces the engine JSONL
// protocol on its output. It owns the protocol discipline; the sandbox mechanics live
// behind the oci.Driver.
type Supervisor struct {
	driver oci.Driver
}

// NewSupervisor returns a supervisor backed by driver.
func NewSupervisor(driver oci.Driver) *Supervisor {
	return &Supervisor{driver: driver}
}

// Run supervises one engine attempt: it builds a hardened, allowlisted sandbox spec,
// runs it, and classifies the outcome. A timeout, oversized stdout, non-zero exit, or
// malformed frame stream each fails the attempt; only a clean run yields frames.
func (s *Supervisor) Run(ctx context.Context, request EngineRequest) (EngineResult, error) {
	if s == nil || s.driver == nil {
		return EngineResult{}, errors.New("supervisor is not initialized")
	}
	if err := request.validate(); err != nil {
		return EngineResult{}, err
	}

	outcome, err := s.driver.Run(ctx, buildSpec(request))
	result := EngineResult{
		ContainerID:     outcome.ContainerID,
		ImageID:         outcome.ImageID,
		ExitCode:        outcome.ExitCode,
		StdoutBytes:     outcome.StdoutBytes,
		Stderr:          outcome.Stderr,
		StderrBytes:     outcome.StderrBytes,
		StderrTruncated: outcome.StderrTruncated,
	}
	if err != nil {
		return result, err
	}
	if outcome.TimedOut {
		return result, ErrEngineTimeout
	}
	if outcome.StdoutTruncated {
		return result, ErrStdoutLimit
	}
	if outcome.ExitCode != 0 {
		return result, fmt.Errorf("%w: exit code %d", ErrEngineExit, outcome.ExitCode)
	}
	frames, err := parseEngineFrames(outcome.Stdout, request)
	if err != nil {
		return result, err
	}
	result.Frames = frames
	return result, nil
}

// buildSpec is the single hardened sandbox spec both the batch and streaming
// supervisors run: the pinned image, the allowlisted environment, the engine label,
// and the wall-time, memory, process, and per-stream output bounds. Sharing it keeps
// the streaming path's isolation identical to the batch path's.
func buildSpec(request EngineRequest) oci.ContainerSpec {
	spec := oci.ContainerSpec{
		ImageDigest: request.ImageDigest,
		Env:         buildEnv(request),
		Labels:      engineLabels(),
		Limits: oci.Limits{
			WallTime:        time.Duration(request.Limits.WallTimeMS) * time.Millisecond,
			MaxMemoryBytes:  request.Limits.MaxMemoryBytes,
			MaxProcessCount: request.Limits.MaxProcessCount,
			NanoCPUs:        1_000_000_000,
		},
		MaxStdoutBytes: request.Limits.MaxStdoutBytes,
		MaxStderrBytes: request.Limits.MaxStderrBytes,
	}
	// Attach the workspace allocation to /workspace when the lease carries one. A workspace-less
	// attempt (every pre-E09 run) mounts nothing and behaves exactly as before.
	if request.WorkspaceHostPath != "" {
		spec.Mounts = []oci.Mount{{
			Source:   request.WorkspaceHostPath,
			Target:   workspaceMountTarget,
			ReadOnly: request.WorkspaceReadOnly,
		}}
	}
	return spec
}

// engineLabels are the leak-accounting labels every engine sandbox carries: the base
// sandbox marker, plus this stack's compose project (PALAI_COMPOSE_PROJECT) when the runner
// runs inside compose, so `palai local down` can force-remove exactly this project's orphans
// and never a concurrent stack's.
func engineLabels() map[string]string {
	labels := map[string]string{sandboxLabelKey: "engine"}
	if project := os.Getenv("PALAI_COMPOSE_PROJECT"); project != "" {
		labels[composeProjectLabelKey] = project
	}
	return labels
}

// buildEnv is the exact environment the engine receives: an empty base (so no host
// value leaks through a well-known name), the supervisor-owned run/attempt identity,
// and the validated caller allowlist. The host environment is never inherited.
func buildEnv(request EngineRequest) []string {
	env := []string{
		"HOME=",
		"HOSTNAME=",
		"PALAI_RUN_ID=" + string(request.RunID),
		"PALAI_ATTEMPT_ID=" + string(request.AttemptID),
		"PATH=",
	}
	extra := make([]string, 0, len(request.Env))
	for key, value := range request.Env {
		extra = append(extra, key+"="+value)
	}
	sort.Strings(extra)
	return append(env, extra...)
}

// parseEngineFrames enforces the strict-JSONL engine protocol: newline-terminated,
// per-frame bounded, canonical envelope with a monotonic sequence and matching run
// identity. Any deviation fails the attempt rather than surfacing partial frames.
func parseEngineFrames(output []byte, request EngineRequest) ([]contracts.EngineFrame, error) {
	if len(output) == 0 || output[len(output)-1] != '\n' {
		return nil, fmt.Errorf("%w: stdout must be non-empty newline-delimited JSON", ErrInvalidEngineOutput)
	}
	lines := bytes.Split(output[:len(output)-1], []byte{'\n'})
	frames := make([]contracts.EngineFrame, 0, len(lines))
	ledger := NewFrameLedger()
	for index, line := range lines {
		if len(line) == 0 || int64(len(line)) > request.Limits.MaxFrameBytes {
			return nil, fmt.Errorf("%w: frame %d violates frame bound", ErrInvalidEngineOutput, index+1)
		}
		var frame contracts.EngineFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			return nil, fmt.Errorf("%w: frame %d is not JSON", ErrInvalidEngineOutput, index+1)
		}
		if err := validateFrame(frame, index, request); err != nil {
			return nil, err
		}
		// Frame-id discipline (ENG-002), the same the streaming path enforces: a reused id
		// with a changed payload is a protocol violation. The strict positional sequence
		// above already makes every batch frame unique, so this only rejects the conflict.
		if _, err := ledger.Admit(frame); err != nil {
			return nil, err
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

func validateFrame(frame contracts.EngineFrame, index int, request EngineRequest) error {
	if err := validateEnvelope(frame, request); err != nil {
		return err
	}
	if frame.Sequence != index+1 {
		return fmt.Errorf("%w: frame %d is out of sequence", ErrInvalidEngineOutput, index+1)
	}
	return nil
}

// validateEnvelope enforces the frame rules shared by the batch and streaming
// supervisors: the engine protocol, a canonical frame id, a non-empty type, an RFC 3339
// timestamp, and matching run/attempt identity. The monotonic sequence is applied by
// each caller — batch by output position, streaming against the last accepted sequence.
func validateEnvelope(frame contracts.EngineFrame, request EngineRequest) error {
	if frame.Protocol != EngineProtocolV1 || !frame.ID.Valid() || frame.Type == "" {
		return fmt.Errorf("%w: frame %s violates the engine envelope", ErrInvalidEngineOutput, frame.ID)
	}
	if _, err := time.Parse(time.RFC3339, frame.Time); err != nil {
		return fmt.Errorf("%w: frame %s has no valid timestamp", ErrInvalidEngineOutput, frame.ID)
	}
	if frame.RunID != "" && frame.RunID != request.RunID {
		return fmt.Errorf("%w: frame %s run identity mismatch", ErrInvalidEngineOutput, frame.ID)
	}
	if frame.AttemptID != "" && frame.AttemptID != request.AttemptID {
		return fmt.Errorf("%w: frame %s attempt identity mismatch", ErrInvalidEngineOutput, frame.ID)
	}
	return nil
}
