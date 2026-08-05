package macagent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"time"
)

// DefaultSocketPath is where palai-agentd binds, and it is HERE rather than in the daemon because both
// halves need it: the daemon binds it, and every caller — `palai up --native`, the runner's admission
// gate — dials it. Two writings of a path is two places for an install to land somewhere nothing looks.
const DefaultSocketPath = "/var/run/palai-agentd.sock"

// DefaultGroup is the group whose members may reach the daemon. Membership in it is the ENTIRE
// credential: no password, no token, no TTY. It is here for the same reason the path is.
const DefaultGroup = "palai"

// MaxResponseBytes caps one reply. It is larger than [MaxRequestBytes] because a reply is longer than a
// request — `ok list` with all 99 slots is a little over 300 bytes — and it exists at all because a
// client that read an unbounded line would let a wedged or wrong daemon spend its memory. The bound is
// on the CLIENT for the same reason the daemon has one on its side: neither end trusts the other's
// framing.
const MaxResponseBytes = 8192

// ExchangeTimeout bounds ONE request/response round trip once a connection is open. It is separate from
// the cold-boot window below, and the difference matters: a daemon that ACCEPTS and then says nothing is
// not a daemon that is still starting, it is a daemon that is wedged, and waiting out a 40-second boot
// window for it would turn a hang into a longer hang.
const ExchangeTimeout = 5 * time.Second

// DialFunc opens one connection to a daemon. It is a function rather than a path so a test can make the
// socket behave the way a cold boot does without waiting for one, and so nothing here has to guess how
// a caller wants its connection made.
type DialFunc func(ctx context.Context) (net.Conn, error)

// UnixDialer dials the daemon's unix socket.
func UnixDialer(path string) DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
}

// Ask sends one request and reads one reply.
//
// ‼️ A CLASSED REFUSAL IS AN ANSWER, NOT AN ERROR, and the split is the whole reason this returns two
// values. `err refused ...` means a daemon is there, running, and declining — which is information a
// caller acts on. A non-nil error means the daemon could not be REACHED or did not speak the protocol,
// which is the only condition a retry could ever fix. This tree has already paid for collapsing those
// two: every tool Exec error wedged a run forever because a refusal that was an answer was being
// handled as a transport failure.
func Ask(ctx context.Context, dial DialFunc, req Request) (Response, error) {
	line, err := req.Line()
	if err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, ExchangeTimeout)
	defer cancel()

	conn, err := dial(ctx)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := io.WriteString(conn, line); err != nil {
		return Response{}, fmt.Errorf("writing %q to the daemon: %w", req.Verb, err)
	}
	// Bounded before it is parsed, MaxResponseBytes+1 so an over-long reply is DETECTED rather than
	// silently truncated into a shorter one that happens to parse.
	reply, err := bufio.NewReader(io.LimitReader(conn, MaxResponseBytes+1)).ReadString('\n')
	if err != nil && (reply == "" || !errors.Is(err, io.EOF)) {
		return Response{}, fmt.Errorf("reading the daemon's reply to %q: %w", req.Verb, err)
	}
	if len(reply) > MaxResponseBytes {
		return Response{}, fmt.Errorf("the daemon's reply to %q is over %d bytes", req.Verb, MaxResponseBytes)
	}
	return ParseResponse(reply)
}

// ProbePolicy bounds how long a caller keeps asking a socket that is not answering yet.
//
// IT IS A POLICY WITH NAMED FIELDS RATHER THAN THREE LITERALS AT THE CALL SITE, so that a test drives
// the SHIPPED numbers instead of numbers a test author chose. A retry test written against its own
// constants proves that retrying works somewhere; it does not prove the machine waits long enough for
// the boot it exists to survive.
type ProbePolicy struct {
	// Attempts is how many times the socket is dialled, INCLUDING the first. One means no retry at all,
	// which is the shape this whole policy exists to not be.
	Attempts int
	// FirstBackoff is the wait after the first refusal, and it doubles up to MaxBackoff. Exponential
	// rather than flat because the common case is a socket that appears almost immediately (the daemon
	// is already up and this is a plain probe) and the expensive case is a machine that has just been
	// powered on.
	FirstBackoff time.Duration
	// MaxBackoff caps the doubling, so the window grows with attempts rather than exploding.
	MaxBackoff time.Duration
}

