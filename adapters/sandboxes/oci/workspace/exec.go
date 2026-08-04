package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// ErrPathEscape is returned when a requested path resolves outside the workspace root — a traversal,
// an absolute path, or a symlink whose target escapes (spec §28.7, SAN-001). Its message never
// includes the host path, only the workspace-relative request.
var ErrPathEscape = errors.New("path escapes the workspace")

// ErrNotRegular is returned when a path resolves to a device, socket, or fifo rather than a regular
// file or directory (spec §28.7, SAN-001). A workspace tool operates on files, never on the kernel
// interfaces a hostile checkout might plant.
var ErrNotRegular = errors.New("path is not a regular file")

// WorkspaceFS confines every file operation to one allocation root (spec §28.7, SAN-001). Paths are
// resolved relative to the root; an absolute path, a `..` traversal, or a symlink whose target
// escapes the root is denied, as is a non-regular target. It owns containment only — read-only
// policy and likely-secret redaction are layered by the file tool above it.
type WorkspaceFS struct {
	root string // absolute, symlink-resolved
}

// NewWorkspaceFS binds a confinement to a real allocation root, resolving it through any symlinks so
// containment checks compare real paths. The root must exist.
func NewWorkspaceFS(root string) (*WorkspaceFS, error) {
	// EVERY REFUSAL HERE WRAPS ErrRootUnusable, and the wrap is what lets the file tool's one
	// asymmetry survive a workspace that lives on another machine: an absent root's own cause chain
	// satisfies fs.ErrNotExist, so without a sentinel of its own a never-provisioned allocation would
	// cross the wire as a missing FILE and be answered instead of faulted (failure.go).
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: no root given", ErrRootUnusable)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve workspace root: %v", ErrRootUnusable, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve workspace root: %v", ErrRootUnusable, err)
	}
	return &WorkspaceFS{root: real}, nil
}

// WriteReport is the before/after summary of one write (spec §28.7): the workspace-relative path,
// the content hash before and after (before is empty for a new file), and whether the file was
// created. It is the changeset-event source the accrual layer consumes (T5).
type WriteReport struct {
	Path       string
	BeforeHash string
	AfterHash  string
	Created    bool
}

// Stat is the confined metadata of a path.
type Stat struct {
	Path  string
	IsDir bool
	Size  int64
}

// DirEntry is one entry of a confined directory listing.
type DirEntry struct {
	Name  string
	IsDir bool
	Size  int64
}

// resolve maps a workspace-relative request to an absolute host path, denying every escape: an
// absolute input, a `..` traversal past the root, or a symlink whose target leaves the root (spec
// §28.7). The returned path is safe to open; the target itself may not exist yet (a new write).
func (w *WorkspaceFS) resolve(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q is absolute", ErrPathEscape, rel)
	}
	abs := filepath.Join(w.root, rel)
	within, err := filepath.Rel(w.root, abs)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, rel)
	}
	if err := w.assertNoSymlinkEscape(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// assertNoSymlinkEscape resolves the deepest existing ancestor of abs through symlinks and verifies
// it still sits under the real root, so a symlink planted in the workspace cannot redirect a read or
// write outside it (spec §28.7, SAN-001). The non-existing tail of a new path cannot be a symlink.
func (w *WorkspaceFS) assertNoSymlinkEscape(abs string) error {
	p := abs
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			within, rerr := filepath.Rel(w.root, real)
			if rerr != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
				return fmt.Errorf("%w: symlink target escapes", ErrPathEscape)
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("resolve path: %w", err)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return fmt.Errorf("%w: no in-workspace ancestor", ErrPathEscape)
		}
		p = parent
	}
}

