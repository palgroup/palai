//go:build component

// Package background holds the E26 T1 container-posture proofs, and every one of them measures a REAL
// Docker daemon rather than our own bookkeeping. It runs only under `make test-component TEST=background`,
// which pins the busybox shell image the OCI shell tool runs argv in (the same digest scripts/test/
// live-provider and scripts/uat/coding pin) and counts sandbox containers before and after.
//
// WHY THE MEASUREMENT HAS TO COME FROM ContainerInspect: the claim of this task is that a process now
// outlives the call that started it. A test that asked our own code whether the container is running
// would pass over an implementation that only believes it is. So liveness here is what the daemon says,
// and the leak accounting in the suite script is what proves the tests clean up after themselves.
package background

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// shellImage is the pinned busybox the sandbox runs. A background task needs a real /bin/sh — the
// fixture engine used by the runner tiers is FROM scratch and has none — which is why this suite pins
// the same image the coding tiers do rather than reusing that harness.
func shellImage(t *testing.T) string {
	t.Helper()
	image := os.Getenv("PALAI_SHELL_IMAGE_ID")
	if image == "" {
		t.Skip("PALAI_SHELL_IMAGE_ID is not set; run via `make test-component TEST=background`")
	}
	return image
}

func daemon(t *testing.T) *client.Client {
	t.Helper()
	c, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("connect to the Docker daemon: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// limits are the bounds every container here runs under. They are POSITIVE for the same reason the
// synchronous path's are: ContainerSpec.validate refuses a non-positive bound, and the detached path
// shares that validation deliberately — a spec that would be refused for an attached call must not be
// accepted for a detached one.
var limits = oci.Limits{WallTime: 30 * time.Second, MaxMemoryBytes: 256 << 20, MaxProcessCount: 64, NanoCPUs: 1_000_000_000}

// allocation is one throwaway workspace under /tmp, opened to the sandbox uid the way the E09 T4
// isolation fixture opens its own. /tmp rather than t.TempDir() because Docker Desktop shares it.
func allocation(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "palai-bgt-")
	if err != nil {
		t.Fatalf("create allocation dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve allocation dir: %v", err)
	}
	if err := workspace.Prepare(resolved); err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	if err := os.Chmod(resolved, 0o777); err != nil {
		t.Fatalf("open allocation to the sandbox uid: %v", err)
	}
	return resolved
}

func newSpec(t *testing.T) toolbroker.BackgroundSpec {
	t.Helper()
	id := "bgt-" + strings.ReplaceAll(t.Name(), "/", "-")
	return toolbroker.BackgroundSpec{TaskID: id, OutputPath: ".palai-session/bg/" + id + ".log"}
}

// TestTodayAShellContainerIsGoneWhenTheCallReturns is the container half of the measurement this task
// rests on, and like its host sibling it tests the code as it stands. dockerDriver.Run force-removes its
// container on a `defer` — unconditionally, on the error path too — so container ownership is bound to
// the Run CALL. That is why "park the run and keep the build going" was a contradiction here as well,
// for a completely different reason than on the host.
//
// It stays in the suite as a guard: the synchronous posture must keep leaving nothing behind.
func TestTodayAShellContainerIsGoneWhenTheCallReturns(t *testing.T) {
	image := shellImage(t)
	cli := daemon(t)
	driver, err := oci.NewDockerDriver()
	if err != nil {
		t.Fatalf("bind docker driver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	root := allocation(t)
	marker := "io.palai.bgt-today=" + t.Name()
	spec := oci.ContainerSpec{
		ImageDigest:    image,
		Labels:         map[string]string{"io.palai.sandbox": "shell", "io.palai.bgt-today": t.Name()},
		Limits:         limits,
		MaxStdoutBytes: 1 << 20,
		MaxStderrBytes: 1 << 16,
		Cmd:            []string{"/bin/sh", "-c", "sleep 2"},
		WorkingDir:     "/workspace",
		Mounts:         []oci.Mount{{Source: root, Target: "/workspace"}},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = driver.Run(context.Background(), spec)
	}()

	// It really ran: the daemon lists a container with our marker while Run is still executing.
	if !waitForContainer(t, cli, marker, true) {
		t.Fatal("the synchronous run never produced a container; the fixture proves nothing")
	}
	<-done
	// And it is gone the moment Run returned — not stopped, REMOVED.
	if ids := listByLabel(t, cli, marker); len(ids) != 0 {
		t.Fatalf("the synchronous run left %d container(s) behind; ownership is supposed to end with the call", len(ids))
	}
}

// TestADetachedContainerIsStillRunningAfterTheCallReturns is E26's first RED. The measurement is
// ContainerInspect — the daemon's own answer — taken AFTER Start has returned.
func TestADetachedContainerIsStillRunningAfterTheCallReturns(t *testing.T) {
	image := shellImage(t)
	cli := daemon(t)
	e := newExecutor(t, image)
	root := allocation(t)
	spec := newSpec(t)

	// The context that carries the attempt is cancelled immediately after Start returns. Under the
	// synchronous posture that is the end of the container; here it must mean nothing at all.
	ctx, cancel := context.WithCancel(context.Background())
	handle, err := e.Start(ctx, toolbroker.ShellCommand{
		Argv:          []string{"sleep", "30"},
		WorkspaceRoot: root,
	}, spec)
	if err != nil {
		t.Fatalf("start detached: %v", err)
	}
	t.Cleanup(func() { _ = e.Kill(context.Background(), handle) })
	cancel()

	if handle.Posture != toolbroker.PostureSandboxedLinux {
		t.Fatalf("handle posture = %q, want %q", handle.Posture, toolbroker.PostureSandboxedLinux)
	}

	time.Sleep(500 * time.Millisecond)
	inspected, err := cli.ContainerInspect(context.Background(), handle.Value, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect %s: %v (the container was removed with the call)", handle.Value, err)
	}
	if !inspected.Container.State.Running {
		t.Fatalf("container %s state = %q, want running after Start returned", handle.Value, inspected.Container.State.Status)
	}
	// The label an operator hunts orphans by, and the one Kill proves ownership through.
	if got := inspected.Container.Config.Labels["io.palai.bg"]; got != spec.TaskID {
		t.Fatalf("io.palai.bg = %q, want %q", got, spec.TaskID)
	}

	// And our own probe agrees with the daemon, which is the only reason it is allowed to exist.
	status, err := e.Probe(context.Background(), handle)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if status.State != toolbroker.BackgroundRunning {
		t.Fatalf("probe state = %q, want running", status.State)
	}
}

// TestTheDetachedCommandFormInterpretsNoMetacharacter pins §3.2's redirect form. The container writes its
// own log, which means a shell is involved, which means the argv could have been re-parsed. It is not:
// `sh -c 'exec "$@" >"$0" 2>&1' <logpath> <argv...>` passes the command as POSITIONAL PARAMETERS, so the
// shell expands the redirect target and nothing else.
//
// The command below is the test: a semicolon, a redirect, a command substitution and an && chained into
// ONE argument. If any of them were interpreted, the marker file would exist and the log would not carry
// the literal text.
func TestTheDetachedCommandFormInterpretsNoMetacharacter(t *testing.T) {
	image := shellImage(t)
	e := newExecutor(t, image)
	root := allocation(t)
	spec := newSpec(t)

	hostile := "alpha; touch /workspace/pwned > /workspace/redirected && echo $(id -u)"
	handle, err := e.Start(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"echo", hostile},
		WorkspaceRoot: root,
	}, spec)
	if err != nil {
		t.Fatalf("start detached: %v", err)
	}
	t.Cleanup(func() { _ = e.Kill(context.Background(), handle) })

	body := waitForLog(t, filepath.Join(root, spec.OutputPath), "alpha")
	if !strings.Contains(body, hostile) {
		t.Fatalf("log = %q, want the argument verbatim: a metacharacter was interpreted", body)
	}
	for _, evidence := range []string{"pwned", "redirected"} {
		if _, err := os.Stat(filepath.Join(root, evidence)); err == nil {
			t.Fatalf("%s exists: the shell interpreted the argument instead of passing it positionally", evidence)
		}
	}
}

// TestADetachedTaskReportsItsExitCodeFromTheDaemon is the container posture's structural advantage over
// the host's, and it is worth pinning because it is the half that survives a restart: the daemon keeps
// the exit status of a stopped container, so the exit code is durable state rather than something a
// watcher goroutine happened to see.
func TestADetachedTaskReportsItsExitCodeFromTheDaemon(t *testing.T) {
	image := shellImage(t)
	e := newExecutor(t, image)
	root := allocation(t)
	spec := newSpec(t)

	handle, err := e.Start(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"/bin/sh", "-c", "exit 7"},
		WorkspaceRoot: root,
	}, spec)
	if err != nil {
		t.Fatalf("start detached: %v", err)
	}
	t.Cleanup(func() { _ = e.Kill(context.Background(), handle) })

	deadline := time.Now().Add(30 * time.Second)
	var status toolbroker.BackgroundStatus
	for time.Now().Before(deadline) {
		status, err = e.Probe(context.Background(), handle)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if status.State == toolbroker.BackgroundExited {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status.State != toolbroker.BackgroundExited {
		t.Fatalf("state = %q, want exited", status.State)
	}
	if status.ExitCode == nil || *status.ExitCode != 7 {
		t.Fatalf("exit code = %v, want 7 from the daemon", status.ExitCode)
	}
}

// TestKillingADetachedTaskRemovesTheContainerAndIsIdempotent measures the teardown from the daemon: the
// container is not merely stopped, it is gone — otherwise every background task would leave a stopped
// container and a writable layer behind, and nothing would ever notice.
func TestKillingADetachedTaskRemovesTheContainerAndIsIdempotent(t *testing.T) {
	image := shellImage(t)
	cli := daemon(t)
	e := newExecutor(t, image)
	root := allocation(t)
	spec := newSpec(t)

	handle, err := e.Start(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"sleep", "60"},
		WorkspaceRoot: root,
	}, spec)
	if err != nil {
		t.Fatalf("start detached: %v", err)
	}

	if err := e.Kill(context.Background(), handle); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if _, err := cli.ContainerInspect(context.Background(), handle.Value, client.ContainerInspectOptions{}); err == nil {
		t.Fatalf("container %s still exists after the kill", handle.Value)
	}
	// "Cancelling twice is idempotent" — the vendor contract for the same operation, and what a reaper
	// racing a notification needs.
	if err := e.Kill(context.Background(), handle); err != nil {
		t.Fatalf("second kill: %v (killing twice must be killing once)", err)
	}
	status, err := e.Probe(context.Background(), handle)
	if err != nil {
		t.Fatalf("probe after kill: %v", err)
	}
	if status.State != toolbroker.BackgroundExited {
		t.Fatalf("state after kill = %q, want exited", status.State)
	}
	if status.ExitCode != nil {
		t.Fatalf("exit code after removal = %d, want nil: a removed container's status is unknowable, not zero", *status.ExitCode)
	}
}

