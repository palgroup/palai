package workspace

import (
	"errors"
	iofs "io/fs"
)

// A WORKSPACE FAILURE HAS TO SURVIVE A WIRE, AND WHAT MUST SURVIVE IS THE SENTINEL, NOT THE TEXT
// (A.3 T5).
//
// The file tool classifies every failure by errors.Is against ErrPathEscape, ErrNotRegular,
// fs.ErrNotExist and fs.ErrPermission, and that classification decides what the model is told:
// a refused traversal comes back as `refused` — the control fired, /etc/passwd was not read — while
// an unrecognised cause comes back as `failed`, which reads as a bug in the platform. Once the
// filesystem is on another machine the error is rebuilt from JSON on this side, and a rebuilt
// error carries no sentinel at all: EVERY refusal would silently become `failed`. The security
// control would still work and would stop being legible, which is the more expensive half.
//
// So the machine names the cause with FailureCode and this side rebuilds an error that answers
// errors.Is the same way. Both ends call this file, so there is one table rather than two.

// ErrRootUnusable is returned when the ALLOCATION ITSELF could not be bound — a blank root, or one
// EvalSymlinks cannot resolve because it was never provisioned or has gone away underneath a live
// run. It is a distinct sentinel because the file tool draws its one asymmetry across exactly this
// line, and the line is a decision on the record rather than an accident of where a constructor sits:
// a root that is absent is THE DEPLOYMENT being broken and takes the fault path, while a root that
// exists and is merely unusable fails inside the operation and is an ANSWER, because the read
// touched nothing. TestAnAllocationRootThatDoesNotExistIsRefusedBeforeAnyRead pins both halves.
//
// Without it the asymmetry does not survive a wire: EvalSymlinks on an absent root fails with ENOENT,
// which errors.Is answers as fs.ErrNotExist, so a rebuilt error would classify a never-provisioned
// allocation as `not_found` — a missing file — and the run would carry on asking a machine that has
// no workspace at all.
var ErrRootUnusable = errors.New("workspace allocation root cannot be used")

// Wire codes for a workspace failure. They are lower_snake and stable: they travel in a durable
// message and a renamed code is a code the other end does not recognise.
const (
	// FailureCodeRootUnusable is checked FIRST in FailureCode, and the order is load-bearing: an
	// absent root's cause chain also satisfies fs.ErrNotExist, so a later arm would win and rename
	// a broken deployment into a missing file.
	FailureCodeRootUnusable = "root_unusable"
	FailureCodePathEscape   = "path_escape"
	FailureCodeNotRegular   = "not_regular"
	FailureCodeNotExist     = "not_exist"
	FailureCodePermission   = "permission"
	// FailureCodeOther is every cause with no sentinel of its own. It is deliberately NOT a guess:
	// the message still crosses, and the file tool maps it to `failed`, which is what an
	// unrecognised local cause already produced.
	FailureCodeOther = "other"
)

// FailureCode names the cause of a workspace failure for the wire. It reads sentinels through
// errors.Is and never message text — the messages are built by fmt.Errorf at several call sites and
// would be several things to keep in step.
func FailureCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrRootUnusable):
		return FailureCodeRootUnusable
	case errors.Is(err, ErrPathEscape):
		return FailureCodePathEscape
	case errors.Is(err, ErrNotRegular):
		return FailureCodeNotRegular
	case errors.Is(err, iofs.ErrNotExist):
		return FailureCodeNotExist
	case errors.Is(err, iofs.ErrPermission):
		return FailureCodePermission
	default:
		return FailureCodeOther
	}
}

// ErrorForCode rebuilds a failure from the wire so errors.Is answers what it answered on the machine.
// The message is carried verbatim — it is the machine's own, already free of anything but the
// workspace-relative request for the paths that matter (resolve's refusals name only `rel`).
//
// AN UNKNOWN CODE IS `other`, NOT A PANIC AND NOT A SENTINEL. A machine running a newer build may
// name a cause this one has never heard of, and inventing a sentinel for it would classify a
// failure by guessing.
func ErrorForCode(code, message string) error {
	if code == "" && message == "" {
		return nil
	}
	var sentinel error
	switch code {
	case FailureCodeRootUnusable:
		sentinel = ErrRootUnusable
	case FailureCodePathEscape:
		sentinel = ErrPathEscape
	case FailureCodeNotRegular:
		sentinel = ErrNotRegular
	case FailureCodeNotExist:
		sentinel = iofs.ErrNotExist
	case FailureCodePermission:
		sentinel = iofs.ErrPermission
	}
	if message == "" {
		message = "workspace operation failed (" + code + ")"
	}
	return &remoteFailure{message: message, sentinel: sentinel}
}

// remoteFailure is a failure that happened somewhere else: its Error is the machine's own sentence
// and its Unwrap is the sentinel that sentence was classified by. A nil sentinel (code `other`)
// unwraps to nothing, so errors.Is finds no match — exactly as an unrecognised local cause does.
type remoteFailure struct {
	message  string
	sentinel error
}

func (e *remoteFailure) Error() string { return e.message }

func (e *remoteFailure) Unwrap() error { return e.sentinel }