// assertRegularOrAbsent denies a path that exists as a device, socket, or fifo — a workspace tool
// touches regular files and directories only (spec §28.7). An absent path is fine (a new write).
func assertRegularOrAbsent(abs string) error {
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat path: %w", err)
	}
	if info.IsDir() || info.Mode().IsRegular() {
		return nil
	}
	// A symlink here resolved to an in-root target (resolve already proved containment); a device,
	// socket, or fifo is refused.
	if info.Mode()&os.ModeSymlink != 0 {
		if resolved, err := os.Stat(abs); err == nil && (resolved.IsDir() || resolved.Mode().IsRegular()) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotRegular, filepath.Base(abs))
}

// Read returns the confined file's bytes, bounded to maxBytes, denying an escape or a non-regular
// target. A file larger than maxBytes is read up to the bound and reported truncated.
func (w *WorkspaceFS) Read(rel string, maxBytes int64) (data []byte, truncated bool, err error) {
	abs, err := w.resolve(rel)
	if err != nil {
		return nil, false, err
	}
	if err := assertRegularOrAbsent(abs); err != nil {
		return nil, false, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, false, fmt.Errorf("read %q: %w", rel, err)
	}
	defer f.Close()
	// Read one byte past the bound so an oversize file is detected and reported truncated.
	data, err = io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read %q: %w", rel, err)
	}
	if int64(len(data)) > maxBytes {
		return data[:maxBytes], true, nil
	}
	return data, false, nil
}

// Write atomically replaces the confined file with data (temp file + rename in the same directory),
// creating parent directories inside the root as needed, and reports the before/after content hash
// and whether it created the file (spec §28.7). A rename is atomic, so a concurrent reader never
// sees a partial write.
func (w *WorkspaceFS) Write(rel string, data []byte) (WriteReport, error) {
	abs, err := w.resolve(rel)
	if err != nil {
		return WriteReport{}, err
	}
	if err := assertRegularOrAbsent(abs); err != nil {
		return WriteReport{}, err
	}

	before := ""
	created := true
	if existing, err := os.ReadFile(abs); err == nil {
		before = hashBytes(existing)
		created = false
	} else if !os.IsNotExist(err) {
		return WriteReport{}, fmt.Errorf("read prior %q: %w", rel, err)
	}

	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return WriteReport{}, fmt.Errorf("prepare %q: %w", rel, err)
	}
	tmp, err := os.CreateTemp(dir, ".palai-write-*")
	if err != nil {
		return WriteReport{}, fmt.Errorf("stage %q: %w", rel, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return WriteReport{}, fmt.Errorf("stage %q: %w", rel, err)
	}
	if err := tmp.Close(); err != nil {
		return WriteReport{}, fmt.Errorf("stage %q: %w", rel, err)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return WriteReport{}, fmt.Errorf("commit %q: %w", rel, err)
	}
	return WriteReport{Path: filepath.ToSlash(rel), BeforeHash: before, AfterHash: hashBytes(data), Created: created}, nil
}