// TestADetachedContainerWeCannotProveIsOursIsNeverSignalled is the container posture's half of the same
// rule the host posture proves with a start time. A container id is not reused the way a pid is, so the
// evidence here is the io.palai.bg LABEL: a container that carries no such label is not ours, and the
// rule is identical — no signal, no removal, a refusal the caller can read.
func TestADetachedContainerWeCannotProveIsOursIsNeverSignalled(t *testing.T) {
	image := shellImage(t)
	cli := daemon(t)
	e := newExecutor(t, image)
	root := allocation(t)

	// A REAL container started outside this program's background path, standing in for anything else on
	// the daemon. It carries the sandbox label so the suite's leak accounting still sees it, and no
	// io.palai.bg label at all.
	created, err := cli.ContainerCreate(context.Background(), client.ContainerCreateOptions{
		Image: image,
		Config: &container.Config{
			Cmd:    []string{"sleep", "60"},
			Labels: map[string]string{"io.palai.sandbox": "shell"},
		},
	})
	if err != nil {
		t.Fatalf("create the innocent container: %v", err)
	}
	t.Cleanup(func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := cli.ContainerStart(context.Background(), created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start the innocent container: %v", err)
	}

	handle := toolbroker.Handle{Posture: toolbroker.PostureSandboxedLinux, Value: created.ID}
	status, err := e.Probe(context.Background(), handle)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if status.State != toolbroker.BackgroundLost {
		t.Fatalf("probe state = %q, want lost: the container carries no io.palai.bg label", status.State)
	}
	if err := e.Kill(context.Background(), handle); err == nil {
		t.Fatal("Kill accepted a container it cannot prove it owns")
	}

	// THE ASSERTION THAT MATTERS: it is still there and still running.
	inspected, err := cli.ContainerInspect(context.Background(), created.ID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("the innocent container was removed: %v", err)
	}
	if !inspected.Container.State.Running {
		t.Fatalf("the innocent container was stopped (state %q); a handle we cannot prove is ours must never be signalled", inspected.Container.State.Status)
	}
	_ = root
}

func newExecutor(t *testing.T, image string) *workspace.ShellExecutor {
	t.Helper()
	driver, err := oci.NewDockerDriver()
	if err != nil {
		t.Fatalf("bind docker driver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	return workspace.NewShellExecutor(driver, image, limits)
}

func listByLabel(t *testing.T, cli *client.Client, label string) []string {
	t.Helper()
	result, err := cli.ContainerList(context.Background(), client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("label", label),
	})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	ids := make([]string, 0, len(result.Items))
	for _, c := range result.Items {
		ids = append(ids, c.ID)
	}
	return ids
}

func waitForContainer(t *testing.T, cli *client.Client, label string, want bool) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if (len(listByLabel(t, cli, label)) > 0) == want {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func waitForLog(t *testing.T, path, marker string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), marker) {
			return string(data)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never contained %q", path, marker)
	return ""
}
