package oci

import (
	"context"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// This file is the DETACHED entry point: a container that outlives the call that started it. It is the
// third way into the same hardening (Run is batch, Start is interactive, StartDetached is this), and the
// rule all three obey is the one stated on createOptions — the isolation cannot drift between them,
// because none of them builds its own options.
//
// WHAT MAKES IT A SEPARATE METHOD RATHER THAN A FLAG ON Run: Run's `defer` force-removes its container
// unconditionally, on the error path too, and that defer is the whole reason a background task was
// impossible. A flag would have made the removal conditional and put the two lifetimes in one function,
// where a future edit gets them the wrong way round exactly once. dockerDriver.Run does not change by
// one byte and a guard in this package's tests keeps it that way.
//
// SURVIVING A CONTROL-PLANE RESTART IS FREE HERE and it is worth naming why: the container is the
// DAEMON'S object, not ours. Nothing in this process holds it, so nothing in this process ending can
// release it. The host posture had to earn the same property with a recorded start time.

// DetachedDriver starts a hardened container and LEAVES IT RUNNING, then answers questions about it from
// the daemon. It is a separate interface from Driver for the InteractiveDriver reason: a driver that can
// only do batch attempts should not have to stub two methods that lie about what it left behind.
type DetachedDriver interface {
	StartDetached(ctx context.Context, spec ContainerSpec) (string, error)
	InspectDetached(ctx context.Context, containerID string) (DetachedStatus, error)
	KillDetached(ctx context.Context, containerID string) error
}

// DetachedStatus is what the daemon says about one detached container.
//
// Found IS SEPARATE FROM Running because "removed" and "stopped" are different facts with different
// consequences: a stopped container still holds its exit status, and a removed one holds nothing at all.
// ExitCode is a pointer for the same reason it is nullable in the row — a container nobody can find has
// no exit code, and inventing a zero would report a clean exit for a build that may have failed.
//
// Labels is carried because it is the OWNERSHIP EVIDENCE. A container id is not reused the way a pid is,
// but the rule "we do not signal what we cannot prove is ours" is a rule rather than a mitigation, so it
// is enforced on both postures with whatever evidence each one has.
type DetachedStatus struct {
	Found    bool
	Running  bool
	Labels   map[string]string
	ExitCode *int
}

// NewDockerDetachedDriver connects to the daemon described by the standard Docker environment, sharing
// the batch driver's client construction and hardening.
func NewDockerDetachedDriver() (DetachedDriver, error) {
	apiClient, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return &dockerDriver{client: apiClient}, nil
}

// StartDetached creates and starts one hardened container from the SAME createOptions the batch and
// interactive paths create from, and returns without removing it. The container id is the handle.
//
// THE ONLY DIFFERENCES FROM Run ARE THE ABSENT `defer` REMOVE AND THE CALLER'S LABEL. Not the uid, not
// the network mode, not the read-only rootfs, not the dropped capabilities, not no-new-privileges, not
// the cgroup bounds — those come from createOptions(spec, false), which this function calls and does not
// reimplement, and a go/ast guard in this package asserts that it calls exactly that.
//
// spec.validate() runs first, unchanged, so a spec that would be REFUSED for an attached call is refused
// here too. That includes the positive WallTime bound, which this path does not itself enforce: the
// deadline of a background task is a column a reaper reads (E26 §0.2), because a context does not
// survive a restart. Sharing the validation rather than relaxing it means the detached path can never
// become the loophole through which an unbounded or unpinned spec reaches the daemon.
func (d *dockerDriver) StartDetached(ctx context.Context, spec ContainerSpec) (string, error) {
	if err := spec.validate(); err != nil {
		return "", err
	}
	if _, err := d.verifyImage(ctx, spec.ImageDigest); err != nil {
		return "", err
	}

	createCtx, cancelCreate := context.WithTimeout(ctx, operationTimeout)
	created, err := d.client.ContainerCreate(createCtx, createOptions(spec, false))
	cancelCreate()
	if err != nil {
		return "", fmt.Errorf("create detached container: %w", err)
	}

	startCtx, cancelStart := context.WithTimeout(ctx, operationTimeout)
	_, err = d.client.ContainerStart(startCtx, created.ID, client.ContainerStartOptions{})
	cancelStart()
	if err != nil {
		// A container that was CREATED and never STARTED is not a background task, it is litter — and the
		// caller is about to be told the start failed, so nothing will ever come looking for it. This is the
		// one cleanup on this path, and it is an explicit error-path call rather than a defer, so no future
		// edit can widen it into the unconditional removal that made this method necessary.
		d.forceRemove(created.ID)
		return "", fmt.Errorf("start detached container: %w", err)
	}
	return created.ID, nil
}

// InspectDetached asks the daemon about one container. A container the daemon does not know is reported
// Found=false rather than as an error: it is the normal terminal answer for a task that was killed and
// removed, and a caller that had to distinguish "gone" from "daemon is broken" by string-matching an
// error would get it wrong.
func (d *dockerDriver) InspectDetached(ctx context.Context, containerID string) (DetachedStatus, error) {
	inspectCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	result, err := d.client.ContainerInspect(inspectCtx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return DetachedStatus{}, nil
		}
		return DetachedStatus{}, fmt.Errorf("inspect detached container: %w", err)
	}
	status := DetachedStatus{Found: true}
	if result.Container.Config != nil {
		status.Labels = result.Container.Config.Labels
	}
	if result.Container.State != nil {
		status.Running = result.Container.State.Running
		if !status.Running {
			// The daemon KEEPS the exit status of a stopped container, which is what makes the container
			// posture's exit code durable across a control-plane restart — the half the host posture cannot
			// have, because there a watcher goroutine is the only thing that ever saw it.
			code := result.Container.State.ExitCode
			status.ExitCode = &code
		}
	}
	return status, nil
}

// KillDetached force-terminates and removes one container, idempotently. Removal is not optional: a
// stopped-but-kept container holds its writable layer, and a posture whose whole point is that nothing
// is waiting around would accumulate them silently.
//
// Docker's own default logging driver caches container logs as JSON with NO ROTATION, which is a second
// reason this path never becomes a log source: a background task writes its own bounded file inside the
// allocation, and `docker logs` is deliberately not a second read path — two read paths would mean two
// redaction paths, and only one of them would be maintained.
func (d *dockerDriver) KillDetached(ctx context.Context, containerID string) error {
	killCtx, cancelKill := context.WithTimeout(ctx, operationTimeout)
	_, err := d.client.ContainerKill(killCtx, containerID, client.ContainerKillOptions{Signal: "SIGKILL"})
	cancelKill()
	// Already stopped or already gone are both "the task is not running", which is what the caller asked
	// for. Only a real daemon failure is worth reporting.
	if err != nil && !cerrdefs.IsNotFound(err) && !cerrdefs.IsConflict(err) {
		return fmt.Errorf("kill detached container: %w", err)
	}

	removeCtx, cancelRemove := context.WithTimeout(ctx, operationTimeout)
	_, err = d.client.ContainerRemove(removeCtx, containerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	cancelRemove()
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove detached container: %w", err)
	}
	return nil
}