// Window is the longest a probe under this policy can wait, and it is COMPUTED from the fields rather
// than written beside them. A second literal saying "about forty seconds" would be a number that drifts
// the moment anyone tunes an attempt — and this value is quoted to an operator in the refusal, where a
// wrong one sends them looking at the wrong thing.
func (p ProbePolicy) Window() time.Duration {
	total := time.Duration(0)
	for i := 1; i < p.Attempts; i++ {
		total += p.backoff(i)
	}
	return total
}

// backoff is the wait after the nth failed attempt, n starting at 1.
func (p ProbePolicy) backoff(n int) time.Duration {
	d := p.FirstBackoff
	for i := 1; i < n && d < p.MaxBackoff; i++ {
		d *= 2
	}
	if p.MaxBackoff > 0 && d > p.MaxBackoff {
		return p.MaxBackoff
	}
	return d
}

// ColdBootProbe is the policy the ZERO-TOUCH PATH probes with, and it is the reason this type exists.
//
// ‼️ THIS IS THE PRICE OF THE cgo DECISION, PAID HERE. Handing a listener to a job on demand needs
// `launch_activate_socket`, a C function in libSystem with no wrapper in x/sys, and this tree has zero
// cgo — every release build is CGO_ENABLED=0 and CI is ubuntu-24.04, so a cgo daemon would be a daemon
// CI never compiles. So the plist is RunAtLoad+KeepAlive and the daemon binds its own socket, which
// means the socket does not exist until the daemon has bound it and a caller racing a cold boot gets
// ECONNREFUSED rather than a queued connection.
//
// AND THAT WINDOW OPENS EXACTLY ON A FRESHLY BOOTED CLOUD MAC — the zero-touch path itself, the one
// nobody is watching. A single-shot probe there reads "no daemon" and keeps a sound machine out of the
// pool: fail-closed is the right direction, refused for being EARLY is the wrong cause, and a hundred
// machines make the difference operational rather than theoretical.
var ColdBootProbe = ProbePolicy{
	Attempts:     12,
	FirstBackoff: 250 * time.Millisecond,
	MaxBackoff:   5 * time.Second,
}

// Health is what a probe MEASURED, and every field of it came off the socket. It is deliberately not a
// boolean: "the daemon is installed" is a claim, and this is the answer to a question that was asked.
type Health struct {
	// Version is the stamp the RUNNING daemon reported, empty when it was too old to have the verb.
	Version string
	// Slots is what it listed, which is also the proof that the round trip completed rather than merely
	// connecting.
	Slots []int
	// Attempts is how many dials it took, and Waited how long was spent between them. They are here so a
	// caller can SAY it lost the cold-boot race and recovered, rather than reporting a silent success
	// indistinguishable from a machine that was already up.
	Attempts int
	Waited   time.Duration
}

// Prober asks a daemon whether it is there, and keeps asking while the answer is "not yet".
type Prober struct {
	// SocketPath is only ever used for the message; the connection comes from Dial.
	SocketPath string
	Policy     ProbePolicy
	// Dial is how a connection is made. Replaced in tests, never in production.
	Dial DialFunc
	// Sleep is time.Sleep in production. It is a field so a test can drive the SHIPPED policy without
	// spending its window — the alternative is a test that invents a faster policy, which measures a
	// number no machine ever runs under.
	Sleep func(time.Duration)
}

// NewProber builds the probe every production caller uses: the default socket, the cold-boot policy,
// and a real clock.
func NewProber(socketPath string) *Prober {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	return &Prober{
		SocketPath: socketPath,
		Policy:     ColdBootProbe,
		Dial:       UnixDialer(socketPath),
		Sleep:      time.Sleep,
	}
}

// ErrNotAnswering is what a probe returns when the window ran out with the socket still refusing. It is
// a sentinel so a caller can tell "no daemon here" from "a daemon that said no", which are two
// different next steps: install one, versus stop and read what it said.
var ErrNotAnswering = errors.New("palai-agentd is not answering")

