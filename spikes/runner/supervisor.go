package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const (
	EngineProtocolV1 = "engine.v1"
	spikeLabelKey    = "io.palai.spike"
	spikeLabelValue  = "runner-supervisor"
)

var (
	ErrMutableImage        = errors.New("engine image must be an immutable ID")
	ErrInvalidEngineOutput = errors.New("invalid engine output")
	ErrStdoutLimit         = errors.New("engine stdout exceeded configured bound")
	ErrEngineTimeout       = errors.New("engine exceeded wall-time bound")
	ErrEngineExit          = errors.New("engine exited unsuccessfully")
	engineIdentifier       = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	engineMode             = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

type SupervisorConfig struct {
	OperationTimeout time.Duration
}

type Supervisor struct {
	client           *client.Client
	operationTimeout time.Duration
}

type EngineRequest struct {
	ImageID   string
	RunID     string
	AttemptID string
	Mode      string
	Limits    LeaseLimits
}

type EngineFrame struct {
	Protocol  string          `json:"protocol"`
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	RunID     string          `json:"run_id"`
	AttemptID string          `json:"attempt_id"`
	Sequence  uint64          `json:"sequence"`
	ReplyTo   *string         `json:"reply_to"`
	Time      time.Time       `json:"time"`
	Data      json.RawMessage `json:"data"`
}

type EngineResult struct {
	ContainerID     string
	ImageID         string
	ExitCode        int64
	Frames          []EngineFrame
	StdoutBytes     int64
	Stderr          []byte
	StderrBytes     int64
	StderrTruncated bool
}

func NewSupervisor(configuration SupervisorConfig) (*Supervisor, error) {
	if configuration.OperationTimeout <= 0 {
		configuration.OperationTimeout = 5 * time.Second
	}
	apiClient, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return &Supervisor{client: apiClient, operationTimeout: configuration.OperationTimeout}, nil
}

func (s *Supervisor) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func (request EngineRequest) Validate() error {
	if !immutableDigestPattern.MatchString(request.ImageID) {
		return ErrMutableImage
	}
	if !engineIdentifier.MatchString(request.RunID) || !engineIdentifier.MatchString(request.AttemptID) {
		return errors.New("engine run and attempt IDs are invalid")
	}
	if !engineMode.MatchString(request.Mode) {
		return errors.New("engine fixture mode is invalid")
	}
	return request.Limits.Validate()
}

func (s *Supervisor) Run(ctx context.Context, request EngineRequest) (result EngineResult, runErr error) {
	if s == nil || s.client == nil {
		return result, errors.New("supervisor is not initialized")
	}
	if err := request.Validate(); err != nil {
		return result, err
	}
	inspectContext, cancelInspect := s.operationContext(ctx)
	image, err := s.client.ImageInspect(inspectContext, request.ImageID)
	cancelInspect()
	if err != nil {
		return result, fmt.Errorf("inspect engine image: %w", err)
	}
	if image.ID != request.ImageID {
		return result, fmt.Errorf("%w: daemon resolved %s", ErrMutableImage, image.ID)
	}
	result.ImageID = image.ID

	processLimit := request.Limits.MaxProcessCount
	createContext, cancelCreate := s.operationContext(ctx)
	created, err := s.client.ContainerCreate(createContext, client.ContainerCreateOptions{
		Image: request.ImageID,
		Config: &container.Config{
			User:            "65532:65532",
			AttachStdout:    true,
			AttachStderr:    true,
			NetworkDisabled: true,
			Env: []string{
				"HOME=",
				"HOSTNAME=",
				"PALAI_ENGINE_MODE=" + request.Mode,
				"PALAI_RUN_ID=" + request.RunID,
				"PALAI_ATTEMPT_ID=" + request.AttemptID,
				"PATH=",
			},
			Labels: map[string]string{
				spikeLabelKey: spikeLabelValue,
			},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode("none"),
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Resources: container.Resources{
				Memory:     request.Limits.MaxMemoryBytes,
				MemorySwap: request.Limits.MaxMemoryBytes,
				NanoCPUs:   1_000_000_000,
				PidsLimit:  &processLimit,
			},
		},
	})
	cancelCreate()
	if err != nil {
		return result, fmt.Errorf("create engine container: %w", err)
	}
	result.ContainerID = created.ID
	defer func() {
		cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), s.operationTimeout)
		defer cancelCleanup()
		_, cleanupErr := s.client.ContainerRemove(cleanupContext, created.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		if cleanupErr != nil && !cerrdefs.IsNotFound(cleanupErr) {
			runErr = errors.Join(runErr, fmt.Errorf("remove engine container: %w", cleanupErr))
		}
	}()

	startContext, cancelStart := s.operationContext(ctx)
	_, err = s.client.ContainerStart(startContext, created.ID, client.ContainerStartOptions{})
	cancelStart()
	if err != nil {
		return result, fmt.Errorf("start engine container: %w", err)
	}

	wallContext, cancelWall := context.WithTimeout(ctx, time.Duration(request.Limits.WallTimeMS)*time.Millisecond)
	wait := s.client.ContainerWait(wallContext, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case response := <-wait.Result:
		result.ExitCode = response.StatusCode
		if response.Error != nil {
			runErr = fmt.Errorf("%w: %s", ErrEngineExit, response.Error.Message)
		}
	case err := <-wait.Error:
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(wallContext.Err(), context.DeadlineExceeded) {
			runErr = ErrEngineTimeout
		} else {
			runErr = fmt.Errorf("wait for engine container: %w", err)
		}
	case <-wallContext.Done():
		if errors.Is(wallContext.Err(), context.DeadlineExceeded) {
			runErr = ErrEngineTimeout
		} else {
			runErr = wallContext.Err()
		}
	}
	cancelWall()
	if errors.Is(runErr, ErrEngineTimeout) {
		killContext, cancelKill := context.WithTimeout(context.Background(), s.operationTimeout)
		_, killErr := s.client.ContainerKill(killContext, created.ID, client.ContainerKillOptions{Signal: "SIGKILL"})
		cancelKill()
		if killErr != nil && !cerrdefs.IsNotFound(killErr) {
			runErr = errors.Join(runErr, fmt.Errorf("kill timed-out engine: %w", killErr))
		}
	}

	stdout := newBoundedBuffer(request.Limits.MaxStdoutBytes)
	stderr := newBoundedBuffer(request.Limits.MaxStderrBytes)
	logsContext, cancelLogs := context.WithTimeout(context.Background(), s.operationTimeout)
	logs, logsErr := s.client.ContainerLogs(logsContext, created.ID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if logsErr == nil {
		_, logsErr = stdcopy.StdCopy(stdout, stderr, logs)
		closeErr := logs.Close()
		logsErr = errors.Join(logsErr, closeErr)
	}
	cancelLogs()
	result.StdoutBytes = stdout.total
	result.StderrBytes = stderr.total
	result.Stderr = append([]byte(nil), stderr.Bytes()...)
	result.StderrTruncated = stderr.truncated
	if logsErr != nil {
		return result, errors.Join(runErr, fmt.Errorf("read engine logs: %w", logsErr))
	}
	if stdout.truncated {
		return result, errors.Join(runErr, ErrStdoutLimit)
	}
	if runErr != nil {
		return result, runErr
	}
	if result.ExitCode != 0 {
		return result, fmt.Errorf("%w: exit code %d", ErrEngineExit, result.ExitCode)
	}
	frames, err := parseEngineFrames(stdout.Bytes(), request)
	if err != nil {
		return result, err
	}
	result.Frames = frames
	return result, nil
}

func (s *Supervisor) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, s.operationTimeout)
}

