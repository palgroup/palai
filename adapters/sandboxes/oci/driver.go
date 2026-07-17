// Package oci runs a single untrusted engine attempt inside a hardened, digest-pinned
// OCI container and returns its raw, separately bounded stdout and stderr. It owns the
// sandbox mechanics only — image-digest verification, container hardening, wall-time
// enforcement, and destruction — never the engine protocol, which the runner
// supervisor layers on top. The Driver seam keeps that boundary explicit so a future
// driver (a remote or Kubernetes sandbox) can satisfy the same contract; the Docker
// implementation lives in docker.go.
package oci

import (
	"context"
	"errors"
	"regexp"
	"time"
)

// ErrMutableImage reports an image reference that is not pinned to an immutable
// sha256 digest, or a daemon that resolved the digest to a different image.
var ErrMutableImage = errors.New("engine image must be pinned to an immutable sha256 digest")

// immutableDigest matches a canonical, lowercase sha256 content digest — the only
// engine image reference the driver will run. Mutable tags are rejected.
var immutableDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Limits are the sandbox resource bounds the driver enforces on the container.
type Limits struct {
	WallTime        time.Duration
	MaxMemoryBytes  int64
	MaxProcessCount int64
	NanoCPUs        int64
}

// ContainerSpec is one engine attempt: the pinned image, the exact environment the
// container receives (the driver adds nothing — no host inheritance), labels for
// leak accounting, and the resource and per-stream output bounds.
type ContainerSpec struct {
	ImageDigest    string
	Env            []string
	Labels         map[string]string
	Limits         Limits
	MaxStdoutBytes int64
	MaxStderrBytes int64
}

// Outcome is the raw result of one sandboxed attempt. Stdout and Stderr are captured
// into independent bounded buffers; TimedOut reports that the wall-time bound was hit
// and the container was force-killed. The driver has already destroyed the container
// by the time it returns.
type Outcome struct {
	ContainerID     string
	ImageID         string
	ExitCode        int64
	Stdout          []byte
	StdoutBytes     int64
	StdoutTruncated bool
	Stderr          []byte
	StderrBytes     int64
	StderrTruncated bool
	TimedOut        bool
}

// Driver runs one engine attempt in an isolated sandbox and destroys it. Run always
// leaves no container behind, even on error; the returned ContainerID is for leak
// verification, not a live handle.
type Driver interface {
	Run(ctx context.Context, spec ContainerSpec) (Outcome, error)
	Close() error
}

// validate enforces the digest and bound invariants before any container is created,
// so a mutable reference or a missing bound never reaches the daemon.
func (spec ContainerSpec) validate() error {
	if !immutableDigest.MatchString(spec.ImageDigest) {
		return ErrMutableImage
	}
	if spec.MaxStdoutBytes <= 0 || spec.MaxStderrBytes <= 0 {
		return errors.New("stdout and stderr bounds must be positive")
	}
	if spec.Limits.WallTime <= 0 || spec.Limits.MaxMemoryBytes <= 0 ||
		spec.Limits.MaxProcessCount <= 0 || spec.Limits.NanoCPUs <= 0 {
		return errors.New("all sandbox resource bounds must be positive")
	}
	return nil
}

// boundedBuffer captures up to limit bytes and records whether more were produced. It
// bounds memory for an untrusted stream while still reporting the true byte total.
type boundedBuffer struct {
	data      []byte
	limit     int64
	total     int64
	truncated bool
}

func newBoundedBuffer(limit int64) *boundedBuffer { return &boundedBuffer{limit: limit} }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.total += int64(len(p))
	if remaining := b.limit - int64(len(b.data)); remaining > 0 {
		take := int64(len(p))
		if take > remaining {
			take = remaining
		}
		b.data = append(b.data, p[:take]...)
	}
	if b.total > b.limit {
		b.truncated = true
	}
	return len(p), nil
}