// Probe dials the daemon and asks it something, retrying while the socket is not there YET.
//
// ‼️ IT ASKS. A probe that connected and closed would prove a socket exists, which is the claim, not the
// measurement — this tree's whole reason for existing this week is an install that was ASSUMED done and
// never connected to. So the round trip is `list`, which exercises the parser, the dispatcher and the
// privileged surface's read path, and its answer is carried back on [Health].
//
// ‼️ AND THE VERSION IT REPORTS COMES OFF THE SOCKET, NEVER OFF THE DISK, because the two DO diverge and
// the divergence is the interesting case: an upgrade that copied a new binary to
// [InstalledBinaryPath] and then failed to kickstart the job leaves a NEW file and an OLD daemon still
// serving. The file answers "we upgraded"; the socket answers what is actually deciding whether an
// account gets created. The socket is the authority, and a caller comparing versions must compare this
// one. A daemon too old to know the verb leaves Version empty, which reads as "older than the first
// build that could say", and that is the correct reading.
//
// ONLY THE COLD-BOOT SHAPES ARE RETRIED. ECONNREFUSED (nothing bound yet) and ENOENT (the node itself
// has not been created yet) are what a machine that is still booting looks like. EACCES is NOT retried:
// it means this process is not in the `palai` group, waiting forty seconds cannot change that, and a
// window spent on it is a window spent lying to an operator whose fix is one dseditgroup line. Anything
// that is not a dial failure at all — a reply that does not parse — is a different daemon, not an early
// one, and it is returned immediately.
func (p *Prober) Probe(ctx context.Context) (Health, error) {
	sleep := p.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	attempts := p.Policy.Attempts
	if attempts < 1 {
		attempts = 1
	}
	health := Health{}
	var last error
	for n := 1; n <= attempts; n++ {
		health.Attempts = n
		resp, err := Ask(ctx, p.Dial, Request{Verb: VerbList})
		switch {
		case err == nil && resp.OK:
			health.Slots = resp.Slots
			health.Version = p.version(ctx)
			return health, nil
		case err == nil:
			// A CLASSED REFUSAL ENDS THE PROBE. The daemon is there and declining, so there is nothing
			// for a retry to wait for — and `unsupported` (this is not a Mac) would otherwise be waited
			// out for the whole window before saying the one thing that was true immediately.
			return health, Errorf(resp.Class, "palai-agentd at %s refused a list: %s", p.SocketPath, resp.Message)
		case !isStillBooting(err):
			return health, fmt.Errorf("palai-agentd at %s: %w", p.SocketPath, err)
		default:
			last = err
		}
		if n < attempts {
			wait := p.Policy.backoff(n)
			health.Waited += wait
			sleep(wait)
		}
	}
	return health, fmt.Errorf("%w at %s after %d attempts over %s: %v — fix: install it (`palai up --native` does, "+
		"with root or passwordless sudo), or if it is installed read /var/log/palai-agentd.log for why it is refusing to serve",
		ErrNotAnswering, p.SocketPath, health.Attempts, p.Policy.Window(), last)
}

// version asks the RUNNING daemon what it is. A daemon too old to know the verb answers
// `err unknown_verb`, and an empty stamp is the honest reading of that: older than the first build that
// could say. It is never an error — a machine that lists is a machine that works, and refusing it for
// being unable to name itself would fail closed on the wrong fact.
func (p *Prober) version(ctx context.Context) string {
	resp, err := Ask(ctx, p.Dial, Request{Verb: VerbVersion})
	if err != nil || !resp.OK {
		return ""
	}
	return resp.Version
}

// isStillBooting reports whether a dial failure is one a machine that has just been powered on produces.
//
// It unwraps to the syscall errno rather than matching on the message, because a *net.OpError's text is
// prose and this tree has already had a caller branch on error text and lose three arms when the wording
// improved.
func isStillBooting(err error) bool {
	// ErrNotExist covers ENOENT however it arrives: the socket node is created by the bind, so before
	// the daemon has bound there is nothing at the path at all.
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	var errno interface{ Timeout() bool }
	if errors.As(err, &errno) && errno.Timeout() {
		// A dial that TIMED OUT reached something that never completed the handshake. That is not the
		// cold-boot shape — it is a machine under load or a wedged listener — but it is transient in the
		// same way, and the window is bounded either way.
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}