func parseEngineFrames(output []byte, request EngineRequest) ([]EngineFrame, error) {
	if len(output) == 0 || output[len(output)-1] != '\n' {
		return nil, fmt.Errorf("%w: stdout must be non-empty newline-delimited JSON", ErrInvalidEngineOutput)
	}
	lines := bytes.Split(output[:len(output)-1], []byte{'\n'})
	frames := make([]EngineFrame, 0, len(lines))
	for index, line := range lines {
		if len(line) == 0 || int64(len(line)) > request.Limits.MaxFrameBytes {
			return nil, fmt.Errorf("%w: frame %d violates frame bound", ErrInvalidEngineOutput, index+1)
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var frame EngineFrame
		if err := decoder.Decode(&frame); err != nil || !decoderAtEOF(decoder) {
			return nil, fmt.Errorf("%w: frame %d is not canonical JSON", ErrInvalidEngineOutput, index+1)
		}
		if frame.Protocol != EngineProtocolV1 || frame.ID == "" || frame.Type == "" ||
			frame.RunID != request.RunID || frame.AttemptID != request.AttemptID ||
			frame.Sequence != uint64(index+1) || frame.Time.IsZero() || len(frame.Data) == 0 || !json.Valid(frame.Data) {
			return nil, fmt.Errorf("%w: frame %d violates engine protocol", ErrInvalidEngineOutput, index+1)
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	limit     int64
	total     int64
	truncated bool
}

func newBoundedBuffer(limit int64) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	buffer.total += int64(len(value))
	remaining := buffer.limit - int64(buffer.buffer.Len())
	if remaining > 0 {
		writeLength := int64(len(value))
		if writeLength > remaining {
			writeLength = remaining
		}
		_, _ = buffer.buffer.Write(value[:writeLength])
	}
	if buffer.total > buffer.limit {
		buffer.truncated = true
	}
	return len(value), nil
}

func (buffer *boundedBuffer) Bytes() []byte {
	return buffer.buffer.Bytes()
}

var _ io.Writer = (*boundedBuffer)(nil)
