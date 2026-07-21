package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Process is one live, interactive engine container: its stdio streams are open for the
// duration of the run rather than captured post-hoc. Stdin carries controller frames
// injected mid-run; Stdout is read line by line and its close marks the engine exit;
// Stderr is a separate stream. Wait blocks until the container exits or the wall-time
// bound force-kills it, and always destroys the container; Kill force-terminates it. A
// batch attempt uses Driver.Run instead — this seam is the streaming counterpart.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	Wait(ctx context.Context) (Outcome, error)
	Kill(ctx context.Context) error
}

// InteractiveDriver starts a hardened engine container and returns it live, with the
// same digest verification, environment allowlist, and resource limits as Driver.Run.
// The runner's streaming supervisor drives the returned Process; the future runner
// gateway (Task 11c) wires this to production.
type InteractiveDriver interface {
	Start(ctx context.Context, spec ContainerSpec) (Process, error)
}

// NewDockerInteractiveDriver connects to the daemon described by the standard Docker
// environment, sharing the batch driver's client construction and hardening.
func NewDockerInteractiveDriver() (InteractiveDriver, error) {
	apiClient, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return &dockerDriver{client: apiClient}, nil
}

// Start creates a hardened, stdin-open container, attaches to its multiplexed stdio,
// starts it, and returns a live Process. The container is created from the same
// createOptions as the batch path (only stdin is additionally opened), so the
// isolation is identical. On any setup failure the container is force-removed.
func (d *dockerDriver) Start(ctx context.Context, spec ContainerSpec) (Process, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	imageID, err := d.verifyImage(ctx, spec.ImageDigest)
	if err != nil {
		return nil, err
	}

	createCtx, cancelCreate := context.WithTimeout(ctx, operationTimeout)
	created, err := d.client.ContainerCreate(createCtx, createOptions(spec, true))
	cancelCreate()
	if err != nil {
		return nil, fmt.Errorf("create engine container: %w", err)
	}

	// Attach before start so no early frame is missed. The hijacked connection stays
	// open for the whole run, so it is bound to ctx, not a short operation timeout.
	attach, err := d.client.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{
		Stream: true, Stdin: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		d.forceRemove(created.ID)
		return nil, fmt.Errorf("attach engine container: %w", err)
	}

	startCtx, cancelStart := context.WithTimeout(ctx, operationTimeout)
	_, err = d.client.ContainerStart(startCtx, created.ID, client.ContainerStartOptions{})
	cancelStart()
	if err != nil {
		attach.Close()
		d.forceRemove(created.ID)
		return nil, fmt.Errorf("start engine container: %w", err)
	}

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	proc := &dockerProcess{
		client:      d.client,
		containerID: created.ID,
		imageID:     imageID,
		wallTime:    spec.Limits.WallTime,
		attach:      attach,
		stdoutR:     stdoutR,
		stderrR:     stderrR,
		demuxDone:   make(chan struct{}),
	}
	// Demultiplex the container's stdcopy-framed stdio into the two pipes. When the
	// stream ends (engine exit or kill), both pipes close and the supervisor's readers
	// see EOF. demuxDone closes once StdCopy has drained the attach to EOF, so a clean
	// exit's destroy() can wait for the tail to be fully copied before tearing down.
	go func() {
		defer close(proc.demuxDone)
		_, copyErr := stdcopy.StdCopy(stdoutW, stderrW, attach.Reader)
		_ = stdoutW.CloseWithError(copyErr)
		_ = stderrW.CloseWithError(copyErr)
	}()
	return proc, nil
}

// forceRemove destroys a container on a background-derived context, ignoring an
// already-absent container. It is the setup-failure and idempotent-teardown cleanup.
func (d *dockerDriver) forceRemove(containerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	if _, err := d.client.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
		// Best-effort: a leaked container is caught by the suite's leak check, not here.
		_ = err
	}
}

// dockerProcess is a live interactive engine container over a hijacked stdio connection.
type dockerProcess struct {
	client      *client.Client
	containerID string
	imageID     string
	wallTime    time.Duration
	attach      client.ContainerAttachResult
	stdoutR     *io.PipeReader
	stderrR     *io.PipeReader
	demuxDone   chan struct{}
	destroyOnce sync.Once
}

// demuxDrainTimeout bounds how long a clean-exit destroy() waits for the stdcopy demux to
// reach EOF before tearing down the attach. It is the recoverable ceiling on the tail-frame
// drain: a wedged daemon that never EOFs the stream must not hang the supervisor's teardown.
const demuxDrainTimeout = 5 * time.Second

