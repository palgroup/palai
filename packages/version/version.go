// Package version is the single build-version stamp source shared by the control-plane, the runner,
// and the CLI. A release build injects the git-describe stamp with
// -ldflags "-X github.com/palgroup/palai/packages/version.Stamp=<v>" (scripts/release/build.sh);
// an unstamped `go build`/`go test`/`go run` binary falls back to the embedded VCS revision, then
// to "dev". The stamp is a build identifier, never a secret.
//
// It also owns the §48.2 runner support window the enroll/connect handshake enforces: a control-plane
// serves the current minor and the previous two, so an unsupported skew is rejected with the required
// intermediate-hop message (OPS-008).
package version

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// Stamp is the release version stamp injected at build time via -ldflags -X. Empty in a plain
// `go build`, so Resolve falls back to the embedded VCS revision.
var Stamp = ""

// Resolve returns this binary's version stamp, in precedence order: the PALAI_VERSION env override, the
// -ldflags Stamp, the build's embedded VCS revision (short, +"-dirty" for a modified tree), else "dev".
// It is the applied_by journal stamp, the runner's advertised version, and the control-plane's own
// version for the support-window check. PALAI_VERSION lets an operator pin the reported version — or a
// drill advertise a deliberately-old runner for the OPS-008 skew proof — without rebuilding the binary;
// it is a compatibility/build identifier (the runner is already mTLS-authenticated), never a secret.
func Resolve() string {
	if env := os.Getenv("PALAI_VERSION"); env != "" {
		return env
	}
	if Stamp != "" {
		return Stamp
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				rev = setting.Value
			case "vcs.modified":
				dirty = setting.Value == "true"
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			if dirty {
				return rev + "-dirty"
			}
			return rev
		}
	}
	return "dev"
}

// supportedMinorLookback is the §48.2 window depth: a control-plane serves its current minor and the
// previous this-many minors (current + previous two). A runner more than this many minors behind must
// hop through an intermediate release first.
const supportedMinorLookback = 2

// Supported reports whether a runner at runnerStamp may connect to a control-plane at controlPlaneStamp
// under the §48.2 support window, and — when it may not — the operator-facing rejection message naming
// the required intermediate hop (OPS-008). The window is enforced only between two RELEASE-stamped
// (parseable major.minor) builds: if either side is an unstamped dev/VCS build the versions cannot be
// compared, so the check is skipped (ok=true) — this keeps a from-source `palai local up`, where both
// binaries are unstamped, working unchanged. The check is fail-open ONLY for unparseable stamps; two
// parseable stamps outside the window are always rejected.
func Supported(controlPlaneStamp, runnerStamp string) (ok bool, message string) {
	cpMajor, cpMinor, cpOK := parse(controlPlaneStamp)
	rMajor, rMinor, rOK := parse(runnerStamp)
	if !cpOK || !rOK {
		return true, "" // one side is an unstamped dev/VCS build — not comparable, so not enforced
	}
	switch {
	case rMajor != cpMajor:
		// Front-load the actionable fact: the messages become a WebSocket close reason (<=123 bytes),
		// so the operator sees the incompatibility and the two versions before any truncation.
		return false, fmt.Sprintf(
			"runner %s major-incompatible with control-plane %s; a major upgrade needs the documented migration path",
			runnerStamp, controlPlaneStamp)
	case rMinor > cpMinor:
		return false, fmt.Sprintf(
			"runner %s newer than control-plane %s; upgrade the control-plane first",
			runnerStamp, controlPlaneStamp)
	case cpMinor-rMinor > supportedMinorLookback:
		// The oldest minor this control-plane still serves is the required intermediate hop: bring the
		// runner up to it before it may connect (it is then within the window of a newer control-plane).
		hop := cpMinor - supportedMinorLookback
		return false, fmt.Sprintf(
			"runner %s unsupported: hop to %d.%d.0 first, then to control-plane %s (window: current+prev %d minors)",
			runnerStamp, cpMajor, hop, controlPlaneStamp, supportedMinorLookback)
	default:
		return true, ""
	}
}

// parse extracts the semantic major.minor from a version stamp, tolerating a leading "v" and a
// git-describe suffix ("-<n>-g<hash>", "+<meta>", "-dirty"). It returns ok=false for a stamp that is
// not a dotted numeric version (a bare VCS hash, "dev", or ""), which the window treats as unstamped.
func parse(v string) (major, minor int, ok bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	// Drop the first git-describe / build-metadata separator: "0.15.0-3-gabc" / "0.15.0+meta" -> "0.15.0".
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	min, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return maj, min, true
}
