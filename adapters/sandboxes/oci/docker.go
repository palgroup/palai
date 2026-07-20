package oci

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// operationTimeout bounds each individual daemon call (inspect, create, start, logs,
// kill, remove) so a wedged daemon cannot hang an attempt. It is independent of the
// engine wall-time, which bounds the container itself.
const operationTimeout = 30 * time.Second

// dockerDriver runs engine attempts against a local Docker daemon through the Moby
// Go client with API-version negotiation.
type dockerDriver struct {
	client *client.Client
}

// NewDockerDriver connects to the daemon described by the standard Docker environment
// and negotiates a supported API version.
func NewDockerDriver() (Driver, error) {
	apiClient, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return &dockerDriver{client: apiClient}, nil
}

func (d *dockerDriver) Close() error {
	if d == nil || d.client == nil {
		return nil
	}
	return d.client.Close()
}

// verifyImage resolves the pinned reference and confirms the daemon returns exactly the
// same immutable image, never a re-tagged one. Both the batch and interactive entry
// points gate on it, so a mutable reference cannot reach either.
func (d *dockerDriver) verifyImage(ctx context.Context, digest string) (string, error) {
	inspectCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	image, err := d.client.ImageInspect(inspectCtx, digest)
	if err != nil {
		return "", fmt.Errorf("inspect engine image: %w", err)
	}
	if image.ID != digest {
		return "", fmt.Errorf("%w: daemon resolved %s", ErrMutableImage, image.ID)
	}
	return image.ID, nil
}

// createOptions is the single hardened container spec both entry points create from:
// unprivileged user, no network, read-only rootfs, all capabilities dropped,
// no-new-privileges, and the exact env/labels/limits. interactive additionally opens
// stdin so the streaming supervisor can inject controller frames mid-run; the isolation
// is otherwise identical, so the batch hardening cannot drift from the streaming one.
func createOptions(spec ContainerSpec, interactive bool) client.ContainerCreateOptions {
	processLimit := spec.Limits.MaxProcessCount
	hostConfig := &container.HostConfig{
		NetworkMode:    container.NetworkMode("none"),
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		Mounts:         workspaceMounts(spec.Mounts),
		Resources: container.Resources{
			Memory:     spec.Limits.MaxMemoryBytes,
			MemorySwap: spec.Limits.MaxMemoryBytes,
			NanoCPUs:   spec.Limits.NanoCPUs,
			PidsLimit:  &processLimit,
		},
	}
	// Bound the writable layer when a disk limit is set and the daemon storage driver supports a
	// size quota (spec §28.8). Best-effort: a daemon that rejects the option surfaces as a create
	// error the caller classifies, never a silently unbounded sandbox.
	if spec.Limits.MaxDiskBytes > 0 {
		hostConfig.StorageOpt = map[string]string{"size": strconv.FormatInt(spec.Limits.MaxDiskBytes, 10)}
	}
	return client.ContainerCreateOptions{
		Image: spec.ImageDigest,
		Config: &container.Config{
			User:            "65532:65532",
			AttachStdin:     interactive,
			OpenStdin:       interactive,
			StdinOnce:       false,
			AttachStdout:    true,
			AttachStderr:    true,
			NetworkDisabled: true,
			Env:             spec.Env,
			Labels:          spec.Labels,
			Cmd:             spec.Cmd,
			WorkingDir:      spec.WorkingDir,
		},
		HostConfig: hostConfig,
	}
}

// workspaceMounts translates the driver's opaque host binds into Moby bind mounts. Only bind
// mounts are ever attached (no volumes, no host sockets); the read-only rootfs and dropped
// capabilities are unaffected, so a writable /workspace does not widen the sandbox (spec §29.9).
func workspaceMounts(specs []Mount) []mount.Mount {
	if len(specs) == 0 {
		return nil
	}
	mounts := make([]mount.Mount, 0, len(specs))
	for _, m := range specs {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	return mounts
}

// Run creates, starts, waits on, and destroys one hardened engine container. It always
// force-removes the container before returning, so no allocation survives the call.
func (d *dockerDriver) Run(ctx context.Context, spec ContainerSpec) (result Outcome, runErr error) {
	if err := spec.validate(); err != nil {
		return Outcome{}, err
	}

	imageID, err := d.verifyImage(ctx, spec.ImageDigest)
	if err != nil {
		return Outcome{}, err
	}
	result.ImageID = imageID

	createCtx, cancelCreate := context.WithTimeout(ctx, operationTimeout)
	created, err := d.client.ContainerCreate(createCtx, createOptions(spec, false))
	cancelCreate()
	if err != nil {
		return result, fmt.Errorf("create engine container: %w", err)
	}
	result.ContainerID = created.ID
	defer func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), operationTimeout)
		defer cancelCleanup()
		_, cleanupErr := d.client.ContainerRemove(cleanupCtx, created.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		if cleanupErr != nil && !cerrdefs.IsNotFound(cleanupErr) {
			runErr = errors.Join(runErr, fmt.Errorf("remove engine container: %w", cleanupErr))
		}
	}()

	startCtx, cancelStart := context.WithTimeout(ctx, operationTimeout)
	_, err = d.client.ContainerStart(startCtx, created.ID, client.ContainerStartOptions{})
	cancelStart()
	if err != nil {
		return result, fmt.Errorf("start engine container: %w", err)
	}

	wallCtx, cancelWall := context.WithTimeout(ctx, spec.Limits.WallTime)
	wait := d.client.ContainerWait(wallCtx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case response := <-wait.Result:
		result.ExitCode = response.StatusCode
	case err := <-wait.Error:
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(wallCtx.Err(), context.DeadlineExceeded) {
			result.TimedOut = true
		} else {
			cancelWall()
			return result, fmt.Errorf("wait for engine container: %w", err)
		}
	case <-wallCtx.Done():
		if errors.Is(wallCtx.Err(), context.DeadlineExceeded) {
			result.TimedOut = true
		} else {
			cancelWall()
			return result, wallCtx.Err()
		}
	}
	cancelWall()

	if result.TimedOut {
		killCtx, cancelKill := context.WithTimeout(context.Background(), operationTimeout)
		_, killErr := d.client.ContainerKill(killCtx, created.ID, client.ContainerKillOptions{Signal: "SIGKILL"})
		cancelKill()
		if killErr != nil && !cerrdefs.IsNotFound(killErr) {
			runErr = errors.Join(runErr, fmt.Errorf("kill timed-out engine: %w", killErr))
		}
	}

	// Read logs on a background-derived context so output is still captured when the
	// caller's context is already cancelled (for example on a timeout kill).
	stdout := newBoundedBuffer(spec.MaxStdoutBytes)
	stderr := newBoundedBuffer(spec.MaxStderrBytes)
	logsCtx, cancelLogs := context.WithTimeout(context.Background(), operationTimeout)
	logs, logsErr := d.client.ContainerLogs(logsCtx, created.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if logsErr == nil {
		_, logsErr = stdcopy.StdCopy(stdout, stderr, logs)
		logsErr = errors.Join(logsErr, logs.Close())
	}
	cancelLogs()
	result.Stdout = stdout.data
	result.StdoutBytes = stdout.total
	result.StdoutTruncated = stdout.truncated
	result.Stderr = stderr.data
	result.StderrBytes = stderr.total
	result.StderrTruncated = stderr.truncated
	if logsErr != nil {
		return result, fmt.Errorf("read engine logs: %w", logsErr)
	}
	return result, nil
}