// Stat returns the confined metadata of a path.
func (w *WorkspaceFS) Stat(rel string) (Stat, error) {
	abs, err := w.resolve(rel)
	if err != nil {
		return Stat{}, err
	}
	if err := assertRegularOrAbsent(abs); err != nil {
		return Stat{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Stat{}, fmt.Errorf("stat %q: %w", rel, err)
	}
	return Stat{Path: filepath.ToSlash(rel), IsDir: info.IsDir(), Size: info.Size()}, nil
}

// List returns the confined directory listing at rel, sorted by name.
func (w *WorkspaceFS) List(rel string) ([]DirEntry, error) {
	abs, err := w.resolve(rel)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", rel, err)
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, DirEntry{Name: e.Name(), IsDir: e.IsDir(), Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Checksum returns the confined file's content hash.
func (w *WorkspaceFS) Checksum(rel string) (string, error) {
	abs, err := w.resolve(rel)
	if err != nil {
		return "", err
	}
	if err := assertRegularOrAbsent(abs); err != nil {
		return "", err
	}
	return checksumFile(abs)
}

// hashBytes is the digest-prefixed sha256 of in-memory content — the write-path counterpart of
// checksumFile, which hashes a file already on disk.
func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// shellMountTarget is the in-sandbox path the workspace is mounted at and the shell command's
// working directory — the same /workspace the engine sees (spec §29.9).
const shellMountTarget = "/workspace"

// ShellExecutor runs a workspace shell command inside a fresh, hardened OCI sandbox (spec §28.8,
// SAN-002/003/004). It implements toolbroker.ShellRunner. Each Run launches, waits on, and destroys
// one container from the pinned image: unprivileged uid 65532, no network (all egress denied),
// read-only rootfs, every capability dropped, no-new-privileges, cgroup memory/pids/cpu(/disk)
// bounds, and the workspace bind-mounted at /workspace as the working directory. Destroying the
// container is the process-group kill — no descendant survives it. The container mounts no runtime
// socket, so a command cannot reach the container runtime. Output is bounded and secret-redacted.
type ShellExecutor struct {
	driver         oci.Driver
	image          string
	limits         oci.Limits
	maxStdoutBytes int64
	maxStderrBytes int64
}

// NewShellExecutor binds a driver, the pinned command image, and the resource bounds a shell call
// runs under. The output bounds default to 1 MiB stdout / 64 KiB stderr.
func NewShellExecutor(driver oci.Driver, image string, limits oci.Limits) *ShellExecutor {
	return &ShellExecutor{driver: driver, image: image, limits: limits, maxStdoutBytes: 1 << 20, maxStderrBytes: 1 << 16}
}

// Run executes one argv command in the sandbox and returns its bounded, redacted result. A non-zero
// container exit is a normal shell outcome (the command failed), not an executor error; an error is
// returned only for an infrastructure failure. A memory OOM, wall-time expiry, or process-group
// destroy surfaces as a SIGKILL termination (exit 137), classified so the caller sees a bounded
// termination rather than a silent non-zero exit (spec §28.8, SAN-003).
func (e *ShellExecutor) Run(ctx context.Context, cmd toolbroker.ShellCommand) (toolbroker.ShellResult, error) {
	if len(cmd.Argv) == 0 {
		return toolbroker.ShellResult{}, errors.New("shell command requires an argv")
	}
	if cmd.WorkspaceRoot == "" {
		return toolbroker.ShellResult{}, errors.New("shell command requires a workspace root")
	}
	// Default argv-form runs the executable directly — no shell, no metacharacter interpretation.
	// Explicit shell mode joins the argv into a shell command line for pipelines/redirection (spec
	// §28.8); it requires a shell in the image, which the argv-form default does not.
	runArgv := cmd.Argv
	if cmd.Shell {
		runArgv = []string{"/bin/sh", "-c", strings.Join(cmd.Argv, " ")}
	}
	// The attempt's own environment values, layered on top of shellEnv() (E25 T3). Refused BEFORE the
	// container is created, so a colliding key never becomes a running process.
	env, err := toolbroker.LayerEnv(shellEnv(), nil, cmd.Env)
	if err != nil {
		return toolbroker.ShellResult{}, err
	}
	envValues := toolbroker.EnvValueList(cmd.Env)
	spec := oci.ContainerSpec{
		ImageDigest:    e.image,
		Env:            env,
		Labels:         map[string]string{"io.palai.sandbox": "shell"},
		Limits:         e.limits,
		MaxStdoutBytes: e.maxStdoutBytes,
		MaxStderrBytes: e.maxStderrBytes,
		Cmd:            runArgv,
		WorkingDir:     shellMountTarget,
		Mounts:         []oci.Mount{{Source: cmd.WorkspaceRoot, Target: shellMountTarget, ReadOnly: cmd.ReadOnly}},
	}

	start := time.Now()
	outcome, err := e.driver.Run(ctx, spec)
	result := toolbroker.ShellResult{
		ExitCode:   int(outcome.ExitCode),
		Stdout:     redactSecrets(string(outcome.Stdout), envValues),
		Stderr:     redactSecrets(string(outcome.Stderr), envValues),
		Truncated:  outcome.StdoutTruncated || outcome.StderrTruncated,
		TimedOut:   outcome.TimedOut,
		DurationMS: time.Since(start).Milliseconds(),
	}
	if outcome.TimedOut {
		result.Signal = "KILL"
	}
	// exit 137 = 128 + SIGKILL(9): a cgroup OOM kill or a wall-time/process-group destroy. Classify
	// it as a bounded termination; a mem-bounded kill is reported OOM. ponytail: exit-code heuristic
	// — the daemon's OOM flag is not surfaced by the driver, and a mem-bounded 137 is the reliable
	// OOM signal in practice.
	//
	// IT RUNS BEFORE THE TIMED-OUT CORRECTION BELOW, and the order is load-bearing: this heuristic
	// GUESSES an OOM from a 137 the daemon reported, and a wall-time expiry is a cause we already
	// KNOW. Correcting the exit code first would feed this branch a 137 we synthesised ourselves and
	// every timeout would claim an OOM that never happened.
	if result.ExitCode == 137 {
		result.Signal = "KILL"
		if e.limits.MaxMemoryBytes > 0 {
			result.OOMKilled = true
		}
	}
	// The driver assigns an exit code only on the branch where the container REPORTED one (docker.go's
	// `<-wait.Result`); a wall-time expiry abandons that wait, so Outcome.ExitCode is still its zero
	// value. Reporting that verbatim would tell the model a reaped build EXITED CLEANLY with no output,
	// and a model checks the exit code first. 137 is what this type's doc comment already promises and
	// what the host posture reports for the same event, so the two postures agree on one outcome. The
	// driver's Outcome stays a faithful record of what the daemon said; the correction is made here,
	// where the result becomes something a model reads.
	if outcome.TimedOut && result.ExitCode == 0 {
		result.ExitCode = 137
	}
	if err != nil {
		return result, fmt.Errorf("sandbox shell: %w", err)
	}
	return result, nil
}

// shellEnv is the minimal environment a shell command receives: no host inheritance and no runtime
// socket address. HOME points at the writable workspace so tools that scribble a dotfile do not fail on
// the read-only rootfs.
//
// "NO CREDENTIAL" STOOD IN THIS COMMENT UNTIL E25 T3 AND IS NOW HALF TRUE, SO IT IS WRITTEN OUT RATHER
// THAN LEFT TO ROT. Nothing is INHERITED — this function still returns a closed two-entry list built
// from constants, and the container still reaches nothing the control-plane process holds. But
// ShellCommand.Env is layered on top of it (toolbroker.LayerEnv) and an operator-authored environment
// may carry a credential value on purpose. Neither entry below can be shadowed by that layer: a
// colliding key is refused and the container is never created.
func shellEnv() []string {
	return []string{"HOME=" + shellMountTarget, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
}

// redactSecrets masks secret-shaped tokens AND this attempt's own environment values in captured shell
// output. Both functions live in the broker package beside ShellResult (packages/tool-broker/redact.go)
// so the host executor masks the SAME shapes and the SAME values: redaction is a property of the result,
// not of the container.
// HostSecretValues is the third source and the one that is NOT about this attempt: the control plane's
// own secret-named environment values. An agent cannot inherit them (the child environment is a
// three-name allow-list) but it can READ them — `ps -E -p <control-plane pid>` served 62 variables with
// their values on a live stack, and `os.Unsetenv` does not remove them because macOS answers `ps` from
// the kernel's copy of the initial environment. Masking the VALUE closes every path that carries it,
// including reading a credential file off disk, and catches the shapes secretPatterns cannot.
func redactSecrets(s string, envValues []string) string {
	s = toolbroker.RedactValues(toolbroker.RedactSecrets(s), envValues)
	s = toolbroker.RedactValues(s, toolbroker.HostSecretValues())
	// And the CONTENTS of the files those variables point at — the paths stay readable, the secrets do not.
	return toolbroker.RedactValues(s, toolbroker.HostSecretFileValues())
}