func (p *dockerProcess) Stdin() io.WriteCloser { return &stdinConn{attach: &p.attach} }
func (p *dockerProcess) Stdout() io.Reader     { return p.stdoutR }
func (p *dockerProcess) Stderr() io.Reader     { return p.stderrR }

// Wait blocks for the container to exit within the wall-time bound, force-killing it on
// expiry (TimedOut), and always destroys the container before returning. It mirrors the
// batch driver's wall-time enforcement so a hung streaming engine is classified terminal
// exactly as a hung batch engine is.
func (p *dockerProcess) Wait(ctx context.Context) (Outcome, error) {
	out := Outcome{ContainerID: p.containerID, ImageID: p.imageID}
	wallCtx, cancelWall := context.WithTimeout(ctx, p.wallTime)
	wait := p.client.ContainerWait(wallCtx, p.containerID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case response := <-wait.Result:
		out.ExitCode = response.StatusCode
	case err := <-wait.Error:
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(wallCtx.Err(), context.DeadlineExceeded) {
			out.TimedOut = true
		} else {
			cancelWall()
			p.destroy(false)
			return out, fmt.Errorf("wait for engine container: %w", err)
		}
	case <-wallCtx.Done():
		if errors.Is(wallCtx.Err(), context.DeadlineExceeded) {
			out.TimedOut = true
		} else {
			cancelWall()
			p.destroy(false)
			return out, wallCtx.Err()
		}
	}
	cancelWall()

	if out.TimedOut {
		killCtx, cancelKill := context.WithTimeout(context.Background(), operationTimeout)
		_, killErr := p.client.ContainerKill(killCtx, p.containerID, client.ContainerKillOptions{Signal: "SIGKILL"})
		cancelKill()
		if killErr != nil && !cerrdefs.IsNotFound(killErr) {
			p.destroy(false)
			return out, fmt.Errorf("kill timed-out engine: %w", killErr)
		}
		// A force-killed engine has no clean tail to preserve; reclaim immediately.
		p.destroy(false)
		return out, nil
	}
	// Clean exit: drain the demux to EOF so the engine's final run.terminal is copied out of
	// the attach buffer before the stream is severed (REC-001).
	p.destroy(true)
	return out, nil
}

// Kill force-terminates and destroys the container. It is idempotent with Wait: the
// container is removed exactly once, so the supervisor can always defer a Kill.
func (p *dockerProcess) Kill(ctx context.Context) error {
	killCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	_, err := p.client.ContainerKill(killCtx, p.containerID, client.ContainerKillOptions{Signal: "SIGKILL"})
	cancel()
	p.destroy(false)
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("kill engine container: %w", err)
	}
	return nil
}

// destroy force-removes the container and closes the hijacked connection exactly once. When
// drain is set (a clean container exit), it first waits for the stdcopy demux to reach EOF —
// bounded by demuxDrainTimeout — so a fast-exit engine's buffered run.terminal is fully
// copied into the stdout pipe before the attach is severed (REC-001). A force kill or a
// wall-time/error teardown passes drain=false: a forcibly-killed engine has no clean tail to
// preserve, and waiting would only delay the reclaim.
func (p *dockerProcess) destroy(drain bool) {
	p.destroyOnce.Do(func() {
		reap := func() {
			ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
			defer cancel()
			_, _ = p.client.ContainerRemove(ctx, p.containerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
			p.attach.Close()
		}
		if drain {
			drainThenReap(p.demuxDone, demuxDrainTimeout, reap)
			return
		}
		reap()
	})
}

// drainThenReap waits for the demux goroutine to reach EOF (demuxDone closed) before invoking
// reap, so the container's buffered tail is fully demultiplexed into the stdout pipe before
// the attach connection is torn down. It waits at most timeout: a wedged demux must not hang
// teardown. Pure and side-effect-only through reap, so the ordering is unit-testable without a
// Docker daemon.
func drainThenReap(demuxDone <-chan struct{}, timeout time.Duration, reap func()) {
	select {
	case <-demuxDone:
	case <-time.After(timeout):
	}
	reap()
}

// stdinConn writes controller frames to the container's stdin and closes only the write
// half of the hijacked connection, signalling stdin EOF to the engine without tearing
// down the still-draining stdout/stderr streams.
type stdinConn struct{ attach *client.ContainerAttachResult }

func (c *stdinConn) Write(p []byte) (int, error) { return c.attach.Conn.Write(p) }
func (c *stdinConn) Close() error {
	if err := c.attach.CloseWrite(); err != nil {
		return err
	}
	return nil
}

// interface assertions.
var (
	_ InteractiveDriver = (*dockerDriver)(nil)
	_ Process           = (*dockerProcess)(nil)
)
